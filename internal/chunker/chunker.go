// Package chunker drives ffmpeg to turn a source video into a shared MP4
// initialization segment (codec config, sent once) plus a sequence of
// GOP-aligned fragmented-MP4 media segments (moof+mdat only, no embedded
// moov). That split — not just the keyframe alignment — is what MSE
// (browser video decoding) actually requires: appending a segment with
// its own moov re-initializes the decoder instead of continuing it.
//
// ffmpeg's own HLS/DASH muxers can produce this split as separate files,
// but their file-naming behavior turned out to be inconsistent across
// invocation styles and codec combinations on this platform (confirmed
// by direct testing — the same arguments that worked for video-only
// silently failed to write the init segment once an audio track was
// added, depending on exact path style, with no consistent pattern).
// Rather than chase that further, we sidestep it entirely: ffmpeg writes
// one continuous fragmented-MP4 stream to stdout
// (-movflags frag_keyframe+empty_moov+default_base_moof), and we parse
// the MP4 box structure ourselves to split it into an init segment
// (everything before the first moof) and media segments (each moof+mdat
// pair, one per forced keyframe). No files, no polling, no ffmpeg
// path/naming quirks to depend on.
package chunker

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
)

type Config struct {
	FFmpegPath string // defaults to "ffmpeg" (resolved via PATH)
	InputPath  string
	GOPSeconds int  // keyframe interval == fragment (segment) boundary
	Realtime   bool // -re: pace reads at source frame rate, simulating a live encode
}

type Segment struct {
	Seq       int64
	PTSMillis int64
	Data      []byte
}

// Run launches ffmpeg and streams results on the returned channels:
// the init segment exactly once on initCh, then media segments in order
// on segments, closing both when ffmpeg exits. Errors are sent on errs;
// at most one error is sent before all channels close.
func Run(ctx context.Context, cfg Config) (initCh <-chan []byte, segments <-chan Segment, errs <-chan error) {
	initC := make(chan []byte, 1)
	segC := make(chan Segment)
	errC := make(chan error, 1)

	go func() {
		defer close(initC)
		defer close(segC)
		defer close(errC)

		ffmpegPath := cfg.FFmpegPath
		if ffmpegPath == "" {
			ffmpegPath = "ffmpeg"
		}
		gop := cfg.GOPSeconds
		if gop <= 0 {
			gop = 2
		}

		// -nostats: ffmpeg's progress meter writes ~220 bytes/sec to stderr
		// forever. Warnings and errors still come through.
		args := []string{"-nostats", "-loglevel", "warning"}
		if cfg.Realtime {
			args = append(args, "-re")
		}
		args = append(args,
			"-i", cfg.InputPath,
			"-map", "0:v:0",
			"-map", "0:a:0?", // "?": don't fail if the source has no audio track
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", gop),
			// No scene-cut keyframes: frag_keyframe starts a fragment at every
			// keyframe, and segment N is labeled N*gop seconds — an extra
			// keyframe at a hard cut would split a fragment and shift every
			// later label. Standard for fixed-duration segmenting.
			"-sc_threshold", "0",
			"-c:a", "aac",
			"-b:a", "128k",
			"-f", "mp4",
			"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
			"pipe:1",
		)

		cmd := exec.CommandContext(ctx, ffmpegPath, args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			errC <- fmt.Errorf("stdout pipe: %w", err)
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			errC <- fmt.Errorf("stderr pipe: %w", err)
			return
		}
		if err := cmd.Start(); err != nil {
			errC <- fmt.Errorf("start ffmpeg: %w", err)
			return
		}

		// stderr must be drained continuously for as long as ffmpeg runs: if
		// the reader ever stops, the OS pipe fills and ffmpeg blocks on its
		// next write — freezing encoding with no error anywhere. This was a
		// bufio.Scanner, which splits on '\n'; ffmpeg's progress meter uses
		// '\r' only, so the whole meter was one ever-growing "line", Scanner
		// gave up at its 64KB limit, and every stream froze after roughly
		// 5-8 minutes of wall time. io.Copy has no line concept and never
		// stops early; tailBuffer keeps just the end for error reports.
		stderrTail := &tailBuffer{max: 8192}
		stderrDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(stderrTail, stderr)
			close(stderrDone)
		}()

		demuxErr := Demux(ctx, stdout, gop, initC, segC)
		if demuxErr != nil && demuxErr != io.EOF {
			_ = cmd.Process.Kill() // stop producing so stderr reaches EOF
		}

		// All reads from the pipes must finish before Wait, which closes them.
		<-stderrDone
		waitErr := cmd.Wait()
		tail := stderrTail.String()
		if demuxErr != nil && demuxErr != io.EOF {
			errC <- fmt.Errorf("demux: %w\n%s", demuxErr, tail)
			return
		}
		if waitErr != nil {
			errC <- fmt.Errorf("ffmpeg exited: %w\n%s", waitErr, tail)
		}
	}()

	return initC, segC, errC
}

