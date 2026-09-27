package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// After a seek, nothing written for the previous position may reach the
// client, and the seek's ack must precede the new position's media — the
// client relies on that order to know which bytes to drop.
func TestSeekEpochDropsStaleWrites(t *testing.T) {
	serverConn := make(chan *safeConn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		serverConn <- &safeConn{conn: c}
	}))
	defer srv.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sc := <-serverConn

	if err := sc.WriteBinary(0, []byte("init")); err != nil {
		t.Fatal(err)
	}
	first, err := sc.beginEpoch(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.WriteBinary(first, []byte("old position")); err != nil {
		t.Fatal(err)
	}
	second, err := sc.beginEpoch(90000) // the viewer seeks
	if err != nil {
		t.Fatal(err)
	}
	// The old stream's goroutine hasn't noticed yet and writes once more.
	if err := sc.WriteBinary(first, []byte("stale")); !errors.Is(err, errStaleEpoch) {
		t.Fatalf("stale write: got %v, want errStaleEpoch", err)
	}
	if err := sc.WriteBinary(second, []byte("new position")); err != nil {
		t.Fatal(err)
	}

	want := []string{"init", `{"seek_ack":0}`, "old position", `{"seek_ack":90000}`, "new position"}
	for _, w := range want {
		_, got, err := client.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != w {
			t.Fatalf("got message %q, want %q", got, w)
		}
	}
}
