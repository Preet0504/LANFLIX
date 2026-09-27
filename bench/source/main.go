// Command source is the benchmark origin. It runs ONE ffmpeg encoder and
// tees its output to every delivery system at once:
//
//   - ours: fragmented MP4 (one fragment per 2s GOP) on stdout → the
//     production demuxer → Redis Streams, served to viewers by the real
//     server over WebSocket push
//   - HLS:  fMP4 segments + an EVENT playlist, pushed by ffmpeg over HTTP PUT
//   - DASH: fMP4 segments + a dynamic MPD, pushed by ffmpeg over HTTP PUT
//   - low-latency: 200ms fMP4 fragments over TCP, demuxed once and handed
//     at the same instant to (a) a second Redis stream served by the same
//     production server ("ours-ll") and (b) an LL-HLS packager (llhls.go)
//   - WebRTC: H.264 + Opus over RTP, relayed to browsers by pion (webrtc.go)
//
// Because every system receives bit-identical frames from the same encoder
// at the same instant, any measured difference is delivery, not encoding.
// (WebRTC's audio is Opus, a second encode of the same source: WebRTC
// can't carry AAC. Video is the same bitstream everywhere.)
//
// The encoder runs without B-frames (-bf 0), as low-latency live encoders
// do: WebRTC can't carry them, and one encoder must serve every system.
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
	"math"
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
	LLMovieID  string           `json:"ll_movie_id"`
	T0         int64            `json:"t0_ms"` // wall clock when the encoder was started
	GOPMs      int64            `json:"gop_ms"`
	Ours       map[int]int64    `json:"ours"` // segment n → wall ms available
	HLS        map[int]int64    `json:"hls"`
	DASH       map[int]int64    `json:"dash"` // 0-based, to line up with ours
	OursSHA256 map[int]string   `json:"ours_sha256"`
	OursLL     map[int]int64    `json:"ours_ll"` // 200ms fragment n → wall ms XADD returned
	LLHLS      map[int]int64    `json:"llhls"`   // fragment n → wall ms servable as an LL-HLS part
	LLFrags    []llFrag         `json:"ll_frags"`
	WebRTC     *rtpAnchor       `json:"webrtc"`
	Encoder    *encoderClock    `json:"encoder_clock"` // filled in when the full timeline is served
	Ended      bool             `json:"ended"`
	Requests   map[string]int64 `json:"requests"`
	RequestLog []reqEntry       `json:"-"`
}

type llFrag struct {
	Msn     int     `json:"msn"`
	Part    int     `json:"part"`
	StartMs float64 `json:"start_ms"` // media time, same timeline as every MSE player
	DurMs   float64 `json:"dur_ms"`
	Key     bool    `json:"key"`
}

// rtpAnchor maps WebRTC's RTP timestamps onto the media timeline the MSE
// players report: the same keyframe's RTP timestamp and its fMP4 decode
// time. (Keyframe #1, not #0: the muxer folds AAC priming into the first
// fMP4 frame's duration, so only from the second GOP on do the two
// timelines differ by a pure constant.)
type rtpAnchor struct {
	RTPTs   uint32  `json:"rtp_ts"`
	MediaMs float64 `json:"media_ms"`
	// Keyframe spacing seen on each side, as a consistency check.
	RTPGapMs   float64 `json:"rtp_gap_ms"`
	MediaGapMs float64 `json:"media_gap_ms"`
}

