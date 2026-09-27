// server bridges browser WebSocket clients to Redis Streams. It exists
// only because browsers can't speak Redis's wire protocol directly — it
// still talks to Redis exactly like any other consumer (plain RESP/TCP via
// go-redis); the WebSocket leg is purely the browser-facing side of that
// bridge, not a coordination layer between clients.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"

	"streaming/internal/ingest"
	"streaming/internal/positionlog"
	"streaming/internal/redisstream"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // LAN demo: no CORS/auth concerns
}

// controlMsg is everything a client can send: a request to change where
// in the stream it's reading from (initial join, or a seek), or a
// heartbeat reporting actual playback position for the viewer dashboard.
//
// Fields are float64, not int64: video.currentTime is a float in every
// browser, and a client-side bug once sent an unrounded value (e.g.
// 14513.607) straight through — with an int64 field that failed to
// unmarshal, silently dropping the *entire* message (seek and position
// alike). Accepting a float and truncating here means a stray fractional
// millisecond can never take down an otherwise-valid message again.
type controlMsg struct {
	SeekMillis     *float64 `json:"seek_ms"`
	PositionMillis *float64 `json:"position_ms"`
}

// safeConn serializes writes: when a seek arrives mid-stream, the old and
// new stream goroutines briefly overlap before the old one notices its
// context was cancelled, and gorilla/websocket forbids concurrent writers.
type safeConn struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (c *safeConn) WriteBinary(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

func main() {
	addr := flag.String("addr", ":8080", "http listen address")
	redisAddr := flag.String("redis", "localhost:6379", "redis address")
	kafkaAddr := flag.String("kafka", "localhost:9092", "kafka broker address")
	flag.Parse()

	store := redisstream.NewStore(*redisAddr)
	defer store.Close()

	if merged, err := store.ReconcileRooms(context.Background()); err != nil {
		log.Printf("reconcile rooms: %v", err)
	} else if merged > 0 {
		log.Printf("merged %d duplicate room(s) into their oldest same-named room", merged)
	}

	positions := positionlog.NewProducer([]string{*kafkaAddr})
	live := newHub(store)
	defer positions.Close()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWS(w, r, store, positions, live)
	})
	http.HandleFunc("/api/host/start", func(w http.ResponseWriter, r *http.Request) {
		handleHostStart(w, r, store)
	})
	http.HandleFunc("/api/room/create", func(w http.ResponseWriter, r *http.Request) {
		handleCreateRoom(w, r, store)
	})
	http.HandleFunc("/api/rooms", func(w http.ResponseWriter, r *http.Request) {
		handleListRooms(w, r, store)
	})
	http.HandleFunc("/api/room", func(w http.ResponseWriter, r *http.Request) {
		handleGetRoom(w, r, store)
	})
	http.HandleFunc("/api/movies", func(w http.ResponseWriter, r *http.Request) {
		handleListMovies(w, r, store)
	})
	http.HandleFunc("/api/movie", func(w http.ResponseWriter, r *http.Request) {
		handleGetMovie(w, r, store)
	})
	http.HandleFunc("/api/resume", func(w http.ResponseWriter, r *http.Request) {
		handleGetResume(w, r, store)
	})
	http.HandleFunc("/api/continue-watching", func(w http.ResponseWriter, r *http.Request) {
		handleContinueWatching(w, r, store)
	})
	http.HandleFunc("/api/movie/delete", func(w http.ResponseWriter, r *http.Request) {
		handleDeleteMovie(w, r, store)
	})
	http.HandleFunc("/api/room/delete", func(w http.ResponseWriter, r *http.Request) {
		handleDeleteRoom(w, r, store)
	})
	http.HandleFunc("/api/movie/poster", handlePoster)
	http.HandleFunc("/api/server-info", func(w http.ResponseWriter, r *http.Request) {
		handleServerInfo(w, *addr)
	})
	http.Handle("/", http.FileServer(http.Dir("web")))

	log.Printf("listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func handleWS(w http.ResponseWriter, r *http.Request, store *redisstream.Store, positions *positionlog.Producer, live *hub) {
	movieID := r.URL.Query().Get("movie")
	if movieID == "" {
		http.Error(w, "movie query param required", http.StatusBadRequest)
		return
	}
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		clientID = randomID()
	}

	// from_ms present (even "0") means the client is asking for a specific
	// point explicitly. Absent means "resume where this client left off",
	// resolved from the last position it reported, defaulting to the start.
	var startMillis int64
	if v := r.URL.Query().Get("from_ms"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			startMillis = parsed
		}
	} else if resume, ok, err := store.GetResumePoint(r.Context(), movieID, clientID); err == nil && ok {
		startMillis = resume
	}

	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}
	defer wsConn.Close()
	conn := &safeConn{conn: wsConn}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The MP4 init segment (codec config) must reach the client's
	// SourceBuffer exactly once, before any media segment — never
	// re-sent on a later seek within the same connection.
	//
	// A viewer can join within milliseconds of the host clicking "go
	// live," before ffmpeg has produced the init segment at all. Failing
	// immediately in that window silently kills the connection with no
	// retry on the client side — wait briefly for it to appear instead
	// of treating "not ready yet" the same as "movie doesn't exist."
	initData, err := waitForInit(ctx, store, movieID, 15*time.Second)
	if err != nil {
		log.Printf("get init movie=%s: %v", movieID, err)
		return
	}
	if err := conn.WriteBinary(initData); err != nil {
		return
	}

	seekCh := make(chan int64, 1)
	seekCh <- startMillis // treat the initial join as a "seek" to from_ms

	go readControlMessages(wsConn, seekCh, cancel, store, positions, clientID, movieID)

	var streamCancel context.CancelFunc
	for {
		select {
		case <-ctx.Done():
			if streamCancel != nil {
				streamCancel()
			}
			return
		case from := <-seekCh:
			if streamCancel != nil {
				streamCancel()
			}
			var streamCtx context.Context
			streamCtx, streamCancel = context.WithCancel(ctx)
			go streamFrom(streamCtx, conn, store, live, movieID, from)
		}
	}
}

