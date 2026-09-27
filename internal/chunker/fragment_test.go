package chunker

import (
	"bytes"
	"context"
	"math"
	"os/exec"
	"testing"
)

// Encodes the 6s A/V sample into ~200ms fragments with a keyframe every
// 2s — the low-latency benchmark's settings — and checks the parsed
// timeline: contiguous, keyframes exactly on GOP boundaries, and nothing
// but GOP-boundary fragments marked decodable.
func TestDemuxFragmentsTimeline(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	out, err := exec.Command("ffmpeg", "-nostats", "-loglevel", "error",
		"-i", "../../testdata/sample_av.mp4",
		"-c:v", "libx264", "-preset", "veryfast", "-bf", "0",
		"-force_key_frames", "expr:gte(t,n_forced*2)", "-sc_threshold", "0",
		"-c:a", "aac", "-f", "mp4",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-frag_duration", "200000",
		"pipe:1").Output()
	if err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}

	initC := make(chan []byte, 1)
	fragC := make(chan Fragment, 1024)
	if err := DemuxFragments(context.Background(), bytes.NewReader(out), initC, fragC); err != nil {
		t.Fatal(err)
	}
	close(fragC)
	if len(<-initC) == 0 {
		t.Fatal("empty init segment")
	}

	var frags []Fragment
	for f := range fragC {
		frags = append(frags, f)
	}
	if len(frags) < 20 {
		t.Fatalf("got %d fragments for 6s at ~200ms, want ~30", len(frags))
	}
	var keyAt []float64
	next := 0.0
	for i, f := range frags {
		if math.Abs(f.DecodeMs-next) > 1 {
			t.Errorf("fragment %d starts at %.1fms, previous ended at %.1fms", i, f.DecodeMs, next)
		}
		if f.DurationMs <= 0 || f.DurationMs > 400 {
			t.Errorf("fragment %d duration %.1fms", i, f.DurationMs)
		}
		next = f.DecodeMs + f.DurationMs
		if f.Keyframe {
			keyAt = append(keyAt, f.DecodeMs)
		}
	}
	// Keyframes are forced at source times 0, 2, 4s. The sample's video
	// starts ~23ms after its audio (AAC priming), which the muxer folds
	// into the first frame, so absolute positions are offset — the
	// invariants are: decodable from the first fragment, then exactly one
	// GOP apart, and nowhere else.
	if len(keyAt) != 3 || keyAt[0] != 0 || !frags[0].Keyframe {
		t.Fatalf("keyframe fragments at %v, want 3 starting at fragment 0", keyAt)
	}
	for i := 2; i < len(keyAt); i++ {
		if gap := keyAt[i] - keyAt[i-1]; math.Abs(gap-2000) > 1 {
			t.Errorf("keyframes %v: gap %.1fms, want 2000", keyAt, gap)
		}
	}
}
