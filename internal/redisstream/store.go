// Package redisstream stores and serves GOP-aligned video chunks using a
// single Redis Stream per movie. The stream doubles as both the byte store
// and the timestamp index: entry IDs are Redis's default "<ms-time>-<seq>"
// form, so seeking to a timestamp is a direct XRANGE rather than a separate
// lookup structure.
package redisstream

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Chunk is one GOP-aligned segment of encoded video.
type Chunk struct {
	Seq       int64
	PTSMillis int64
	Keyframe  bool
	Data      []byte
}

type Store struct {
	rdb         *redis.Client
	retentionMs int64
}

func NewStore(addr string) *Store {
	return &Store{rdb: redis.NewClient(&redis.Options{Addr: addr}), retentionMs: defaultRetention.Milliseconds()}
}

// SetRetention changes how much media each stream keeps behind its newest
// chunk; 0 keeps everything.
func (s *Store) SetRetention(d time.Duration) { s.retentionMs = d.Milliseconds() }

func (s *Store) Close() error { return s.rdb.Close() }

func streamKey(movieID string) string {
	return "movie:" + movieID
}

func initKey(movieID string) string {
	return "movie:" + movieID + ":init"
}

// keyframesKey indexes the entries a decoder can start from (score = PTS
// ms, member = entry ID). With one chunk per GOP that's every entry; with
// sub-GOP chunks (low-latency mode) most chunks can't be decoded alone.
func keyframesKey(movieID string) string {
	return "movie:" + movieID + ":keyframes"
}

// SetInit stores the movie's one-time MP4 initialization segment (codec
// config) — fetched once per client, never part of the chunk stream.
func (s *Store) SetInit(ctx context.Context, movieID string, data []byte) error {
	if err := s.rdb.Set(ctx, initKey(movieID), data, 0).Err(); err != nil {
		return fmt.Errorf("set init: %w", err)
	}
	return nil
}

func (s *Store) GetInit(ctx context.Context, movieID string) ([]byte, error) {
	data, err := s.rdb.Get(ctx, initKey(movieID)).Bytes()
	if err != nil {
		return nil, fmt.Errorf("get init: %w", err)
	}
	return data, nil
}

// defaultRetention is how much media a stream keeps behind its newest
// chunk. It used to be a chunk count (5000 — about 2.8 hours of 2s
// chunks), which would give a 200ms low-latency stream only 17 minutes.
// Entry IDs are presentation timestamps, so XADD's MINID trims by media
// time directly, whatever the chunk size. Approximate ("~") so it stays
// cheap on every write.
const defaultRetention = 3 * time.Hour

// PublishChunk appends a chunk to the movie's stream and returns its entry ID.
func (s *Store) PublishChunk(ctx context.Context, movieID string, c Chunk) (string, error) {
	// ID is keyed on the video's own PTS timeline, not wall-clock time —
	// that's what lets RangeFrom seek by PTS with a plain XRANGE instead
	// of a separate index.
	id := fmt.Sprintf("%d-%d", c.PTSMillis, c.Seq+1) // +1: Redis rejects the reserved ID "0-0"
	args := &redis.XAddArgs{
		Stream: streamKey(movieID),
		ID:     id,
		Values: map[string]interface{}{
			"seq":      c.Seq,
			"pts_ms":   c.PTSMillis,
			"keyframe": c.Keyframe,
			"data":     c.Data,
		},
	}
	minMs := c.PTSMillis - s.retentionMs
	if s.retentionMs > 0 && minMs > 0 {
		args.MinID, args.Approx = strconv.FormatInt(minMs, 10), true
	}
	pipe := s.rdb.TxPipeline()
	pipe.XAdd(ctx, args)
	if c.Keyframe {
		pipe.ZAdd(ctx, keyframesKey(movieID), redis.Z{Score: float64(c.PTSMillis), Member: id})
	}
	if args.MinID != "" {
		pipe.ZRemRangeByScore(ctx, keyframesKey(movieID), "-inf", "("+args.MinID)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return "", fmt.Errorf("publish chunk seq=%d: %w", c.Seq, err)
	}
	return id, nil
}

