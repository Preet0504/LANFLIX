// wsclient is a smoke-test client for the WebSocket bridge server: it
// connects, optionally sends a seek, and logs every binary frame it
// receives. Proves the server's backfill/tail/seek behavior without
// needing a browser.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "server host:port")
	movieID := flag.String("movie", "demo", "movie/stream ID")
	fromMillis := flag.Int64("from-ms", 0, "initial join offset, milliseconds")
	seekAfter := flag.Duration("seek-after", 0, "if set, send a seek this long after connecting")
	seekTo := flag.Int64("seek-to-ms", 0, "offset to seek to, if -seek-after is set")
	flag.Parse()

	u := url.URL{Scheme: "ws", Host: *addr, Path: "/ws",
		RawQuery: url.Values{"movie": {*movieID}, "from_ms": {strconv.FormatInt(*fromMillis, 10)}}.Encode()}

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	log.Printf("connected to %s", u.String())

	if *seekAfter > 0 {
		go func() {
			time.Sleep(*seekAfter)
			msg, _ := json.Marshal(struct {
				SeekMillis int64 `json:"seek_ms"`
			}{*seekTo})
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				log.Printf("send seek: %v", err)
				return
			}
			log.Printf("sent seek to %dms", *seekTo)
		}()
	}

	start := time.Now()
	first := true
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("closed: %v", err)
			return
		}
		if first {
			log.Printf("time-to-first-frame: %s", time.Since(start))
			first = false
		}
		log.Printf("frame: %d bytes", len(data))
	}
}
