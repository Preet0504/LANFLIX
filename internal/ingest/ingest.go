// Package ingest turns a video file into a published movie: register it,
// encode+chunk it, publish the init segment and every chunk. It's the
// shared core behind both the CLI producer and the server's browser
// upload endpoint, so a host streaming from a file picker gets exactly
// the same pipeline as the original CLI tool.
package ingest

import (
	"context"
	"fmt"
	"time"

	"streaming/internal/chunker"
	"streaming/internal/redisstream"
)

type Options struct {
	MovieID    string
	RoomID     string
	Title      string
	InputPath  string
	GOPSeconds int
	Realtime   bool
	FFmpegPath string
	// ChunkMs is the published chunk duration: 0 for one chunk per GOP,
	// or e.g. 200 for low-latency mode (see chunker.Config.FragmentMs).
	ChunkMs int
}

// Run registers the movie, then encodes and publishes it start to
// finish, updating its status in the registry as it goes. It blocks
// until the source is fully consumed or ctx is cancelled — call it in
// its own goroutine for a live upload.
func Run(ctx context.Context, store *redisstream.Store, opts Options) error {
	if err := store.RegisterMovie(ctx, opts.MovieID, opts.RoomID, opts.Title); err != nil {
		return fmt.Errorf("register movie: %w", err)
	}
	if opts.ChunkMs > 0 {
		if err := store.SetChunkMs(ctx, opts.MovieID, int64(opts.ChunkMs)); err != nil {
			return err
		}
	}

	// Refresh well inside the heartbeat's TTL so a slow Redis round trip
	// can't let it lapse while we're still encoding.
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			_ = store.Heartbeat(hbCtx, opts.MovieID)
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
			}
		}
	}()

	initCh, segments, errs := chunker.Run(ctx, chunker.Config{
		FFmpegPath: opts.FFmpegPath,
		InputPath:  opts.InputPath,
		GOPSeconds: opts.GOPSeconds,
		Realtime:   opts.Realtime,
		FragmentMs: opts.ChunkMs,
	})

	initData, ok := <-initCh
	if !ok {
		_ = store.SetMovieStatus(ctx, opts.MovieID, "ended")
		if err := <-errs; err != nil {
			return fmt.Errorf("chunker failed before producing an init segment: %w", err)
		}
		return fmt.Errorf("chunker closed before producing an init segment")
	}
	if err := store.SetInit(ctx, opts.MovieID, initData); err != nil {
		_ = store.SetMovieStatus(ctx, opts.MovieID, "ended")
		return fmt.Errorf("publish init segment: %w", err)
	}
	if err := store.SetMovieStatus(ctx, opts.MovieID, "live"); err != nil {
		return fmt.Errorf("set movie live: %w", err)
	}

	for seg := range segments {
		if _, err := store.PublishChunk(ctx, opts.MovieID, redisstream.Chunk{
			Seq:       seg.Seq,
			PTSMillis: seg.PTSMillis,
			Keyframe:  seg.Keyframe,
			Data:      seg.Data,
		}); err != nil {
			_ = store.SetMovieStatus(ctx, opts.MovieID, "ended")
			return fmt.Errorf("publish chunk seq=%d: %w", seg.Seq, err)
		}
	}

	_ = store.SetMovieStatus(ctx, opts.MovieID, "ended")
	if err := <-errs; err != nil {
		return fmt.Errorf("chunker: %w", err)
	}
	return nil
}