// RangeFrom returns every chunk covering fromMillis onward, oldest first —
// the backfill a late joiner uses to catch up before switching to Tail.
//
// Entry IDs are keyed by each segment's *start* PTS, so a naive XRANGE
// from fromMillis would skip the segment that actually contains it
// whenever fromMillis isn't exactly on a GOP boundary (real playback
// positions from resume/seek almost never are). We first find the last
// segment starting at or before fromMillis and start there instead, so
// the client always gets the segment that covers the requested moment.
func (s *Store) RangeFrom(ctx context.Context, movieID string, fromMillis int64) ([]Chunk, string, error) {
	var chunks []Chunk
	lastID, err := s.StreamFrom(ctx, movieID, fromMillis, func(c Chunk) error {
		chunks = append(chunks, c)
		return nil
	})
	return chunks, lastID, err
}

// rangePage bounds how much of a backfill is held in memory at once.
const rangePage = 4

// StreamFrom is RangeFrom delivered incrementally: it hands each chunk to
// fn as soon as its page is read, and returns the ID of the last one (the
// point to Tail from). Reading the whole range up front — as RangeFrom
// once did — made the first byte of a seek wait for *every* segment from
// the target to the live edge to come out of Redis: a seek 40 segments
// back read ~24MB before sending anything, so seek latency grew with
// distance from live. Found by the benchmark, where it cost us ~2x vs HLS.
func (s *Store) StreamFrom(ctx context.Context, movieID string, fromMillis int64, fn func(Chunk) error) (string, error) {
	key := streamKey(movieID)
	start := "-"
	if fromMillis > 0 {
		var err error
		if start, err = s.startEntry(ctx, movieID, fromMillis); err != nil {
			return "", err
		}
	}
	lastID := ""
	for {
		entries, err := s.rdb.XRangeN(ctx, key, start, "+", rangePage).Result()
		if err != nil {
			return "", fmt.Errorf("range from %d: %w", fromMillis, err)
		}
		for _, e := range entries {
			c, err := decode(e.Values)
			if err != nil {
				return "", err
			}
			if err := fn(c); err != nil {
				return "", err
			}
			lastID = e.ID
		}
		if len(entries) < rangePage {
			break
		}
		start = "(" + lastID // exclusive: continue after the last one sent
	}
	if lastID == "" {
		lastID = "0-0" // nothing yet: tail everything that arrives
	}
	return lastID, nil
}

// startEntry picks the entry a viewer starting at fromMillis must begin
// from: the last keyframe chunk at or before it. Starting on any other
// chunk hands the decoder frames that reference ones it never received.
// Streams published before the keyframe index existed (all one-GOP
// chunks, so every entry is a keyframe) fall back to the containing entry.
func (s *Store) startEntry(ctx context.Context, movieID string, fromMillis int64) (string, error) {
	ids, err := s.rdb.ZRevRangeByScore(ctx, keyframesKey(movieID), &redis.ZRangeBy{
		Max: strconv.FormatInt(fromMillis, 10), Min: "-inf", Count: 1,
	}).Result()
	if err != nil {
		return "", fmt.Errorf("find keyframe for %d: %w", fromMillis, err)
	}
	if len(ids) > 0 {
		return ids[0], nil
	}
	containing, err := s.rdb.XRevRangeN(ctx, streamKey(movieID), strconv.FormatInt(fromMillis, 10), "-", 1).Result()
	if err != nil {
		return "", fmt.Errorf("find containing segment for %d: %w", fromMillis, err)
	}
	if len(containing) > 0 {
		return containing[0].ID, nil
	}
	return "-", nil
}

