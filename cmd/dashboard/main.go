// dashboard consumes viewer position events from Kafka and serves a live
// "who's watching what, and how far in" view. It's a deliberately
// separate process from the delivery server — proving out the actual
// reason Kafka sits on this path: producer and consumer are decoupled,
// so this could be swapped for an analytics pipeline or a "resume
// playback" service without touching the server that publishes events.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"sync"
	"time"

	"streaming/internal/positionlog"
)

type viewerState struct {
	MovieID        string `json:"movie_id"`
	PositionMillis int64  `json:"position_ms"`
	LastSeenUnixMs int64  `json:"last_seen_ms"`
}

type registry struct {
	mu      sync.Mutex
	viewers map[string]viewerState
}

func newRegistry() *registry {
	return &registry{viewers: make(map[string]viewerState)}
}

func (r *registry) update(e positionlog.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.viewers[e.ClientID] = viewerState{
		MovieID:        e.MovieID,
		PositionMillis: e.PositionMillis,
		LastSeenUnixMs: e.EventTimeUnixMs,
	}
}

// staleAfter prunes viewers whose last ping is older than this — the
// player pings every 2s, so anything past a few missed pings is gone.
const staleAfter = 10 * time.Second

func (r *registry) snapshot() map[string]viewerState {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-staleAfter).UnixMilli()
	out := make(map[string]viewerState, len(r.viewers))
	for id, v := range r.viewers {
		if v.LastSeenUnixMs < cutoff {
			continue
		}
		out[id] = v
	}
	return out
}

func main() {
	addr := flag.String("addr", ":8091", "http listen address")
	kafkaAddr := flag.String("kafka", "localhost:9092", "kafka broker address")
	flag.Parse()

	reg := newRegistry()
	consumer := positionlog.NewConsumer([]string{*kafkaAddr}, "dashboard")
	defer consumer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := consumer.Run(ctx, reg.update); err != nil && ctx.Err() == nil {
			log.Printf("kafka consumer: %v", err)
		}
	}()

	http.HandleFunc("/api/viewers", func(w http.ResponseWriter, r *http.Request) {
		// The host dashboard page is served from the main server's origin
		// (a different port), so this needs to be fetchable cross-origin.
		// Fine for a trusted-LAN tool with no auth to protect anyway.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reg.snapshot())
	})
	http.Handle("/", http.FileServer(http.Dir("web/dashboard")))

	log.Printf("dashboard listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
