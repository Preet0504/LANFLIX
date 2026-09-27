package chunker

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
)

// Fragment is one moof+mdat pair plus the timing a low-latency packager
// needs. Demux labels each segment by its position (segment n starts at
// n*gop), which only holds when every fragment is exactly one GOP. With
// sub-GOP fragments (ffmpeg -frag_duration) the timing has to come from
// the fragment itself, and most fragments can't be decoded on their own.
type Fragment struct {
	Seq        int64
	DecodeMs   float64 // video track's baseMediaDecodeTime (tfdt)
	DurationMs float64 // sum of the fragment's video sample durations
	Keyframe   bool    // first video sample is a sync sample: a decoder can start here
	Data       []byte
}

type track struct {
	timescale uint32
	video     bool
	defDur    uint32 // trex defaults, used when tfhd/trun don't override
	defFlags  uint32
}

// DemuxFragments is Demux for sub-GOP fragments: same box splitting, but
// every fragment is described from its own moof rather than its position.
func DemuxFragments(ctx context.Context, r io.Reader, initC chan<- []byte, fragC chan<- Fragment) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var initBuf, pendingMoof []byte
	var tracks map[uint32]*track
	seq := int64(0)

	for {
		boxType, raw, err := readBox(br)
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
			if pendingMoof == nil {
				return fmt.Errorf("mdat box with no preceding moof")
			}
			if tracks == nil {
				if tracks, err = parseInit(initBuf); err != nil {
					return err
				}
				select {
				case initC <- initBuf:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			f, err := describeFragment(tracks, pendingMoof)
			if err != nil {
				return fmt.Errorf("fragment %d: %w", seq, err)
			}
			f.Seq = seq
			f.Data = append(append(make([]byte, 0, len(pendingMoof)+len(raw)), pendingMoof...), raw...)
			select {
			case fragC <- f:
			case <-ctx.Done():
				return ctx.Err()
			}
			seq++
			pendingMoof = nil
		default:
			if tracks == nil {
				initBuf = append(initBuf, raw...)
			}
		}
	}
}

// eachBox calls fn for every box directly inside b (a box payload).
func eachBox(b []byte, fn func(typ string, payload []byte) error) error {
	for len(b) >= 8 {
		size := uint64(binary.BigEndian.Uint32(b[0:4]))
		typ := string(b[4:8])
		hdr := uint64(8)
		if size == 1 {
			if len(b) < 16 {
				return fmt.Errorf("truncated largesize box %q", typ)
			}
			size, hdr = binary.BigEndian.Uint64(b[8:16]), 16
		} else if size == 0 {
			size = uint64(len(b))
		}
		if size < hdr || size > uint64(len(b)) {
			return fmt.Errorf("box %q: bad size %d", typ, size)
		}
		if err := fn(typ, b[hdr:size]); err != nil {
			return err
		}
		b = b[size:]
	}
	return nil
}

func u32(b []byte, off int) (uint32, error) {
	if off+4 > len(b) {
		return 0, fmt.Errorf("truncated box")
	}
	return binary.BigEndian.Uint32(b[off:]), nil
}

