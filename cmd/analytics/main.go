// analytics is the "mini-Keystone": a Kafka consumer, in its own
// consumer group, completely independent of the delivery server and the
// live dashboard, that replays the viewer-positions topic to build
// durable engagement stats — total distinct viewers per movie, and how
// far each one got. This is the concrete case for keeping Kafka on this
// path at all: the answer to "how many people have ever watched this"
// can only come from processing the full event history, and a plain
// Redis resume point (which only ever holds the latest value) can never
// answer it. Adding this service required touching neither the server
// nor the dashboard — it just subscribes to the same topic they already
// produce to.
package main

import (
	"context"
	"flag"
	"log"

	"streaming/internal/positionlog"
	"streaming/internal/redisstream"
)

func main() {
	redisAddr := flag.String("redis", "localhost:6379", "redis address")
	kafkaAddr := flag.String("kafka", "localhost:9092", "kafka broker address")
	flag.Parse()

	store := redisstream.NewStore(*redisAddr)
	defer store.Close()

	consumer := positionlog.NewConsumer([]string{*kafkaAddr}, "analytics")
	defer consumer.Close()

	log.Println("analytics consumer running")
	err := consumer.Run(context.Background(), func(e positionlog.Event) {
		if err := store.UpdateReach(context.Background(), e.MovieID, e.ClientID, e.PositionMillis); err != nil {
			log.Printf("update reach movie=%s client=%s: %v", e.MovieID, e.ClientID, err)
		}
	})
	if err != nil {
		log.Fatal(err)
	}
}
