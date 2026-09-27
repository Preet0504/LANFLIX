// Command source is the benchmark origin. It runs ONE ffmpeg encoder and
// tees its output to all three delivery systems at once:
//
//   - ours: fragmented MP4 on stdout → the production demuxer → Redis Streams,
//     served to viewers by the real server over WebSocket push
//   - HLS:  fMP4 segments + an EVENT playlist, pushed by ffmpeg over HTTP PUT
//   - DASH: fMP4 segments + a dynamic MPD, pushed by ffmpeg over HTTP PUT
//
// Because every system receives bit-identical frames from the same encoder
// at the same instant, any measured difference is delivery, not encoding.
//
// HLS/DASH are ingested over HTTP into memory rather than written to disk —
// the way production origins ingest. (Disk also doesn't work here: ffmpeg
// updates playlists by renaming a temp file over the old one, and Windows
// refuses that rename while any reader has the file open, which aborted
// the whole encoder within seconds.)
//
// Per segment, it records the wall-clock time the segment became available
// on each system — ours: XADD returned; HLS: listed in an uploaded
// playlist; DASH: listed in an uploaded MPD timeline — and serves those
// timelines, the HLS/DASH content, and the benchmark pages.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"streaming/internal/chunker"
	"streaming/internal/redisstream"
)

type timeline struct {
	mu         sync.Mutex
	MovieID    string           `json:"movie_id"`
	T0         int64            `json:"t0_ms"` // wall clock when the encoder was started
	GOPMs      int64            `json:"gop_ms"`
	Ours       map[int]int64    `json:"ours"` // segment n → wall ms available
	HLS        map[int]int64    `json:"hls"`
	DASH       map[int]int64    `json:"dash"` // 0-based, to line up with ours
	OursSHA256 map[int]string   `json:"ours_sha256"`
	Ended      bool             `json:"ended"`
	Requests   map[string]int64 `json:"requests"`
	RequestLog []reqEntry       `json:"-"`
}

type reqEntry struct {
	At     int64  `json:"at"`
	System string `json:"system"`
	Kind   string `json:"kind"`
}

func nowMs() int64 { return time.Now().UnixMilli() }

func (t *timeline) mark(m map[int]int64, n int, at int64) {
	t.mu.Lock()
	if _, seen := m[n]; !seen {
		m[n] = at
	}
	t.mu.Unlock()
}

// memFS holds everything ffmpeg uploads for HLS/DASH.
type memFS struct {
	mu    sync.RWMutex
	files map[string][]byte
}

func (m *memFS) put(p string, b []byte) { m.mu.Lock(); m.files[p] = b; m.mu.Unlock() }
func (m *memFS) del(p string)           { m.mu.Lock(); delete(m.files, p); m.mu.Unlock() }
func (m *memFS) get(p string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.files[p]
	return b, ok
}