// Tail blocks waiting for chunks after afterID and calls handler for each,
// updating afterID as it goes. It runs until ctx is cancelled or handler
// returns an error. Pass "$" as afterID to start from "new chunks only".
func (s *Store) Tail(ctx context.Context, movieID string, afterID string, handler func(id string, c Chunk) error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		res, err := s.rdb.XRead(ctx, &redis.XReadArgs{
			Streams: []string{streamKey(movieID), afterID},
			Block:   0, // block indefinitely until a new entry arrives
			Count:   64,
		}).Result()
		if err != nil {
			if err == redis.Nil {
				continue
			}
			return fmt.Errorf("tail after %s: %w", afterID, err)
		}

		for _, stream := range res {
			for _, e := range stream.Messages {
				c, err := decode(e.Values)
				if err != nil {
					return err
				}
				if err := handler(e.ID, c); err != nil {
					return err
				}
				afterID = e.ID
			}
		}
	}
}

// LatestID returns the stream ID of the newest chunk, or "0-0" if there
// are none yet — a position to tail from that skips nothing after it.
func (s *Store) LatestID(ctx context.Context, movieID string) (string, error) {
	entries, err := s.rdb.XRevRangeN(ctx, streamKey(movieID), "+", "-", 1).Result()
	if err != nil {
		return "", fmt.Errorf("latest id: %w", err)
	}
	if len(entries) == 0 {
		return "0-0", nil
	}
	return entries[0].ID, nil
}

// LatestPTS returns the PTS of the most recent chunk published for a
// movie — the "live edge" a viewer's seek range is clamped to, since
// nothing past it exists in the cache yet.
func (s *Store) LatestPTS(ctx context.Context, movieID string) (int64, bool, error) {
	entries, err := s.rdb.XRevRangeN(ctx, streamKey(movieID), "+", "-", 1).Result()
	if err != nil {
		return 0, false, fmt.Errorf("latest pts: %w", err)
	}
	if len(entries) == 0 {
		return 0, false, nil
	}
	c, err := decode(entries[0].Values)
	if err != nil {
		return 0, false, err
	}
	return c.PTSMillis, true, nil
}

// --- Movie registry: what's streaming, and its metadata --------------

type MovieMeta struct {
	ID               string `json:"id"`
	RoomID           string `json:"room_id"`
	Title            string `json:"title"`
	Status           string `json:"status"` // "encoding" | "live" | "ended"
	StartedAtMs      int64  `json:"started_at_ms"`
	LiveEdgeMs       int64  `json:"live_edge_ms"`
	HasLiveEdge      bool   `json:"has_live_edge"`
	TotalViewersEver int64  `json:"total_viewers_ever"`
	// ChunkMs is the stream's chunk duration: 2000 (one chunk per GOP) or
	// 200 in low-latency mode. The live edge is the *start* of the newest
	// chunk, so clients need it to know where the newest media ends.
	ChunkMs int64 `json:"chunk_ms"`
}

// DefaultChunkMs is the chunk duration of streams that don't record one.
const DefaultChunkMs = 2000

// SetChunkMs records a stream's chunk duration.
func (s *Store) SetChunkMs(ctx context.Context, movieID string, chunkMs int64) error {
	if err := s.rdb.HSet(ctx, movieMetaKey(movieID), "chunk_ms", chunkMs).Err(); err != nil {
		return fmt.Errorf("set chunk_ms: %w", err)
	}
	return nil
}

func movieMetaKey(movieID string) string { return "movie:" + movieID + ":meta" }
func moviesIndexKey() string             { return "movies:all" }

// RegisterMovie records a new movie in the index with status "encoding",
// tagged to the room it's streaming in.
func (s *Store) RegisterMovie(ctx context.Context, movieID, roomID, title string) error {
	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, movieMetaKey(movieID), map[string]interface{}{
		"room_id":       roomID,
		"title":         title,
		"status":        "encoding",
		"started_at_ms": time.Now().UnixMilli(),
	})
	pipe.SAdd(ctx, moviesIndexKey(), movieID)
	pipe.Set(ctx, heartbeatKey(movieID), 1, heartbeatTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("register movie: %w", err)
	}
	return nil
}

// --- Liveness: is anything still encoding this movie? ------------------
//
// Encoding runs inside whichever process called ingest.Run. If that
// process dies (server restart, crash), nothing ever sets the status to
// "ended" — the movie would sit at "live" forever and, since deletion
// refuses live movies, could never be cleaned up either. The ingester
// refreshes a short-TTL key while it runs; a non-ended movie whose key
// has expired has no writer left, and is treated as ended.