// lite is what the benchmark pages poll: the full timeline grows to tens
// of thousands of entries, and re-downloading it every second would load
// the very browser being measured.
func (t *timeline) lite() map[string]any {
	edge, llEdge := -1, 0.0
	for n := range t.Ours {
		edge = max(edge, n)
	}
	if n := len(t.LLFrags); n > 0 {
		// End of the newest fragment: LL-HLS's hold-back is measured from
		// the end of the playlist, and ours-ll must start from the same point.
		llEdge = t.LLFrags[n-1].StartMs + t.LLFrags[n-1].DurMs
	}
	return map[string]any{
		"movie_id": t.MovieID, "ll_movie_id": t.LLMovieID, "t0_ms": t.T0, "gop_ms": t.GOPMs,
		"ours_edge": edge, "ll_edge_ms": llEdge, "webrtc": t.WebRTC, "ended": t.Ended,
	}
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
	durSec := flag.Int("duration", 3600, "stop encoding after this many seconds")
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
	// Keep the whole run: seeks may target any point in it.
	store.SetRetention(0)

	room, _, err := store.CreateOrGetRoom(ctx, shortID(), "Benchmark")
	if err != nil {
		log.Fatal(err)
	}
	movieID, llMovieID := shortID(), shortID()
	if err := store.RegisterMovie(ctx, movieID, room.ID, "Benchmark stream"); err != nil {
		log.Fatal(err)
	}
	if err := store.RegisterMovie(ctx, llMovieID, room.ID, "Benchmark stream (200ms chunks)"); err != nil {
		log.Fatal(err)
	}
	if err := store.SetChunkMs(ctx, llMovieID, 200); err != nil {
		log.Fatal(err)
	}
	defer func() {
		// Leave the user's app the way we found it.
		c := context.Background()
		for _, id := range []string{movieID, llMovieID} {
			_ = store.SetMovieStatus(c, id, "ended")
			_ = store.DeleteMovie(c, id)
		}
		_ = store.DeleteRoom(c, room.ID)
		log.Printf("cleaned up benchmark movies %s, %s and room %s", movieID, llMovieID, room.ID)
	}()

	tl := &timeline{
		MovieID: movieID, LLMovieID: llMovieID, GOPMs: int64(*gop) * 1000,
		Ours: map[int]int64{}, HLS: map[int]int64{}, DASH: map[int]int64{},
		OursSHA256: map[int]string{}, OursLL: map[int]int64{}, LLHLS: map[int]int64{},
		Requests: map[string]int64{},
	}
	fs := &memFS{files: map[string][]byte{}}
	ll := newLLHLS()
	rtc, err := newRTCOrigin()
	if err != nil {
		log.Fatal(err)
	}
	if err := rtc.relay(ctx, "127.0.0.1:47004", rtc.video, true); err != nil {
		log.Fatal(err)
	}
	if err := rtc.relay(ctx, "127.0.0.1:47006", rtc.audio, false); err != nil {
		log.Fatal(err)
	}
	llListener, err := net.Listen("tcp", "127.0.0.1:47010")
	if err != nil {
		log.Fatal(err)
	}

	// Killing a process on Windows skips signal handlers, so cleanup of the
	// benchmark room would never run; POST /shutdown exits gracefully.
	go serve(*httpAddr, *webDir, *assetsDir, tl, fs, ll, rtc, stop)
	time.Sleep(200 * time.Millisecond) // listener up before ffmpeg starts uploading

	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			_ = store.Heartbeat(ctx, movieID)
			_ = store.Heartbeat(ctx, llMovieID)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	// Stream indexes: 0 video, 1 AAC, 2 Opus (WebRTC only).
	ingest := `http\://127.0.0.1\:` + port + `/ingest/`
	tee := strings.Join([]string{
		`[select=0,1:f=mp4:movflags=+frag_keyframe+empty_moov+default_base_moof]pipe\:1`,
		fmt.Sprintf(`[select=0,1:f=hls:method=PUT:hls_time=%d:hls_segment_type=fmp4:hls_list_size=0:hls_playlist_type=event]%shls/index.m3u8`, *gop, ingest),
		fmt.Sprintf(`[select=0,1:f=dash:method=PUT:seg_duration=%d:use_template=1:use_timeline=1:window_size=0]%sdash/manifest.mpd`, *gop, ingest),
		`[select=0,1:f=mp4:movflags=+frag_keyframe+empty_moov+default_base_moof:frag_duration=200000]tcp\://127.0.0.1\:47010`,
		// dump_extra: with a global header, SPS/PPS live only in extradata;
		// WebRTC receivers need them in-band before every keyframe.
		`[select=0:f=rtp:bsfs/v=dump_extra]rtp\://127.0.0.1\:47004?pkt_size=1200`,
		`[select=2:f=rtp]rtp\://127.0.0.1\:47006?pkt_size=1200`,
	}, "|")

	// Production encoder settings (internal/chunker) plus -bf 0.
	args := []string{
		"-nostats", "-loglevel", "warning",
		"-re", "-stream_loop", "-1", "-i", *input, "-t", strconv.Itoa(*durSec),
		"-map", "0:v:0", "-map", "0:a:0", "-map", "0:a:0",
		"-c:v", "libx264", "-preset", "veryfast", "-bf", "0",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", *gop), "-sc_threshold", "0",
		"-c:a:0", "aac", "-b:a:0", "128k",
		"-c:a:1", "libopus", "-b:a:1", "128k", "-ar:a:1", "48000",
		// Required with tee: MP4-family outputs need codec config in a global
		// header (avcC), but tee doesn't advertise that, so without this x264
		// emits in-band Annex B headers and the MP4 slaves come out malformed.
		"-flags", "+global_header",
		"-f", "tee", tee,
	}
	go ingestLowLatency(ctx, llListener, store, llMovieID, ll, rtc, tl)
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

// ingestLowLatency demuxes the 200ms-fragment output once and hands every
// fragment, at the same instant, to the LL-HLS packager and to a second
// Redis stream served by the production server (ours-ll). Each consumer
// has its own goroutine, so neither system waits on the other's write.
func ingestLowLatency(ctx context.Context, ln net.Listener, store *redisstream.Store, movieID string, ll *llhls, rtc *rtcOrigin, tl *timeline) {
	conn, err := ln.Accept()
	ln.Close()
	if err != nil {
		log.Printf("low-latency ingest: accept: %v", err)
		return
	}
	defer conn.Close()

	initC := make(chan []byte, 1)
	fragC := make(chan chunker.Fragment, 64)
	errC := make(chan error, 1)
	go func() {
		errC <- chunker.DemuxFragments(ctx, conn, initC, fragC)
		close(fragC)
	}()

	select {
	case init := <-initC:
		ll.setInit(init)
		if err := store.SetInit(ctx, movieID, init); err != nil {
			log.Printf("low-latency ingest: set init: %v", err)
			return
		}
		_ = store.SetMovieStatus(ctx, movieID, "live")
	case err := <-errC:
		log.Printf("low-latency ingest: no init segment: %v", err)
		return
	}

	toRedis := make(chan chunker.Fragment, 256)
	redisDone := make(chan struct{})
	go func() {
		defer close(redisDone)
		for f := range toRedis {
			if _, err := store.PublishChunk(ctx, movieID, redisstream.Chunk{
				Seq: f.Seq, PTSMillis: int64(math.Round(f.DecodeMs)), Keyframe: f.Keyframe, Data: f.Data,
			}); err != nil {
				log.Printf("ours-ll publish seq=%d: %v", f.Seq, err)
				continue
			}
			tl.mark(tl.OursLL, int(f.Seq), nowMs())
		}
	}()

	var keyMs []float64
	for f := range fragC {
		msn, part := ll.add(f)
		at := nowMs()
		toRedis <- f

		tl.mu.Lock()
		tl.LLHLS[int(f.Seq)] = at
		tl.LLFrags = append(tl.LLFrags, llFrag{Msn: msn, Part: part, StartMs: f.DecodeMs, DurMs: f.DurationMs, Key: f.Keyframe})
		needAnchor := tl.WebRTC == nil
		tl.mu.Unlock()

		if f.Keyframe {
			keyMs = append(keyMs, f.DecodeMs)
		}
		if rtpKeys := rtc.keyframes(); needAnchor && len(keyMs) >= 3 && len(rtpKeys) >= 3 {
			a := &rtpAnchor{
				RTPTs: rtpKeys[1], MediaMs: keyMs[1],
				RTPGapMs:   float64(int32(rtpKeys[2]-rtpKeys[1])) / 90,
				MediaGapMs: keyMs[2] - keyMs[1],
			}
			tl.mu.Lock()
			tl.WebRTC = a
			tl.mu.Unlock()
			log.Printf("webrtc anchor: rtp %d = media %.1fms (keyframe gap rtp %.1fms, fmp4 %.1fms)", a.RTPTs, a.MediaMs, a.RTPGapMs, a.MediaGapMs)
		}
	}
	close(toRedis)
	<-redisDone
	ll.end()
	_ = store.SetMovieStatus(context.Background(), movieID, "ended")
	if err := <-errC; err != nil && ctx.Err() == nil {
		log.Printf("low-latency demux: %v", err)
	}
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

func serve(addr, webDir, assetsDir string, tl *timeline, fs *memFS, ll *llhls, rtc *rtcOrigin, shutdown context.CancelFunc) {
	mux := http.NewServeMux()
	mux.Handle("/out/llhls/", countRequests(tl, ll))
	mux.HandleFunc("/webrtc/whep", func(w http.ResponseWriter, r *http.Request) {
		tl.mu.Lock()
		tl.Requests["webrtc/signal"]++
		tl.RequestLog = append(tl.RequestLog, reqEntry{At: nowMs(), System: "webrtc", Kind: "signal"})
		tl.mu.Unlock()
		rtc.ServeHTTP(w, r)
	})

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
		if r.URL.Query().Get("lite") != "" {
			_ = json.NewEncoder(w).Encode(tl.lite())
			return
		}
		tl.Encoder = rtc.clock()
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
		case strings.Contains(rel, "/part"):
			kind = "part"
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
