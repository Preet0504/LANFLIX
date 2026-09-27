// producer encodes a source video into GOP-aligned segments and publishes
// them to a Redis Stream, simulating "client 1" broadcasting a live watch.
// It's a thin CLI wrapper around internal/ingest — the same path the
// server's browser upload endpoint uses.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"streaming/internal/ingest"
	"streaming/internal/redisstream"
)

func main() {
	input := flag.String("input", "", "path to source video file")
	movieID := flag.String("movie", "demo", "movie/stream ID")
	roomID := flag.String("room", "cli", "room ID this stream belongs to")
	title := flag.String("title", "", "display title (defaults to movie ID)")
	redisAddr := flag.String("redis", "localhost:6379", "redis address")
	gop := flag.Int("gop", 2, "segment duration / keyframe interval, seconds")
	realtime := flag.Bool("realtime", true, "pace encoding at source frame rate (simulates a live broadcast)")
	flag.Parse()

	if *input == "" {
		log.Fatal("-input is required")
	}
	displayTitle := *title
	if displayTitle == "" {
		displayTitle = *movieID
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store := redisstream.NewStore(*redisAddr)
	defer store.Close()

	err := ingest.Run(ctx, store, ingest.Options{
		MovieID:    *movieID,
		RoomID:     *roomID,
		Title:      displayTitle,
		InputPath:  *input,
		GOPSeconds: *gop,
		Realtime:   *realtime,
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Println("done")
}