func readControlMessages(conn *websocket.Conn, seekCh chan int64, cancel context.CancelFunc, store *redisstream.Store, positions *positionlog.Producer, clientID, movieID string) {
	defer cancel()

	// Position pings are written by a single dedicated goroutine, in the
	// exact order they arrive. An earlier version fired an independent
	// goroutine per ping — under normal network jitter, two pings' Redis
	// writes could complete out of order, letting an older, smaller
	// position clobber a newer one and corrupt the resume point (caught
	// via the analytics work: a viewer's resume point read back as 0
	// after several seconds of real playback). The channel is buffered
	// so a burst of pings still never blocks the read loop below.
	positionCh := make(chan int64, 16)
	go func() {
		for pos := range positionCh {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := positions.Publish(ctx, positionlog.Event{
				ClientID:        clientID,
				MovieID:         movieID,
				PositionMillis:  pos,
				EventTimeUnixMs: time.Now().UnixMilli(),
			}); err != nil {
				log.Printf("publish position client=%s: %v", clientID, err)
			}
			if err := store.SaveResumePoint(ctx, movieID, clientID, pos); err != nil {
				log.Printf("save resume point client=%s: %v", clientID, err)
			}
			if err := store.RecordClientMovie(ctx, clientID, movieID); err != nil {
				log.Printf("record client movie client=%s: %v", clientID, err)
			}
			cancel()
		}
	}()
	defer close(positionCh)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return // client disconnected
		}
		var msg controlMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("bad control message: %v", err)
			continue
		}
		if msg.SeekMillis != nil {
			seek := int64(*msg.SeekMillis)
			select {
			case seekCh <- seek:
			default:
				// a seek is already queued; drop, the newer one below will win
				<-seekCh
				seekCh <- seek
			}
		}
		if msg.PositionMillis != nil {
			select {
			case positionCh <- int64(*msg.PositionMillis):
			default:
				// writer goroutine is stalled behind a slow Kafka/Redis
				// call; drop this ping rather than block reading further
				// control messages (a seek must never wait behind this).
				log.Printf("position ping dropped (writer busy) client=%s", clientID)
			}
		}
	}
}

