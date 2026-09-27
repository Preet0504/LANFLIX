// Command udp measures what raw UDP streaming (MPEG-TS, the classic
// multicast-style approach) does to a video under packet loss. For each
// loss rate it streams the same clip over UDP to a receiver that drops a
// controlled fraction of datagrams (Bernoulli, seeded), then counts the
// damage: MPEG-TS continuity-counter breaks, decoder errors, and video
// frames that no longer decode.
//
// Loss is simulated at the receiver because the benchmark host (Windows,
// no admin) has no netem equivalent. The same limitation means TCP-based
// systems (ours, HLS, DASH) are NOT put under loss here; TCP retransmits
// by design, so for them loss becomes delay, never corruption.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type result struct {
	LossPct         float64 `json:"loss_pct"`
	Datagrams       int     `json:"datagrams"`
	Dropped         int     `json:"dropped"`
	TSPackets       int     `json:"ts_packets_received"`
	CCErrors        int     `json:"continuity_errors"`
	DecodeErrors    int     `json:"decode_errors"`
	FramesDecoded   int     `json:"frames_decoded"`
	FramesExpected  int     `json:"frames_expected"`
	FrameLossPct    float64 `json:"frame_loss_pct"`
}

func main() {
	input := flag.String("input", "bench/media/source.mp4", "clip to stream")
	secs := flag.Int("secs", 60, "seconds of video per run")
	losses := flag.String("losses", "0,0.1,0.5,1,2,5", "simulated datagram loss rates, percent")
	dir := flag.String("dir", "bench/out/udp", "received streams")
	out := flag.String("out", "bench/results/udp.json", "results file")
	flag.Parse()

	_ = os.MkdirAll(*dir, 0o755)
	var rates []float64
	for _, s := range strings.Split(*losses, ",") {
		v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
		rates = append(rates, v)
	}

	// Reference: frames in the same clip received with no loss at all.
	ref := filepath.Join(*dir, "reference.ts")
	if err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-t", strconv.Itoa(*secs), "-i", *input,
		"-c", "copy", "-f", "mpegts", ref).Run(); err != nil {
		log.Fatalf("reference: %v", err)
	}
	expected := countFrames(ref)

	results := make([]result, len(rates))
	var wg sync.WaitGroup
	for i, p := range rates {
		wg.Add(1)
		go func(i int, p float64) {
			defer wg.Done()
			r, err := run(*input, *secs, p, 17000+i, filepath.Join(*dir, fmt.Sprintf("recv_%g.ts", p)), int64(1000+i))
			if err != nil {
				log.Printf("loss %g%%: %v", p, err)
				return
			}
			r.FramesExpected = expected
			if expected > 0 {
				r.FrameLossPct = 100 * float64(expected-r.FramesDecoded) / float64(expected)
			}
			results[i] = r
			log.Printf("loss %4g%%: dropped %d/%d datagrams, CC errors %d, decode errors %d, frames %d/%d (%.1f%% lost)",
				p, r.Dropped, r.Datagrams, r.CCErrors, r.DecodeErrors, r.FramesDecoded, expected, r.FrameLossPct)
		}(i, p)
	}
	wg.Wait()
	b, _ := json.Marshal(results)
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", *out)
}

func run(input string, secs int, lossPct float64, port int, recvPath string, seed int64) (result, error) {
	res := result{LossPct: lossPct}
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		return res, err
	}
	defer pc.Close()
	_ = pc.SetReadBuffer(16 << 20) // so the OS itself drops nothing on loopback

	f, err := os.Create(recvPath)
	if err != nil {
		return res, err
	}
	defer f.Close()

	sender := exec.Command("ffmpeg", "-loglevel", "error", "-re", "-t", strconv.Itoa(secs), "-i", input,
		"-c", "copy", "-f", "mpegts", fmt.Sprintf("udp://127.0.0.1:%d?pkt_size=1316", port))
	if err := sender.Start(); err != nil {
		return res, err
	}
	senderDone := make(chan struct{})
	go func() { _ = sender.Wait(); close(senderDone) }()

	rng := rand.New(rand.NewSource(seed))
	lastCC := map[uint16]int{}
	buf := make([]byte, 65536)
	for {
		_ = pc.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		n, _, err := pc.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-senderDone:
				goto done // sender finished and the socket went quiet
			default:
				continue
			}
		}
		res.Datagrams++
		if rng.Float64()*100 < lossPct {
			res.Dropped++
			continue
		}
		_, _ = f.Write(buf[:n])
		for off := 0; off+188 <= n; off += 188 {
			pkt := buf[off : off+188]
			if pkt[0] != 0x47 {
				continue
			}
			res.TSPackets++
			pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
			if pid == 0x1fff {
				continue // null packets carry no continuity counter
			}
			hasPayload := pkt[3]&0x10 != 0
			cc := int(pkt[3] & 0x0f)
			if prev, ok := lastCC[pid]; ok && hasPayload && cc != (prev+1)&0x0f && cc != prev {
				res.CCErrors++
			}
			if hasPayload {
				lastCC[pid] = cc
			}
		}
	}
done:
	f.Close()
	res.DecodeErrors = countDecodeErrors(recvPath)
	res.FramesDecoded = countFrames(recvPath)
	return res, nil
}

func countFrames(path string) int {
	b, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-count_frames",
		"-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(strings.Split(string(b), "\n")[0]))
	return n
}

func countDecodeErrors(path string) int {
	b, _ := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
	count := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}
