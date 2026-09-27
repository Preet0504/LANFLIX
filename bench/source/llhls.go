package main

import (
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"streaming/internal/chunker"
)

// llhls is a Low-Latency HLS origin (RFC 8216bis / Apple LL-HLS) built on
// the same 200ms fMP4 fragments the LANFLIX-LL stream publishes to Redis:
// each fragment is one partial segment, a keyframe fragment starts a new
// segment. It implements what hls.js uses in low-latency mode — partial
// segments, blocking playlist reload (_HLS_msn/_HLS_part), preload hints
// served by blocking until the part exists, and delta playlist updates
// (_HLS_skip) so reloads don't re-send the whole EVENT playlist 5x/s.
//
// Parts are named by their global fragment number (part<seq>.mp4) so the
// benchmark page can attribute each download to the fragment it contains.
type llhls struct {
	mu      sync.Mutex
	changed chan struct{} // closed and replaced on every new part
	init    []byte
	segs    []*llSeg
	ended   bool
}

type llSeg struct {
	parts    []llPart
	complete bool
}

type llPart struct {
	seq   int64
	dur   float64 // seconds
	indep bool
	data  []byte
}

const (
	partTarget   = 0.2 // ffmpeg -frag_duration; parts are exactly 6 frames at 30fps
	partHoldBack = 0.6 // 3x part target, Apple's recommendation
	partWindow   = 3   // recent segments that still list their parts
	skipUntil    = 12.0
	blockTimeout = 6 * time.Second // 3x target duration
)

func newLLHLS() *llhls { return &llhls{changed: make(chan struct{})} }

func (l *llhls) setInit(b []byte) {
	l.mu.Lock()
	l.init = b
	l.mu.Unlock()
}

func (l *llhls) add(f chunker.Fragment) (msn, part int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f.Keyframe || len(l.segs) == 0 {
		if n := len(l.segs); n > 0 {
			l.segs[n-1].complete = true
		}
		l.segs = append(l.segs, &llSeg{})
	}
	s := l.segs[len(l.segs)-1]
	s.parts = append(s.parts, llPart{seq: f.Seq, dur: f.DurationMs / 1000, indep: f.Keyframe, data: f.Data})
	close(l.changed)
	l.changed = make(chan struct{})
	return len(l.segs) - 1, len(s.parts) - 1
}

func (l *llhls) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := len(l.segs); n > 0 {
		l.segs[n-1].complete = true
	}
	l.ended = true
	close(l.changed)
	l.changed = make(chan struct{})
}

// waitFor blocks until ready() holds (checked under the lock) or timeout.
func (l *llhls) waitFor(r *http.Request, ready func() bool) bool {
	deadline := time.After(blockTimeout)
	for {
		l.mu.Lock()
		ok, ch := ready(), l.changed
		l.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-ch:
		case <-deadline:
			return false
		case <-r.Context().Done():
			return false
		}
	}
}

// hasPart reports whether the playlist already contains segment msn, part
// p (p < 0: the whole segment). Caller holds the lock.
func (l *llhls) hasPart(msn, p int) bool {
	if msn < len(l.segs)-1 || (msn == len(l.segs)-1 && l.segs[msn].complete) {
		return true
	}
	if msn == len(l.segs)-1 && p >= 0 {
		return p < len(l.segs[msn].parts)
	}
	return false
}