// waitForInit polls for a movie's init segment, retrying while GetInit
// returns "not found" (redis.Nil) since that's expected right up until
// ingest.Run finishes its first ffmpeg-startup step. Any other error is
// returned immediately.
func waitForInit(ctx context.Context, store *redisstream.Store, movieID string, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		data, err := store.GetInit(ctx, movieID)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, redis.Nil) || time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-time.After(150 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// randomMovieID is shorter than randomID: it's a room code a host reads
// aloud or pastes into a link, not an internal client identifier.
func randomMovieID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func handleCreateRoom(w http.ResponseWriter, r *http.Request, store *redisstream.Store) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = r.FormValue("name")
	}
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	// Same short-code style as movie IDs; a room code is just as shareable.
	// If the name is already taken, the existing room comes back instead.
	room, created, err := store.CreateOrGetRoom(r.Context(), randomMovieID(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"room_id": room.ID,
		"name":    room.Name,
		"created": created,
	})
}

type roomSummary struct {
	redisstream.RoomMeta
	StreamCount   int    `json:"stream_count"`
	LiveCount     int    `json:"live_count"`
	PosterMovieID string `json:"poster_movie_id,omitempty"`
}

func handleListRooms(w http.ResponseWriter, r *http.Request, store *redisstream.Store) {
	rooms, err := store.ListRooms(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	movies, err := store.ListMovies(r.Context()) // newest first
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	byRoom := map[string]*roomSummary{}
	out := make([]*roomSummary, 0, len(rooms))
	for _, room := range rooms {
		s := &roomSummary{RoomMeta: room}
		byRoom[room.ID] = s
		out = append(out, s)
	}
	for _, m := range movies {
		s, ok := byRoom[m.RoomID]
		if !ok {
			continue
		}
		s.StreamCount++
		if m.Status != "ended" {
			s.LiveCount++
		}
		if s.PosterMovieID == "" && posterExists(m.ID) {
			s.PosterMovieID = m.ID
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func handleDeleteRoom(w http.ResponseWriter, r *http.Request, store *redisstream.Store) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "POST or DELETE required", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id query param required", http.StatusBadRequest)
		return
	}
	movies, _ := store.ListMoviesByRoom(r.Context(), id)
	switch err := store.DeleteRoom(r.Context(), id); {
	case err == nil:
		for _, m := range movies {
			removeUpload(m.ID)
		}
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, redisstream.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, redisstream.ErrStillStreaming):
		http.Error(w, "a stream in this room is still live; wait for it to end", http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleServerInfo reports the URL other devices on the network should
// use. A host who opened the dashboard as "localhost" would otherwise
// share links that only resolve on their own machine.
func handleServerInfo(w http.ResponseWriter, listenAddr string) {
	_, port, _ := net.SplitHostPort(listenAddr)
	info := map[string]string{}
	if ip := lanIP(); ip != "" && port != "" {
		info["lan_url"] = "http://" + net.JoinHostPort(ip, port)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

// lanIP returns the source address the OS would use for its default
// route — the adapter actually connected to the router, as opposed to
// VPN/VM/WSL virtual adapters. Dialing UDP sends no packets; it only
// runs the route lookup. 192.0.2.1 is TEST-NET-1, never really routed.
func lanIP() string {
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// Posters live next to the uploaded file. Movie IDs are server-generated
// 6-char hex, so anything else is rejected before touching the
// filesystem — the ID ends up in a path.
var movieIDPattern = regexp.MustCompile(`^[0-9a-f]{6}$`)

func posterPath(movieID string) string {
	return filepath.Join("uploads", movieID, "poster.jpg")
}

func posterExists(movieID string) bool {
	if !movieIDPattern.MatchString(movieID) {
		return false
	}
	_, err := os.Stat(posterPath(movieID))
	return err == nil
}

func handlePoster(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !posterExists(id) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, posterPath(id))
}

// generatePoster grabs one frame a couple of seconds in (skipping black
// lead-in frames at 0) as the card artwork. Best effort: a missing
// poster just falls back to a placeholder in the UI.
func generatePoster(inputPath, movieID string) {
	for _, at := range []string{"3", "0"} { // very short clips may not reach 3s
		cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-ss", at, "-i", inputPath,
			"-frames:v", "1", "-vf", "scale=640:-2", "-q:v", "4", posterPath(movieID))
		if err := cmd.Run(); err == nil && posterExists(movieID) {
			return
		}
	}
	log.Printf("poster movie=%s: could not extract a frame", movieID)
}

func handleGetRoom(w http.ResponseWriter, r *http.Request, store *redisstream.Store) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id query param required", http.StatusBadRequest)
		return
	}
	meta, ok, err := store.GetRoomMeta(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meta)
}

// handleHostStart accepts a browser file upload, registers a new movie
// within a room, and starts encoding+publishing it in the background. It
// streams the upload straight to disk (MultipartReader, not
// ParseMultipartForm) so a multi-gigabyte movie file is never buffered
// in memory.
func handleHostStart(w http.ResponseWriter, r *http.Request, store *redisstream.Store) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		http.Error(w, "room query param required", http.StatusBadRequest)
		return
	}
	if _, ok, err := store.GetRoomMeta(r.Context(), roomID); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	} else if !ok {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart/form-data", http.StatusBadRequest)
		return
	}

	movieID := randomMovieID()
	title := movieID
	var savedPath string

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "bad multipart data", http.StatusBadRequest)
			return
		}
		switch part.FormName() {
		case "title":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			if t := string(b); t != "" {
				title = t
			}
		case "video":
			dir := filepath.Join("uploads", movieID)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}
			filename := part.FileName()
			if filename == "" {
				filename = "video"
			}
			savedPath = filepath.Join(dir, filepath.Base(filename))
			out, err := os.Create(savedPath)
			if err != nil {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}
			_, copyErr := io.Copy(out, part)
			out.Close()
			if copyErr != nil {
				http.Error(w, "upload failed", http.StatusInternalServerError)
				return
			}
		}
		part.Close()
	}

	if savedPath == "" {
		http.Error(w, "no video file provided (expected a \"video\" form field)", http.StatusBadRequest)
		return
	}

	// Register synchronously so the movie is listable the instant this
	// handler returns, rather than racing the background goroutine below.
	if err := store.RegisterMovie(r.Context(), movieID, roomID, title); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	go generatePoster(savedPath, movieID)
	go func() {
		ctx := context.Background()
		if err := ingest.Run(ctx, store, ingest.Options{
			MovieID:    movieID,
			RoomID:     roomID,
			Title:      title,
			InputPath:  savedPath,
			GOPSeconds: 2,
			Realtime:   true,
		}); err != nil {
			log.Printf("ingest movie=%s: %v", movieID, err)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"movie_id": movieID, "title": title})
}

