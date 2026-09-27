package main

import (
	"context"
	"math"
	"sync"
)

// credit is per-connection flow control. The player grants the server a
// window of media ahead of its playhead ({"until_ms": ...}, sent with its
// position pings); the server writes a chunk only once its timestamp is
// inside the window, and otherwise waits for the player to catch up.
//
// Without it, push had no brakes: after a seek the server sent everything
// from the seek point to the live edge as fast as the socket allowed. The
// benchmark measured ~5x more media downloaded per session than HLS, and
// the flood caused two player bugs (a buffer-quota retry loop and a frozen
// seek). Clients that never grant — older players, the load generator —
// open without a window and keep the old unlimited behavior.
type credit struct {
	mu      sync.Mutex
	untilMs int64
	window  int64 // 0: unlimited
	changed chan struct{}
}

func newCredit(windowMs, fromMs int64) *credit {
	c := &credit{window: windowMs, changed: make(chan struct{})}
	c.untilMs = math.MaxInt64
	if windowMs > 0 {
		c.untilMs = fromMs + windowMs
	}
	return c
}

func (c *credit) notify() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// grant extends the window to untilMs. Grants only ever extend it: a ping
// sent just before a seek can't shrink the window reset() gave the seek.
func (c *credit) grant(untilMs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.window > 0 && untilMs > c.untilMs {
		c.untilMs = untilMs
		c.notify()
	}
}

// reset starts a fresh window at a seek target, in either direction.
func (c *credit) reset(fromMs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.window > 0 {
		c.untilMs = fromMs + c.window
		c.notify()
	}
}

// wait blocks until a chunk starting at ptsMs is inside the window.
func (c *credit) wait(ctx context.Context, ptsMs int64) error {
	for {
		c.mu.Lock()
		ok, ch := ptsMs <= c.untilMs, c.changed
		c.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