const heartbeatTTL = 10 * time.Second

func heartbeatKey(movieID string) string { return "movie:" + movieID + ":alive" }

func (s *Store) Heartbeat(ctx context.Context, movieID string) error {
	return s.rdb.Set(ctx, heartbeatKey(movieID), 1, heartbeatTTL).Err()
}

func (s *Store) SetMovieStatus(ctx context.Context, movieID, status string) error {
	if err := s.rdb.HSet(ctx, movieMetaKey(movieID), "status", status).Err(); err != nil {
		return fmt.Errorf("set movie status: %w", err)
	}
	return nil
}

func (s *Store) GetMovieMeta(ctx context.Context, movieID string) (MovieMeta, bool, error) {
	vals, err := s.rdb.HGetAll(ctx, movieMetaKey(movieID)).Result()
	if err != nil {
		return MovieMeta{}, false, fmt.Errorf("get movie meta: %w", err)
	}
	if len(vals) == 0 {
		return MovieMeta{}, false, nil
	}
	meta := MovieMeta{ID: movieID, RoomID: vals["room_id"], Title: vals["title"], Status: vals["status"]}
	meta.StartedAtMs, _ = strconv.ParseInt(vals["started_at_ms"], 10, 64)
	if meta.ChunkMs, _ = strconv.ParseInt(vals["chunk_ms"], 10, 64); meta.ChunkMs <= 0 {
		meta.ChunkMs = DefaultChunkMs
	}
	if meta.Status != "ended" {
		if n, err := s.rdb.Exists(ctx, heartbeatKey(movieID)).Result(); err == nil && n == 0 {
			// Orphaned: its ingester is gone. Persist so the correction
			// sticks and the movie becomes deletable.
			meta.Status = "ended"
			s.rdb.HSet(ctx, movieMetaKey(movieID), "status", "ended")
		}
	}
	if edge, ok, err := s.LatestPTS(ctx, movieID); err == nil && ok {
		meta.LiveEdgeMs = edge
		meta.HasLiveEdge = true
	}
	if n, err := s.TotalViewersEver(ctx, movieID); err == nil {
		meta.TotalViewersEver = n
	}
	return meta, true, nil
}

// ListMovies returns metadata for every known movie, most recently
// started first.
func (s *Store) ListMovies(ctx context.Context) ([]MovieMeta, error) {
	ids, err := s.rdb.SMembers(ctx, moviesIndexKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("list movies: %w", err)
	}
	metas := make([]MovieMeta, 0, len(ids))
	for _, id := range ids {
		meta, ok, err := s.GetMovieMeta(ctx, id)
		if err != nil || !ok {
			continue
		}
		metas = append(metas, meta)
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].StartedAtMs > metas[j].StartedAtMs })
	return metas, nil
}

// ListMoviesByRoom returns only the movies streaming in one room, most
// recently started first.
func (s *Store) ListMoviesByRoom(ctx context.Context, roomID string) ([]MovieMeta, error) {
	all, err := s.ListMovies(ctx)
	if err != nil {
		return nil, err
	}
	filtered := make([]MovieMeta, 0, len(all))
	for _, m := range all {
		if m.RoomID == roomID {
			filtered = append(filtered, m)
		}
	}
	return filtered, nil
}

// --- Room registry: a host's named container for one or more streams -

type RoomMeta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CreatedAtMs int64  `json:"created_at_ms"`
}

func roomMetaKey(roomID string) string { return "room:" + roomID + ":meta" }
func roomsIndexKey() string            { return "rooms:all" }

// roomNamesKey maps a normalized room name to the one room that owns it.
// Names are unique: without this, "Project" and "project " were two
// separate rooms, and a host creating a room they'd already made got a
// silent duplicate instead of their existing room back.
func roomNamesKey() string { return "rooms:byname" }

