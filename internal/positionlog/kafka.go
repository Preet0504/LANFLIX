// Package positionlog carries "which client is watching which movie at
// which timestamp" events through Kafka. Unlike the video-byte path
// (Redis Streams), this genuinely benefits from Kafka: write volume
// scales with concurrent viewer count, and it decouples the producer
// (the delivery server) from consumers that don't exist yet (this
// package ships with one — the dashboard — but analytics, "resume
// where you left off", etc. could subscribe independently later).
package positionlog

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/segmentio/kafka-go"
)

const Topic = "viewer-positions"

type Event struct {
	ClientID        string `json:"client_id"`
	MovieID         string `json:"movie_id"`
	PositionMillis  int64  `json:"position_ms"`
	EventTimeUnixMs int64  `json:"event_time_ms"`
}

type Producer struct {
	w *kafka.Writer
}

func NewProducer(brokers []string) *Producer {
	return &Producer{w: &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Topic:        Topic,
		Balancer:     &kafka.LeastBytes{},
		RequiredAcks: kafka.RequireOne,
		Async:        true, // a dropped position ping isn't worth blocking playback for
	}}
}

func (p *Producer) Publish(ctx context.Context, e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal position event: %w", err)
	}
	return p.w.WriteMessages(ctx, kafka.Message{
		Key:   []byte(e.ClientID),
		Value: data,
		Time:  time.UnixMilli(e.EventTimeUnixMs),
	})
}

func (p *Producer) Close() error { return p.w.Close() }

type Consumer struct {
	r *kafka.Reader
}

func NewConsumer(brokers []string, groupID string) *Consumer {
	return &Consumer{r: kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers,
		GroupID: groupID,
		Topic:   Topic,
	})}
}

func (c *Consumer) Close() error { return c.r.Close() }

// Run calls handler for every event until ctx is cancelled. Read errors
// (e.g. "Not Coordinator For Group", which single-broker KRaft setups can
// return transiently while a group/topic is still settling) are logged
// and retried with a short backoff rather than treated as fatal.
func (c *Consumer) Run(ctx context.Context, handler func(Event)) error {
	for {
		m, err := c.r.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("positionlog: read error, retrying: %v", err)
			select {
			case <-time.After(500 * time.Millisecond):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var e Event
		if err := json.Unmarshal(m.Value, &e); err != nil {
			continue // skip malformed
		}
		handler(e)
	}
}
