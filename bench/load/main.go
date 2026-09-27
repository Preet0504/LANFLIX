// Command load measures our origin under N concurrent viewers. For each N
// it connects N WebSocket clients at the live edge of the benchmark
// stream, records when every client receives every new segment, and
// SHA-256-checks every received segment against what was published.
// Server CPU is sampled from the OS for the duration of each step.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type timeline struct {
	MovieID    string            `json:"movie_id"`
	GOPMs      int64             `json:"gop_ms"`
	Ours       map[string]int64  `json:"ours"`
	OursSHA256 map[string]string `json:"ours_sha256"`
}

type receipt struct {
	seq  int
	at   int64
	hash string
}

type stepResult struct {
	N            int       `json:"n"`
	DurationS    float64   `json:"duration_s"`
	DelaysMs     []float64 `json:"delays_ms"` // publish → received, every (client, live segment)
	P50, P95     float64
	P99, Max     float64
	Received     int     `json:"segments_received"`
	Expected     int     `json:"segments_expected"`  // published before the window closed
	Delivered    int     `json:"segments_delivered"` // of those, how many every client got
	Mismatches   int     `json:"hash_mismatches"`
	ConnectFails int     `json:"connect_failures"`
	ServerCPUPct float64 `json:"server_cpu_pct"` // of one core
	ClientCPUPct float64 `json:"client_cpu_pct"` // this load generator, of one core
	MbpsOut      float64 `json:"aggregate_mbps"` // bytes delivered to all clients / window
}

func getTimeline(u string) (timeline, error) {
	var t timeline
	r, err := http.Get(u)
	if err != nil {
		return t, err
	}
	defer r.Body.Close()
	return t, json.NewDecoder(r.Body).Decode(&t)
}

func edge(t timeline) int {
	m := -1
	for k := range t.Ours {
		if n, _ := strconv.Atoi(k); n > m {
			m = n
		}
	}
	return m
}

func main() {
	server := flag.String("server", "localhost:8090", "our server")
	tlURL := flag.String("timeline", "http://localhost:8095/timeline", "benchmark origin timeline")
	steps := flag.String("n", "1,10,50,100,200", "concurrent viewer counts")
	dur := flag.Duration("dur", 20*time.Second, "measurement window per step")
	pid := flag.Int("pid", 0, "server process id, for CPU sampling")
	out := flag.String("out", "bench/results/load.json", "results file")
	flag.Parse()

	var results []stepResult
	for _, s := range strings.Split(*steps, ",") {
		n, _ := strconv.Atoi(strings.TrimSpace(s))
		res, err := runStep(*server, *tlURL, n, *dur, *pid)
		if err != nil {
			log.Fatalf("n=%d: %v", n, err)
		}
		log.Printf("n=%-4d delay p50=%.1fms p95=%.1fms p99=%.1fms max=%.1fms  delivered %d/%d  hash mismatches=%d  server cpu=%.0f%%  client cpu=%.0f%%  %.0f Mbps",
			n, res.P50, res.P95, res.P99, res.Max, res.Delivered, res.Expected, res.Mismatches, res.ServerCPUPct, res.ClientCPUPct, res.MbpsOut)
		results = append(results, res)
		time.Sleep(3 * time.Second)
	}
	b, _ := json.Marshal(results)
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", *out)
}

func runStep(server, tlURL string, n int, dur time.Duration, pid int) (stepResult, error) {
	tl, err := getTimeline(tlURL)
	if err != nil {
		return stepResult{}, err
	}
	joinSeq := edge(tl)
	if joinSeq < 0 {
		return stepResult{}, fmt.Errorf("no segments published yet")
	}

	var mu sync.Mutex
	perClient := make([][]receipt, n)
	var fails int
	var wg sync.WaitGroup
	stop := make(chan struct{})

	cpuStart, selfStart, wallStart := cpuTime(pid), cpuTime(os.Getpid()), time.Now()
	var bytesIn int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u := url.URL{Scheme: "ws", Host: server, Path: "/ws", RawQuery: url.Values{
				"movie": {tl.MovieID}, "client_id": {fmt.Sprintf("load-%d-%d", n, i)},
				"from_ms": {strconv.FormatInt(int64(joinSeq)*tl.GOPMs, 10)},
			}.Encode()}
			c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
			if err != nil {
				mu.Lock()
				fails++
				mu.Unlock()
				return
			}
			go func() { <-stop; c.Close() }()
			for {
				_, data, err := c.ReadMessage()
				if err != nil {
					return
				}
				at := time.Now().UnixMilli()
				mu.Lock()
				bytesIn += int64(len(data))
				mu.Unlock()
				// A media segment starts moof → mfhd; sequence_number (1-based) at offset 20.
				if len(data) < 24 || string(data[4:8]) != "moof" {
					continue
				}
				sum := sha256.Sum256(data)
				r := receipt{seq: int(binary.BigEndian.Uint32(data[20:24])) - 1, at: at, hash: hex.EncodeToString(sum[:])}
				mu.Lock()
				perClient[i] = append(perClient[i], r)
				mu.Unlock()
			}
		}(i)
	}
	time.Sleep(dur)
	stopAt := time.Now().UnixMilli()
	close(stop)
	wg.Wait()
	wall := time.Since(wallStart)
	cpuUsed := cpuTime(pid) - cpuStart
	selfUsed := cpuTime(os.Getpid()) - selfStart

	time.Sleep(500 * time.Millisecond)
	tl, err = getTimeline(tlURL) // final publish times + hashes
	if err != nil {
		return stepResult{}, err
	}
	// Expected = segments published before the window closed, with a small
	// margin for one in flight at that instant; one published after the
	// clients disconnected was never deliverable and isn't a loss.
	lastSeq := joinSeq
	for k, pub := range tl.Ours {
		if n, _ := strconv.Atoi(k); n > lastSeq && pub <= stopAt-250 {
			lastSeq = n
		}
	}

	res := stepResult{N: n, DurationS: wall.Seconds(), ConnectFails: fails}
	if pid > 0 {
		res.ServerCPUPct = 100 * cpuUsed.Seconds() / wall.Seconds()
	}
	res.ClientCPUPct = 100 * selfUsed.Seconds() / wall.Seconds()
	res.MbpsOut = float64(bytesIn) * 8 / 1e6 / wall.Seconds()
	for _, rs := range perClient {
		for _, r := range rs {
			key := strconv.Itoa(r.seq)
			if want, ok := tl.OursSHA256[key]; ok && want != r.hash {
				res.Mismatches++
			}
			res.Received++
			// Delay only for segments published *after* the client joined
			// (live tail); the join segment was published before connect.
			if r.seq > joinSeq {
				if pub, ok := tl.Ours[key]; ok {
					res.DelaysMs = append(res.DelaysMs, float64(r.at-pub))
				}
			}
		}
	}
	// Every connected client should have every segment from join to the
	// last one published before the window closed.
	res.Expected = (n - fails) * (lastSeq - joinSeq + 1)
	delivered := 0
	for _, rs := range perClient {
		for _, r := range rs {
			if r.seq <= lastSeq {
				delivered++
			}
		}
	}
	res.Delivered = delivered
	sorted := append([]float64(nil), res.DelaysMs...)
	sort.Float64s(sorted)
	pct := func(p float64) float64 {
		if len(sorted) == 0 {
			return 0
		}
		return sorted[int(p*float64(len(sorted)-1))]
	}
	res.P50, res.P95, res.P99, res.Max = pct(.50), pct(.95), pct(.99), pct(1)
	return res, nil
}
