package chunker

import (
	"context"
	"os"
	"testing"
	"time"
)

// When CHUNKER_FAKE_FFMPEG is set, this test binary impersonates ffmpeg:
// Run launches it (via Config.FFmpegPath) exactly as it would the real one.
func TestMain(m *testing.M) {
	if os.Getenv("CHUNKER_FAKE_FFMPEG") == "1" {
		fakeFFmpegProgressFlood()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Real ffmpeg's progress meter separates updates with '\r' and never
// writes a newline, at ~220 bytes/sec for as long as encoding runs. 2MB
// is ~2.5 hours of that — far past any pipe buffer.
func fakeFFmpegProgressFlood() {
	line := []byte("frame=  123 fps= 30 q=29.0 size=       1KiB time=00:00:04.00 bitrate=   2.0kbits/s speed=1.00x    \r")
	for written := 0; written < 2<<20; written += len(line) {
		if _, err := os.Stderr.Write(line); err != nil {
			return
		}
	}
}

// Regression: stderr was drained with bufio.Scanner, which stops at a
// 64KB "line". With a '\r'-only progress meter the drain quit, the pipe
// filled, and ffmpeg blocked forever — every stream froze after ~5-8
// minutes of wall time.
func TestRunDoesNotStallWhenFFmpegFloodsStderr(t *testing.T) {
	t.Setenv("CHUNKER_FAKE_FFMPEG", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := time.Now()
	_, segments, errs := Run(ctx, Config{FFmpegPath: os.Args[0], InputPath: "unused"})
	for range segments {
	}
	err := <-errs
	if ctx.Err() != nil {
		t.Fatalf("Run stalled until the %s deadline instead of finishing (err=%v)", 15*time.Second, err)
	}
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	t.Logf("drained 2MB of progress output and finished in %s", time.Since(start).Round(time.Millisecond))
}