func (l *llhls) playlist(skip bool) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:9\n#EXT-X-TARGETDURATION:2\n#EXT-X-PLAYLIST-TYPE:EVENT\n")
	fmt.Fprintf(&b, "#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=%.3f,CAN-SKIP-UNTIL=%.1f\n", partHoldBack, skipUntil)
	fmt.Fprintf(&b, "#EXT-X-PART-INF:PART-TARGET=%.3f\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-MAP:URI=\"init.mp4\"\n", partTarget)

	// Delta update: skip complete segments that end more than skipUntil
	// before the end of the playlist.
	first := 0
	if skip {
		total := 0.0
		for _, s := range l.segs {
			total += segDur(s)
		}
		acc := 0.0
		for first < len(l.segs) && l.segs[first].complete && total-(acc+segDur(l.segs[first])) > skipUntil {
			acc += segDur(l.segs[first])
			first++
		}
		if first > 0 {
			fmt.Fprintf(&b, "#EXT-X-SKIP:SKIPPED-SEGMENTS=%d\n", first)
		}
	}
	for i := first; i < len(l.segs); i++ {
		s := l.segs[i]
		if i >= len(l.segs)-partWindow {
			for _, p := range s.parts {
				fmt.Fprintf(&b, "#EXT-X-PART:DURATION=%.5f,URI=\"part%d.mp4\"", p.dur, p.seq)
				if p.indep {
					b.WriteString(",INDEPENDENT=YES")
				}
				b.WriteString("\n")
			}
		}
		if s.complete {
			fmt.Fprintf(&b, "#EXTINF:%.5f,\nseg%d.mp4\n", segDur(s), i)
		}
	}
	if l.ended {
		b.WriteString("#EXT-X-ENDLIST\n")
	} else if n := len(l.segs); n > 0 {
		last := l.segs[n-1].parts
		fmt.Fprintf(&b, "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"part%d.mp4\"\n", last[len(last)-1].seq+1)
	}
	return b.String()
}

func segDur(s *llSeg) float64 {
	d := 0.0
	for _, p := range s.parts {
		d += p.dur
	}
	return d
}

// findPart locates a part by its global fragment number. Caller holds the lock.
func (l *llhls) findPart(seq int64) ([]byte, bool) {
	for i := len(l.segs) - 1; i >= 0; i-- {
		ps := l.segs[i].parts
		if len(ps) == 0 || ps[0].seq > seq {
			continue
		}
		if k := seq - ps[0].seq; k < int64(len(ps)) {
			return ps[k].data, true
		}
		return nil, false
	}
	return nil, false
}

func (l *llhls) lastSeq() int64 {
	if n := len(l.segs); n > 0 {
		ps := l.segs[n-1].parts
		return ps[len(ps)-1].seq
	}
	return -1
}

func (l *llhls) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	switch {
	case name == "index.m3u8":
		q := r.URL.Query()
		if v := q.Get("_HLS_msn"); v != "" {
			msn, err := strconv.Atoi(v)
			if err != nil {
				http.Error(w, "bad _HLS_msn", http.StatusBadRequest)
				return
			}
			p := -1
			if v := q.Get("_HLS_part"); v != "" {
				if p, err = strconv.Atoi(v); err != nil {
					http.Error(w, "bad _HLS_part", http.StatusBadRequest)
					return
				}
			}
			l.mu.Lock()
			tooFar := msn > len(l.segs)+1
			l.mu.Unlock()
			if tooFar {
				http.Error(w, "_HLS_msn too far in the future", http.StatusBadRequest)
				return
			}
			l.waitFor(r, func() bool { return l.ended || l.hasPart(msn, p) })
		}
		l.mu.Lock()
		body := l.playlist(q.Get("_HLS_skip") == "YES" || q.Get("_HLS_skip") == "v2")
		l.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(body))

	case name == "init.mp4":
		l.mu.Lock()
		b := l.init
		l.mu.Unlock()
		if b == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(b)

	case strings.HasPrefix(name, "part"):
		seq, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(name, "part"), ".mp4"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		// A preload-hinted part is requested before it exists: hold the
		// request open and answer the moment it's produced.
		var data []byte
		l.waitFor(r, func() bool {
			var ok bool
			data, ok = l.findPart(seq)
			return ok || l.ended || seq > l.lastSeq()+1
		})
		if data == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(data)

	case strings.HasPrefix(name, "seg"):
		msn, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "seg"), ".mp4"))
		l.mu.Lock()
		var parts []llPart
		if err == nil && msn >= 0 && msn < len(l.segs) && l.segs[msn].complete {
			parts = l.segs[msn].parts
		}
		l.mu.Unlock()
		if parts == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		for _, p := range parts {
			_, _ = w.Write(p.data)
		}

	default:
		http.NotFound(w, r)
	}
}