func main() {
	input := flag.String("input", "bench/media/source.mp4", "source video (looped)")
	redisAddr := flag.String("redis", "localhost:6379", "redis address")
	httpAddr := flag.String("http", ":8095", "ingest + serve HLS/DASH, pages and timeline")
	webDir := flag.String("web", "bench/web", "benchmark pages")
	assetsDir := flag.String("assets", "web/assets", "production player assets, served read-only")
	durSec := flag.Int("duration", 2400, "stop encoding after this many seconds")
	gop := flag.Int("gop", 2, "segment duration, seconds")
	flag.Parse()

	_, port, err := net.SplitHostPort(*httpAddr)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store := redisstream.NewStore(*redisAddr)
	defer store.Close()

	room, _, err := store.CreateOrGetRoom(ctx, shortID(), "Benchmark")
	if err != nil {
		log.Fatal(err)
	}
	movieID := shortID()
	if err := store.RegisterMovie(ctx, movieID, room.ID, "Benchmark stream"); err != nil {
		log.Fatal(err)
	}
	defer func() {
		// Leave the user's app the way we found it.
		c := context.Background()
		_ = store.SetMovieStatus(c, movieID, "ended")
		_ = store.DeleteMovie(c, movieID)
		_ = store.DeleteRoom(c, room.ID)
		log.Printf("cleaned up benchmark movie %s and room %s", movieID, room.ID)
	}()

	tl := &timeline{
		MovieID: movieID, GOPMs: int64(*gop) * 1000,
		Ours: map[int]int64{}, HLS: map[int]int64{}, DASH: map[int]int64{},
		OursSHA256: map[int]string{}, Requests: map[string]int64{},
	}
	fs := &memFS{files: map[string][]byte{}}

	// Killing a process on Windows skips signal handlers, so cleanup of the
	// benchmark room would never run; POST /shutdown exits gracefully.
	go serve(*httpAddr, *webDir, *assetsDir, tl, fs, stop)
	time.Sleep(200 * time.Millisecond) // listener up before ffmpeg starts uploading

	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			_ = store.Heartbeat(ctx, movieID)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	ingest := `http\://127.0.0.1\:` + port + `/ingest/`
	tee := strings.Join([]string{
		`[f=mp4:movflags=+frag_keyframe+empty_moov+default_base_moof]pipe\:1`,
		fmt.Sprintf(`[f=hls:method=PUT:hls_time=%d:hls_segment_type=fmp4:hls_list_size=0:hls_playlist_type=event]%shls/index.m3u8`, *gop, ingest),
		fmt.Sprintf(`[f=dash:method=PUT:seg_duration=%d:use_template=1:use_timeline=1:window_size=0]%sdash/manifest.mpd`, *gop, ingest),
	}, "|")

	// Same encoder settings as production (internal/chunker).
	args := []string{
		"-nostats", "-loglevel", "warning",
		"-re", "-stream_loop", "-1", "-i", *input, "-t", strconv.Itoa(*durSec),
		"-map", "0:v:0", "-map", "0:a:0",
		"-c:v", "libx264", "-preset", "veryfast",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", *gop), "-sc_threshold", "0",
		"-c:a", "aac", "-b:a", "128k",
		// Required with tee: MP4-family outputs need codec config in a global
		// header (avcC), but tee doesn't advertise that, so without this x264
		// emits in-band Annex B headers and the MP4 slaves come out malformed.
		"-flags", "+global_header",
		"-f", "tee", tee,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Fatal(err)
	}
	tl.T0 = nowMs()
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	go func() { _, _ = io.Copy(os.Stderr, stderr) }()
	log.Printf("encoding: movie=%s room=%s t0=%d", movieID, room.ID, tl.T0)

	initC := make(chan []byte, 1)
	segC := make(chan chunker.Segment)
	demuxErr := make(chan error, 1)
	go func() {
		demuxErr <- chunker.Demux(ctx, stdout, *gop, initC, segC)
		close(segC)
	}()

	select {
	case init := <-initC:
		if err := store.SetInit(ctx, movieID, init); err != nil {
			log.Fatal(err)
		}
		_ = store.SetMovieStatus(ctx, movieID, "live")
	case <-ctx.Done():
		return
	}

	for seg := range segC {
		if _, err := store.PublishChunk(ctx, movieID, redisstream.Chunk{
			Seq: seg.Seq, PTSMillis: seg.PTSMillis, Keyframe: true, Data: seg.Data,
		}); err != nil {
			log.Printf("publish seq=%d: %v", seg.Seq, err)
			continue
		}
		at := nowMs()
		sum := sha256.Sum256(seg.Data)
		tl.mu.Lock()
		tl.Ours[int(seg.Seq)] = at
		tl.OursSHA256[int(seg.Seq)] = hex.EncodeToString(sum[:])
		tl.mu.Unlock()
	}
	if err := <-demuxErr; err != nil && ctx.Err() == nil {
		log.Printf("demux: %v", err)
	}
	_ = cmd.Wait()
	_ = store.SetMovieStatus(context.Background(), movieID, "ended")
	tl.mu.Lock()
	tl.Ended = true
	tl.mu.Unlock()
	log.Printf("encoding finished; still serving on %s until /shutdown", *httpAddr)
	<-ctx.Done()
}