// NormalizeRoomName is what uniqueness is judged on: case-insensitive,
// with runs of whitespace collapsed and ends trimmed.
func NormalizeRoomName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// CreateOrGetRoom claims a room name atomically (HSETNX). If the name is
// free, the room is created under candidateID; if it's already taken,
// the existing room is returned untouched and created is false.
func (s *Store) CreateOrGetRoom(ctx context.Context, candidateID, name string) (meta RoomMeta, created bool, err error) {
	display := strings.Join(strings.Fields(name), " ")
	norm := NormalizeRoomName(display)
	if norm == "" {
		return RoomMeta{}, false, fmt.Errorf("room name is empty")
	}

	claimed, err := s.rdb.HSetNX(ctx, roomNamesKey(), norm, candidateID).Result()
	if err != nil {
		return RoomMeta{}, false, fmt.Errorf("claim room name: %w", err)
	}
	if !claimed {
		existingID, err := s.rdb.HGet(ctx, roomNamesKey(), norm).Result()
		if err != nil {
			return RoomMeta{}, false, fmt.Errorf("look up room name: %w", err)
		}
		existing, ok, err := s.GetRoomMeta(ctx, existingID)
		if err != nil {
			return RoomMeta{}, false, err
		}
		if ok {
			return existing, false, nil
		}
		// The name index pointed at a room that no longer exists (deleted
		// out from under it); take the name over rather than refusing.
		if err := s.rdb.HSet(ctx, roomNamesKey(), norm, candidateID).Err(); err != nil {
			return RoomMeta{}, false, fmt.Errorf("reclaim room name: %w", err)
		}
	}

	now := time.Now().UnixMilli()
	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, roomMetaKey(candidateID), map[string]interface{}{
		"name":          display,
		"created_at_ms": now,
	})
	pipe.SAdd(ctx, roomsIndexKey(), candidateID)
	if _, err := pipe.Exec(ctx); err != nil {
		return RoomMeta{}, false, fmt.Errorf("create room: %w", err)
	}
	return RoomMeta{ID: candidateID, Name: display, CreatedAtMs: now}, true, nil
}

// ReconcileRooms rebuilds the name index from existing rooms and merges
// any duplicates that predate it: for each name, the oldest room keeps
// it, and every stream in a newer same-named room is re-tagged into the
// keeper. Re-tagging only touches a movie's room_id field, so streams
// that are live mid-merge keep encoding and playing untouched. Safe to
// run on every startup.
func (s *Store) ReconcileRooms(ctx context.Context) (merged int, err error) {
	rooms, err := s.ListRooms(ctx)
	if err != nil {
		return 0, err
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].CreatedAtMs < rooms[j].CreatedAtMs })

	keeperByName := map[string]string{}
	for _, r := range rooms {
		norm := NormalizeRoomName(r.Name)
		keeper, seen := keeperByName[norm]
		if !seen {
			keeperByName[norm] = r.ID
			if err := s.rdb.HSet(ctx, roomNamesKey(), norm, r.ID).Err(); err != nil {
				return merged, fmt.Errorf("index room name: %w", err)
			}
			continue
		}

		movies, err := s.ListMoviesByRoom(ctx, r.ID)
		if err != nil {
			return merged, err
		}
		pipe := s.rdb.Pipeline()
		for _, m := range movies {
			pipe.HSet(ctx, movieMetaKey(m.ID), "room_id", keeper)
		}
		pipe.Del(ctx, roomMetaKey(r.ID))
		pipe.SRem(ctx, roomsIndexKey(), r.ID)
		if _, err := pipe.Exec(ctx); err != nil {
			return merged, fmt.Errorf("merge room %s into %s: %w", r.ID, keeper, err)
		}
		merged++
	}
	return merged, nil
}

