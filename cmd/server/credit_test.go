package main

import (
	"context"
	"testing"
	"time"
)

func waitsFor(c *credit, pts int64, d time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return c.wait(ctx, pts) != nil
}

func TestCreditWithoutWindowNeverBlocks(t *testing.T) {
	c := newCredit(0, 0) // a client that never grants: the old behavior
	if waitsFor(c, 1<<50, 50*time.Millisecond) {
		t.Fatal("unlimited credit blocked")
	}
	c.reset(0) // no-ops without a window
	if waitsFor(c, 1<<50, 50*time.Millisecond) {
		t.Fatal("reset imposed a window on an unlimited connection")
	}
}

func TestCreditWindow(t *testing.T) {
	c := newCredit(30_000, 100_000) // joined at 100s with a 30s window

	if waitsFor(c, 130_000, 50*time.Millisecond) {
		t.Fatal("chunk at the window's edge blocked")
	}
	if !waitsFor(c, 132_000, 50*time.Millisecond) {
		t.Fatal("chunk past the window was sent")
	}

	// A grant releases a chunk that is already waiting.
	done := make(chan error, 1)
	go func() { done <- c.wait(context.Background(), 132_000) }()
	time.Sleep(20 * time.Millisecond)
	c.grant(135_000)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("grant didn't release the waiting chunk")
	}

	// Seek back to 10s: the window restarts there.
	c.reset(10_000)
	if !waitsFor(c, 60_000, 50*time.Millisecond) {
		t.Fatal("after seeking back, a chunk beyond the new window was sent")
	}
	// A stale grant from before the seek can't shrink it below the reset...
	c.grant(20_000)
	if waitsFor(c, 40_000, 50*time.Millisecond) {
		t.Fatal("a stale grant shrank the window")
	}
	// ...and grants still extend it.
	c.grant(70_000)
	if waitsFor(c, 60_000, 50*time.Millisecond) {
		t.Fatal("grant after the seek didn't extend the window")
	}
}