// Demux reads a fragmented-MP4 stream box by box. Every box before the
// first "moof" (ftyp, moov, ...) is the init segment, sent once. Each
// "moof" immediately followed by its "mdat" is one media segment.
// Exported so tools that run their own ffmpeg (the benchmark harness)
// split output exactly the way production does.
func Demux(ctx context.Context, stdout io.Reader, gop int, initC chan<- []byte, segC chan<- Segment) error {
	r := bufio.NewReaderSize(stdout, 1<<20)

	var initBuf []byte
	var pendingMoof []byte
	sentInit := false
	seq := int64(0)

	for {
		boxType, raw, err := readBox(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		switch boxType {
		case "moof":
			pendingMoof = raw
		case "mdat":
			if !sentInit {
				select {
				case initC <- initBuf:
				case <-ctx.Done():
					return ctx.Err()
				}
				sentInit = true
			}
			if pendingMoof == nil {
				return fmt.Errorf("mdat box with no preceding moof")
			}
			data := make([]byte, 0, len(pendingMoof)+len(raw))
			data = append(data, pendingMoof...)
			data = append(data, raw...)
			select {
			case segC <- Segment{Seq: seq, PTSMillis: seq * int64(gop) * 1000, Data: data}:
			case <-ctx.Done():
				return ctx.Err()
			}
			seq++
			pendingMoof = nil
		default:
			// ftyp, moov, free, etc. Part of the init segment until the
			// first moof; ignored if they show up after (e.g. a trailing
			// mfra index box some muxers append at the very end).
			if !sentInit {
				initBuf = append(initBuf, raw...)
			}
		}
	}
}

// tailBuffer is an io.Writer that retains only the last max bytes.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// readBox reads one top-level MP4 box — the 8-byte size+type header plus
// its full body — and returns its type and raw bytes (header included).
func readBox(r io.Reader) (boxType string, raw []byte, err error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil {
		return "", nil, err // io.EOF here means the stream ended cleanly
	}
	size := uint64(binary.BigEndian.Uint32(header[0:4]))
	boxType = string(header[4:8])

	if size == 1 {
		// 64-bit "largesize" extension follows the type.
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return "", nil, fmt.Errorf("read largesize: %w", err)
		}
		size = binary.BigEndian.Uint64(ext)
		if size < 16 {
			return "", nil, fmt.Errorf("box %q: largesize %d smaller than header", boxType, size)
		}
		body := make([]byte, size-16)
		if _, err := io.ReadFull(r, body); err != nil {
			return "", nil, fmt.Errorf("read box %q body: %w", boxType, err)
		}
		raw = make([]byte, 0, len(header)+len(ext)+len(body))
		raw = append(raw, header...)
		raw = append(raw, ext...)
		raw = append(raw, body...)
		return boxType, raw, nil
	}
	if size < 8 {
		return "", nil, fmt.Errorf("box %q has invalid size %d", boxType, size)
	}
	body := make([]byte, size-8)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", nil, fmt.Errorf("read box %q body: %w", boxType, err)
	}
	raw = make([]byte, 0, len(header)+len(body))
	raw = append(raw, header...)
	raw = append(raw, body...)
	return boxType, raw, nil
}