// DeleteRoom removes a room and every stream in it. Like DeleteMovie, it
// refuses while any stream is still encoding or live — deleting keys out
// from under a running ingest would just get them recreated.
func (s *Store) DeleteRoom(ctx context.Context, roomID string) error {
	meta, ok, err := s.GetRoomMeta(ctx, roomID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	movies, err := s.ListMoviesByRoom(ctx, roomID)
	if err != nil {
		return err
	}
	for _, m := range movies {
		if m.Status != "ended" {
			return ErrStillStreaming
		}
	}
	for _, m := range movies {
		if err := s.DeleteMovie(ctx, m.ID); err != nil {
			return err
		}
	}

	norm := NormalizeRoomName(meta.Name)
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, roomMetaKey(roomID))
	pipe.SRem(ctx, roomsIndexKey(), roomID)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("delete room: %w", err)
	}
	// Only release the name if it still points at this room.
	if owner, err := s.rdb.HGet(ctx, roomNamesKey(), norm).Result(); err == nil && owner == roomID {
		s.rdb.HDel(ctx, roomNamesKey(), norm)
	}
	return nil
}

var (
	ErrNotFound       = errors.New("not found")
	ErrStillStreaming = errors.New("still streaming")
)

func (s *Store) GetRoomMeta(ctx context.Context, roomID string) (RoomMeta, bool, error) {
	vals, err := s.rdb.HGetAll(ctx, roomMetaKey(roomID)).Result()
	if err != nil {
		return RoomMeta{}, false, fmt.Errorf("get room meta: %w", err)
	}
	if len(vals) == 0 {
		return RoomMeta{}, false, nil
	}
	meta := RoomMeta{ID: roomID, Name: vals["name"]}
	meta.CreatedAtMs, _ = strconv.ParseInt(vals["created_at_ms"], 10, 64)
	return meta, true, nil
}

// ListRooms returns every known room, most recently created first.
func (s *Store) ListRooms(ctx context.Context) ([]RoomMeta, error) {
	ids, err := s.rdb.SMembers(ctx, roomsIndexKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	metas := make([]RoomMeta, 0, len(ids))
	for _, id := range ids {
		meta, ok, err := s.GetRoomMeta(ctx, id)
		if err != nil || !ok {
			continue
		}
		metas = append(metas, meta)
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].CreatedAtMs > metas[j].CreatedAtMs })
	return metas, nil
}

// DeleteMovie removes everything a movie occupies in Redis: the chunk
// stream, the init segment, the registry entry, every client's resume
// point for it, and its analytics. This is the only way to actually
// reclaim memory for a movie today — MAXLEN trimming bounds an
// individual stream's size, but nothing ever removes an ended movie's
// keys on its own.
//
// One thing this deliberately does NOT do: remove this movie's ID from
// every client's "movies I've touched" set (client:<id>:movies) — that
// would mean scanning every client just to delete one movie. A stale
// reference left behind is harmless: ListClientMovies already skips any
// movie ID whose meta lookup comes back empty.
func (s *Store) DeleteMovie(ctx context.Context, movieID string) error {
	resumeKeys, err := s.rdb.Keys(ctx, "resume:"+movieID+":*").Result()
	if err != nil {
		return fmt.Errorf("find resume points: %w", err)
	}

	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, streamKey(movieID))
	pipe.Del(ctx, initKey(movieID))
	pipe.Del(ctx, keyframesKey(movieID))
	pipe.Del(ctx, movieMetaKey(movieID))
	pipe.Del(ctx, reachKey(movieID))
	pipe.Del(ctx, heartbeatKey(movieID))
	pipe.SRem(ctx, moviesIndexKey(), movieID)
	if len(resumeKeys) > 0 {
		pipe.Del(ctx, resumeKeys...)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("delete movie: %w", err)
	}
	return nil
}

// --- Resume points: last known position per (movie, client) ----------

func resumeKey(movieID, clientID string) string {
	return "resume:" + movieID + ":" + clientID
}

func (s *Store) SaveResumePoint(ctx context.Context, movieID, clientID string, positionMillis int64) error {
	if err := s.rdb.Set(ctx, resumeKey(movieID, clientID), positionMillis, 30*24*time.Hour).Err(); err != nil {
		return fmt.Errorf("save resume point: %w", err)
	}
	return nil
}

