package main

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"

	"streaming/internal/redisstream"
)

// hub fans each movie's live chunks out to every viewer from ONE Redis
// tail per movie. Before it, every viewer ran its own blocking XREAD,
// each holding a connection from go-redis's pool (10 per core — 80 on the
// benchmark host). Past 80 viewers the rest queued for a connection, so a
// new segment reached viewers in waves: median delivery went from ~5ms at
// 50 viewers to ~160-210ms at 200, with Redis pinned at 80 blocked clients.
// Now Redis work scales with movies, not viewers.
type hub struct {
	store  *redisstream.Store
	mu     sync.Mutex
	movies map[string]*movieFeed
}

type movieFeed struct {
	subs   map[*subscription]struct{}
	cancel context.CancelFunc
}

type liveChunk struct {
	id   string
	pts  int64
	data []byte
}

type subscription struct {
	ch chan liveChunk
	// lagged is closed if this viewer fell too far behind (its buffer
	// filled) or the shared tail failed; it then reads Redis directly
	// from its own last position, so nothing is ever silently skipped.
	lagged     chan struct{}
	laggedOnce sync.Once
}

func (s *subscription) markLagged() { s.laggedOnce.Do(func() { close(s.lagged) }) }

// subBuffer is how many segments (2s each) a viewer may trail the live
// feed by before being moved to its own tail.
const subBuffer = 64

func newHub(store *redisstream.Store) *hub {
	return &hub{store: store, movies: map[string]*movieFeed{}}
}

// subscribe returns a subscription receiving every chunk published after
// the moment it returns. A viewer must subscribe *before* backfilling, so
// the backfill and the live feed overlap instead of leaving a gap.
func (h *hub) subscribe(movieID string) (*subscription, func(), error) {
	sub := &subscription{ch: make(chan liveChunk, subBuffer), lagged: make(chan struct{})}

	h.mu.Lock()
	feed, ok := h.movies[movieID]
	if !ok {
		// Start tailing from the newest entry that exists right now; anything
		// at or before it is covered by the subscriber's own backfill.
		startID, err := h.store.LatestID(context.Background(), movieID)
		if err != nil {
			h.mu.Unlock()
			return nil, nil, err
		}
		ctx, cancel := context.WithCancel(context.Background())
		feed = &movieFeed{subs: map[*subscription]struct{}{}, cancel: cancel}
		h.movies[movieID] = feed
		go h.run(ctx, movieID, startID, feed)
	}
	feed.subs[sub] = struct{}{}
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(feed.subs, sub)
		if len(feed.subs) == 0 && h.movies[movieID] == feed {
			feed.cancel()
			delete(h.movies, movieID)
		}
	}
	return sub, unsubscribe, nil
}

func (h *hub) run(ctx context.Context, movieID, startID string, feed *movieFeed) {
	err := h.store.Tail(ctx, movieID, startID, func(id string, c redisstream.Chunk) error {
		lc := liveChunk{id: id, pts: c.PTSMillis, data: c.Data} // one buffer shared by every viewer
		h.mu.Lock()
		for sub := range feed.subs {
			select {
			case sub.ch <- lc:
			default:
				sub.markLagged() // never let one slow viewer stall the rest
			}
		}
		h.mu.Unlock()
		return nil
	})
	if ctx.Err() != nil {
		return // last viewer left
	}
	log.Printf("hub tail movie=%s stopped: %v; viewers fall back to direct reads", movieID, err)
	h.mu.Lock()
	for sub := range feed.subs {
		sub.markLagged()
	}
	if h.movies[movieID] == feed {
		delete(h.movies, movieID)
	}
	h.mu.Unlock()
}

// idAfter reports whether stream ID a ("ms-seq") comes after b.
func idAfter(a, b string) bool {
	am, as := splitID(a)
	bm, bs := splitID(b)
	return am > bm || (am == bm && as > bs)
}

func splitID(id string) (uint64, uint64) {
	ms, seq, _ := strings.Cut(id, "-")
	m, _ := strconv.ParseUint(ms, 10, 64)
	s, _ := strconv.ParseUint(seq, 10, 64)
	return m, s
}