func handleListMovies(w http.ResponseWriter, r *http.Request, store *redisstream.Store) {
	var movies []redisstream.MovieMeta
	var err error
	if roomID := r.URL.Query().Get("room"); roomID != "" {
		movies, err = store.ListMoviesByRoom(r.Context(), roomID)
	} else {
		movies, err = store.ListMovies(r.Context())
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(movies)
}

// handleGetResume lets the client resolve its own resume point before
// connecting, so it always knows the exact offset it's starting at and
// can align video.currentTime to it — the server-side fallback in
// handleWS (used when from_ms is omitted) can't communicate that value
// back to a browser client after the fact.
func handleGetResume(w http.ResponseWriter, r *http.Request, store *redisstream.Store) {
	movieID := r.URL.Query().Get("movie")
	clientID := r.URL.Query().Get("client_id")
	if movieID == "" || clientID == "" {
		http.Error(w, "movie and client_id query params required", http.StatusBadRequest)
		return
	}
	pos, _, err := store.GetResumePoint(r.Context(), movieID, clientID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"position_ms": pos})
}

type continueWatchingItem struct {
	Movie      redisstream.MovieMeta `json:"movie"`
	PositionMs int64                 `json:"position_ms"`
}

// handleContinueWatching lists every movie this client has made
// progress in, across every room, with their resume point — the
// cross-movie counterpart to /api/resume (which only answers for one
// specific movie at a time).
func handleContinueWatching(w http.ResponseWriter, r *http.Request, store *redisstream.Store) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		http.Error(w, "client_id query param required", http.StatusBadRequest)
		return
	}
	movieIDs, err := store.ListClientMovies(r.Context(), clientID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	items := make([]continueWatchingItem, 0, len(movieIDs))
	for _, id := range movieIDs {
		meta, ok, err := store.GetMovieMeta(r.Context(), id)
		if err != nil || !ok {
			continue // deleted since; skip silently
		}
		pos, ok, err := store.GetResumePoint(r.Context(), id, clientID)
		if err != nil || !ok || pos <= 0 {
			continue // no real progress worth resuming
		}
		items = append(items, continueWatchingItem{Movie: meta, PositionMs: pos})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

// handleDeleteMovie refuses to delete a movie that's still being
// streamed: the ingest goroutine has no cancellation hook reachable from
// here, so removing its Redis keys wouldn't stop it — the next chunk it
// publishes would just recreate the stream via XADD's auto-create
// behavior, leaving a confusing half-deleted state. Deletion is for
// reclaiming space from movies that have actually finished.
func handleDeleteMovie(w http.ResponseWriter, r *http.Request, store *redisstream.Store) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "POST or DELETE required", http.StatusMethodNotAllowed)
		return
	}
	movieID := r.URL.Query().Get("id")
	if movieID == "" {
		http.Error(w, "id query param required", http.StatusBadRequest)
		return
	}
	meta, ok, err := store.GetMovieMeta(r.Context(), movieID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if meta.Status != "ended" {
		http.Error(w, "movie is still streaming (status: "+meta.Status+"); wait for it to end before deleting", http.StatusConflict)
		return
	}
	if err := store.DeleteMovie(r.Context(), movieID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	removeUpload(movieID)
	w.WriteHeader(http.StatusNoContent)
}

// removeUpload deletes the original uploaded file and its poster. Redis
// cleanup alone left every deleted stream's source video on disk forever.
func removeUpload(movieID string) {
	if !movieIDPattern.MatchString(movieID) {
		return // IDs end up in a path; never trust anything else
	}
	if err := os.RemoveAll(filepath.Join("uploads", movieID)); err != nil {
		log.Printf("remove upload movie=%s: %v", movieID, err)
	}
}

func handleGetMovie(w http.ResponseWriter, r *http.Request, store *redisstream.Store) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id query param required", http.StatusBadRequest)
		return
	}
	meta, ok, err := store.GetMovieMeta(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meta)
}