func (s *Store) GetResumePoint(ctx context.Context, movieID, clientID string) (int64, bool, error) {
	v, err := s.rdb.Get(ctx, resumeKey(movieID, clientID)).Int64()
	if err != nil {
		if err == redis.Nil {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("get resume point: %w", err)
	}
	return v, true, nil
}

// --- Continue watching: which movies has this client touched? --------
//
// This is current-state, not history — "what am I in the middle of" —
// so like resume points it's written directly at ping time, not via
// Kafka. Kafka's replay strength doesn't apply here; there's only ever
// one answer ("my latest position"), never a need to reconstruct it
// from a stream of past events.

func clientMoviesKey(clientID string) string { return "client:" + clientID + ":movies" }

func (s *Store) RecordClientMovie(ctx context.Context, clientID, movieID string) error {
	if err := s.rdb.SAdd(ctx, clientMoviesKey(clientID), movieID).Err(); err != nil {
		return fmt.Errorf("record client movie: %w", err)
	}
	return nil
}

func (s *Store) ListClientMovies(ctx context.Context, clientID string) ([]string, error) {
	ids, err := s.rdb.SMembers(ctx, clientMoviesKey(clientID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list client movies: %w", err)
	}
	return ids, nil
}

// --- Reach: the analytics consumer's materialized view of engagement -
//
// Unlike resume points, this genuinely needs Kafka's replay: "how many
// distinct people have ever watched this, and how far did each get" can
// only be answered by processing the full event history, not by reading
// current state. Written exclusively by cmd/analytics (a separate Kafka
// consumer group), read by cmd/server — the two never talk to each
// other directly, only through this Redis view and the Kafka topic.

func reachKey(movieID string) string { return "analytics:" + movieID + ":reach" }

// UpdateReach records the furthest point a client has ever reached in a
// movie. ZADD GT: a late-arriving or out-of-order event with a smaller
// position than what's already recorded is a no-op, so replaying the
// topic (or replaying it again after a restart) is always safe.
func (s *Store) UpdateReach(ctx context.Context, movieID, clientID string, positionMillis int64) error {
	err := s.rdb.ZAddArgs(ctx, reachKey(movieID), redis.ZAddArgs{
		GT:      true,
		Members: []redis.Z{{Score: float64(positionMillis), Member: clientID}},
	}).Err()
	if err != nil {
		return fmt.Errorf("update reach: %w", err)
	}
	return nil
}

// TotalViewersEver is the count of distinct clients who have ever
// reported a position for this movie — "most watched" ranking data.
func (s *Store) TotalViewersEver(ctx context.Context, movieID string) (int64, error) {
	n, err := s.rdb.ZCard(ctx, reachKey(movieID)).Result()
	if err != nil {
		return 0, fmt.Errorf("total viewers ever: %w", err)
	}
	return n, nil
}

// RetentionAt counts how many of a movie's distinct viewers ever reached
// at least atMillis into it — one point on an audience-retention curve
// (the same shape YouTube/Wistia-style analytics use).
func (s *Store) RetentionAt(ctx context.Context, movieID string, atMillis int64) (int64, error) {
	n, err := s.rdb.ZCount(ctx, reachKey(movieID), strconv.FormatInt(atMillis, 10), "+inf").Result()
	if err != nil {
		return 0, fmt.Errorf("retention at %d: %w", atMillis, err)
	}
	return n, nil
}

func decode(v map[string]interface{}) (Chunk, error) {
	seq, err := strconv.ParseInt(fmt.Sprint(v["seq"]), 10, 64)
	if err != nil {
		return Chunk{}, fmt.Errorf("decode seq: %w", err)
	}
	pts, err := strconv.ParseInt(fmt.Sprint(v["pts_ms"]), 10, 64)
	if err != nil {
		return Chunk{}, fmt.Errorf("decode pts_ms: %w", err)
	}
	keyframe := fmt.Sprint(v["keyframe"]) == "1" || fmt.Sprint(v["keyframe"]) == "true"

	var data []byte
	switch d := v["data"].(type) {
	case string:
		data = []byte(d)
	case []byte:
		data = d
	default:
		return Chunk{}, fmt.Errorf("decode data: unexpected type %T", v["data"])
	}

	return Chunk{Seq: seq, PTSMillis: pts, Keyframe: keyframe, Data: data}, nil
}