var (
	hlsSegLine    = regexp.MustCompile(`(?m)^index(\d+)\.m4s\s*$`)
	firstTimeline = regexp.MustCompile(`(?s)<SegmentTimeline>(.*?)</SegmentTimeline>`)
	sEntry        = regexp.MustCompile(`<S\b[^>]*/>`)
	sRepeat       = regexp.MustCompile(`\br="(-?\d+)"`)
)

// A segment is available to an HLS client once an uploaded playlist lists it.
func markHLS(tl *timeline, playlist []byte, at int64) {
	for _, m := range hlsSegLine.FindAllSubmatch(playlist, -1) {
		n, _ := strconv.Atoi(string(m[1]))
		tl.mark(tl.HLS, n, at)
	}
}

// A segment is available to a DASH client once an uploaded MPD lists it in
// the (first, video) SegmentTimeline.
func markDASH(tl *timeline, mpd []byte, at int64) {
	m := firstTimeline.FindSubmatch(mpd)
	if m == nil {
		return
	}
	count := 0
	for _, s := range sEntry.FindAll(m[1], -1) {
		count++
		if r := sRepeat.FindSubmatch(s); r != nil {
			if n, _ := strconv.Atoi(string(r[1])); n > 0 {
				count += n
			}
		}
	}
	for i := 0; i < count; i++ {
		tl.mark(tl.DASH, i, at)
	}
}

func serve(addr, webDir, assetsDir string, tl *timeline, fs *memFS, shutdown context.CancelFunc) {
	mux := http.NewServeMux()

	mux.HandleFunc("/ingest/", func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(strings.TrimPrefix(r.URL.Path, "/ingest/"))
		switch r.Method {
		case http.MethodPut, http.MethodPost:
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fs.put(p, b)
			at := nowMs()
			switch {
			case strings.HasSuffix(p, ".m3u8"):
				markHLS(tl, b, at)
			case strings.HasSuffix(p, ".mpd"):
				markDASH(tl, b, at)
			}
		case http.MethodDelete:
			fs.del(p)
		}
		w.WriteHeader(http.StatusOK)
	})

	types := map[string]string{
		".m3u8": "application/vnd.apple.mpegurl", ".mpd": "application/dash+xml",
		".m4s": "video/mp4", ".mp4": "video/mp4",
	}
	mux.Handle("/out/", countRequests(tl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(strings.TrimPrefix(r.URL.Path, "/out/"))
		b, ok := fs.get(p)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if ct, ok := types[path.Ext(p)]; ok {
			w.Header().Set("Content-Type", ct)
		}
		_, _ = w.Write(b)
	})))

	mux.Handle("/web/", http.StripPrefix("/web/", noStore(http.FileServer(http.Dir(webDir)))))
	mux.Handle("/assets/", http.StripPrefix("/assets/", noStore(http.FileServer(http.Dir(assetsDir)))))
	mux.HandleFunc("/timeline", func(w http.ResponseWriter, r *http.Request) {
		tl.mu.Lock()
		defer tl.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(w).Encode(tl)
	})
	mux.HandleFunc("/requests", func(w http.ResponseWriter, r *http.Request) {
		tl.mu.Lock()
		defer tl.mu.Unlock()
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		out := []reqEntry{}
		for _, e := range tl.RequestLog {
			if e.At >= since {
				out = append(out, e)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		shutdown()
	})
	log.Printf("benchmark origin on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// countRequests logs every viewer-side HLS/DASH request so origin load per
// viewer can be measured. Playlists/manifests are never cacheable, or
// clients would silently miss new segments and latency would be meaningless.
func countRequests(tl *timeline, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/out/")
		system := strings.SplitN(rel, "/", 2)[0]
		kind := "segment"
		switch {
		case strings.HasSuffix(rel, ".m3u8"), strings.HasSuffix(rel, ".mpd"):
			kind = "manifest"
		case strings.Contains(rel, "init"):
			kind = "init"
		}
		tl.mu.Lock()
		tl.Requests[system+"/"+kind]++
		tl.RequestLog = append(tl.RequestLog, reqEntry{At: nowMs(), System: system, Kind: kind})
		tl.mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		h.ServeHTTP(w, r)
	})
}

func shortID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