// streamFrom backfills from fromMillis then live-tails, writing every
// chunk as a binary WS frame, until ctx is cancelled (client disconnected
// or a newer seek superseded this one).
func streamFrom(ctx context.Context, conn *safeConn, store *redisstream.Store, live *hub, movieID string, fromMillis int64) {
	// Subscribe before backfilling: the live feed then overlaps the backfill
	// rather than leaving a gap between them. Overlap is skipped by ID below.
	sub, unsubscribe, err := live.subscribe(movieID)
	if err != nil {
		log.Printf("subscribe movie=%s: %v", movieID, err)
		return
	}
	defer unsubscribe()

	// Send each backfilled segment as soon as it's read; stop early if a
	// newer seek cancelled this stream mid-backfill.
	lastID, err := store.StreamFrom(ctx, movieID, fromMillis, func(c redisstream.Chunk) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return conn.WriteBinary(c.Data)
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("backfill movie=%s from_ms=%d: %v", movieID, fromMillis, err)
		}
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.lagged:
			// Fell behind the shared feed (or it failed): read Redis directly
			// from exactly where this viewer is, so nothing is skipped.
			unsubscribe()
			err := store.Tail(ctx, movieID, lastID, func(id string, c redisstream.Chunk) error {
				return conn.WriteBinary(c.Data)
			})
			if err != nil && ctx.Err() == nil {
				log.Printf("tail movie=%s: %v", movieID, err)
			}
			return
		case lc := <-sub.ch:
			if !idAfter(lc.id, lastID) {
				continue // already sent during backfill
			}
			if err := conn.WriteBinary(lc.data); err != nil {
				return
			}
			lastID = lc.id
		}
	}
}
