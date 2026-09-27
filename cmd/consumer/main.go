// consumer is a smoke-test client: it backfills from a given offset (the
// "late joiner" path) then live-tails new chunks (the "already caught up"
// path), printing what it receives. No decoding/playback — this just
// proves the publish/backfill/tail mechanics work end to end.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"streaming/internal/redisstream"
)

func main() {
	movieID := flag.String("movie", "demo", "movie/stream ID")
	redisAddr := flag.String("redis", "localhost:6379", "redis address")
	fromMillis := flag.Int64("from-ms", 0, "backfill starting at this PTS offset, in milliseconds")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store := redisstream.NewStore(*redisAddr)
	defer store.Close()

	chunks, lastID, err := store.RangeFrom(ctx, *movieID, *fromMillis)
	if err != nil {
		log.Fatalf("backfill: %v", err)
	}
	for _, c := range chunks {
		log.Printf("[backfill] seq=%d pts_ms=%d bytes=%d", c.Seq, c.PTSMillis, len(c.Data))
	}
	log.Printf("caught up (%d chunks), switching to live tail after id=%s", len(chunks), lastID)

	if lastID == "" {
		lastID = "$"
	}
	err = store.Tail(ctx, *movieID, lastID, func(id string, c redisstream.Chunk) error {
		log.Printf("[live]     seq=%d pts_ms=%d bytes=%d id=%s", c.Seq, c.PTSMillis, len(c.Data), id)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		log.Fatalf("tail: %v", err)
	}
}