// parseInit reads each track's timescale and type (mdhd, hdlr) and its
// fragment defaults (trex) from the init segment.
func parseInit(init []byte) (map[uint32]*track, error) {
	tracks := map[uint32]*track{}
	var trex [][]byte
	err := eachBox(init, func(typ string, p []byte) error {
		if typ != "moov" {
			return nil
		}
		return eachBox(p, func(typ string, p []byte) error {
			switch typ {
			case "mvex":
				return eachBox(p, func(typ string, p []byte) error {
					if typ == "trex" {
						trex = append(trex, p)
					}
					return nil
				})
			case "trak":
				t := &track{}
				var id uint32
				err := eachBox(p, func(typ string, p []byte) error {
					switch typ {
					case "tkhd":
						off := 12 // version 0: flags, ctime, mtime, then track_ID
						if len(p) > 0 && p[0] == 1 {
							off = 20
						}
						var err error
						id, err = u32(p, off)
						return err
					case "mdia":
						return eachBox(p, func(typ string, p []byte) error {
							var err error
							switch typ {
							case "mdhd":
								off := 12
								if len(p) > 0 && p[0] == 1 {
									off = 20
								}
								t.timescale, err = u32(p, off)
							case "hdlr":
								if len(p) >= 12 {
									t.video = string(p[8:12]) == "vide"
								}
							}
							return err
						})
					}
					return nil
				})
				if err != nil {
					return err
				}
				tracks[id] = t
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("parse init: %w", err)
	}
	for _, p := range trex {
		id, err := u32(p, 4)
		if err != nil {
			return nil, err
		}
		if t := tracks[id]; t != nil {
			t.defDur, _ = u32(p, 12)
			t.defFlags, _ = u32(p, 20)
		}
	}
	for _, t := range tracks {
		if t.video && t.timescale > 0 {
			return tracks, nil
		}
	}
	return nil, fmt.Errorf("parse init: no video track")
}

// describeFragment reads the video traf of a moof: start (tfdt), total
// duration and first-sample sync flag (trun, falling back to tfhd, then
// trex defaults — ISO/IEC 14496-12 §8.8).
func describeFragment(tracks map[uint32]*track, moof []byte) (Fragment, error) {
	var f Fragment
	found := false
	err := eachBox(moof[8:], func(typ string, p []byte) error {
		if typ != "traf" {
			return nil
		}
		var t *track
		var tfhdFlags, tfhdDur, tfhdSampleFlags uint32
		var decode uint64
		var dur uint64
		firstFlags, haveFirst := uint32(0), false
		err := eachBox(p, func(typ string, p []byte) error {
			if len(p) < 4 {
				return fmt.Errorf("truncated %s", typ)
			}
			flags := binary.BigEndian.Uint32(p[0:4]) & 0xffffff
			switch typ {
			case "tfhd":
				id, err := u32(p, 4)
				if err != nil {
					return err
				}
				if t = tracks[id]; t == nil || !t.video {
					t = nil
					return nil
				}
				tfhdFlags = flags
				off := 8
				if flags&0x01 != 0 {
					off += 8 // base_data_offset
				}
				if flags&0x02 != 0 {
					off += 4 // sample_description_index
				}
				if flags&0x08 != 0 {
					tfhdDur, _ = u32(p, off)
					off += 4
				}
				if flags&0x10 != 0 {
					off += 4 // default_sample_size
				}
				if flags&0x20 != 0 {
					tfhdSampleFlags, _ = u32(p, off)
				}
			case "tfdt":
				if t == nil {
					return nil
				}
				if p[0] == 1 {
					if len(p) < 12 {
						return fmt.Errorf("truncated tfdt")
					}
					decode = binary.BigEndian.Uint64(p[4:12])
				} else {
					v, err := u32(p, 4)
					if err != nil {
						return err
					}
					decode = uint64(v)
				}
			case "trun":
				if t == nil {
					return nil
				}
				count, err := u32(p, 4)
				if err != nil {
					return err
				}
				off := 8
				if flags&0x01 != 0 {
					off += 4 // data_offset
				}
				if flags&0x04 != 0 {
					v, err := u32(p, off)
					if err != nil {
						return err
					}
					if !haveFirst {
						firstFlags, haveFirst = v, true
					}
					off += 4
				}
				defDur := t.defDur
				if tfhdFlags&0x08 != 0 {
					defDur = tfhdDur
				}
				for i := uint32(0); i < count; i++ {
					d := defDur
					if flags&0x100 != 0 {
						if d, err = u32(p, off); err != nil {
							return err
						}
						off += 4
					}
					if flags&0x200 != 0 {
						off += 4
					}
					if flags&0x400 != 0 {
						v, err := u32(p, off)
						if err != nil {
							return err
						}
						if i == 0 && !haveFirst {
							firstFlags, haveFirst = v, true
						}
						off += 4
					}
					if flags&0x800 != 0 {
						off += 4
					}
					dur += uint64(d)
				}
			}
			return nil
		})
		if err != nil || t == nil {
			return err
		}
		if !haveFirst {
			firstFlags = t.defFlags
			if tfhdFlags&0x20 != 0 {
				firstFlags = tfhdSampleFlags
			}
		}
		ts := float64(t.timescale)
		f.DecodeMs = float64(decode) * 1000 / ts
		f.DurationMs = float64(dur) * 1000 / ts
		f.Keyframe = firstFlags&0x00010000 == 0 // sample_is_non_sync_sample clear
		found = true
		return nil
	})
	if err != nil {
		return f, err
	}
	if !found {
		return f, fmt.Errorf("no video track in fragment")
	}
	return f, nil
}
