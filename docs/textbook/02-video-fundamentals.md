# Chapter 2 — How digital video works

You can't design a video system without knowing what video *is* at the byte level. The central design decision in LANFLIX, how to cut video into pieces, is forced by how compression works. This chapter builds that knowledge from zero.

## 2.1 Frames and the size problem

A video is a sequence of still images, called **frames**, shown quickly enough that the eye sees motion. **Frame rate** is how many per second: 24 for film, 30 or 60 for most screens. Each frame is a grid of **pixels**. At 720p the grid is 1280 × 720, or 921,600 pixels.

A pixel's color is stored as numbers. Video usually uses **YUV** rather than RGB: one brightness value (Y, "luma") and two color-difference values (U and V, "chroma"). The eye is much less sensitive to color detail than to brightness detail, so most video keeps full-resolution brightness but quarter-resolution color. This is called **4:2:0 chroma subsampling**, and it averages to 1.5 bytes per pixel.

Now compute the size of *uncompressed* 720p video at 30 frames per second:

```
1280 × 720 pixels × 1.5 bytes × 30 frames/s  ≈  41.5 MB per second  ≈  330 Mb/s
```

A single viewer would need 330 megabits per second, more than most Wi-Fi networks can carry in total. Yet the same video, compressed, streams comfortably at about 3 Mb/s. That's a reduction of about 100×, and where it comes from matters, because it dictates how video can be cut up.

## 2.2 Compression: codecs

A **codec** (coder–decoder) is the algorithm that compresses and decompresses video. LANFLIX uses **H.264**, also called AVC. It's the most widely supported video codec in existence: every browser, phone and GPU can decode it, usually in dedicated hardware. Newer codecs (H.265/HEVC, VP9, AV1) compress better but aren't universally supported. For a zero-install system, H.264's universality wins.

H.264 exploits two kinds of redundancy:

- **Spatial redundancy**, within a frame. Neighboring pixels are usually similar: a blue sky is mostly blue. The codec divides the frame into blocks, predicts each block from its neighbors, and stores only the difference. It then transforms the difference into frequencies and discards detail the eye won't miss. That last part is the "lossy" step.
- **Temporal redundancy**, between frames. Consecutive frames are usually nearly identical: in a news broadcast only the presenter's mouth moves. Instead of storing each frame fully, the codec stores *how blocks moved* since a previous frame (motion vectors) plus a small correction.

Temporal compression is where most of the saving comes from, and it creates the most important concept in this chapter: **frames that depend on other frames**.

## 2.3 Frame types, keyframes and the GOP

H.264 has three kinds of frames:

| Type | Name | Stores | Can be decoded… |
|---|---|---|---|
| **I** | Intra frame | A complete picture, compressed only spatially | On its own |
| **P** | Predicted frame | Differences from an *earlier* frame | Only if the earlier frame was decoded |
| **B** | Bi-directional frame | Differences from an earlier *and a later* frame | Only if both were decoded |

A special kind of I-frame, the **IDR frame** (instantaneous decoder refresh), guarantees that no frame after it refers to any frame before it. It's a clean starting point. In streaming, "**keyframe**" means an IDR frame: **the only kind of frame a decoder can start from.**

The frames from one keyframe up to (not including) the next form a **GOP**, a group of pictures:

```
 keyframe                                      keyframe
    │                                             │
    ▼                                             ▼
  ┌───┬───┬───┬───┬───┬───┬─── ··· ───┬───┬───┐ ┌───┬───┬───
  │ I │ P │ B │ B │ P │ B │    ···     │ P │ B │ │ I │ P │ B  ···
  └───┴───┴───┴───┴───┴───┴─── ··· ───┴───┴───┘ └───┴───┴───
  └──────────────── one GOP (2 s = 60 frames) ────────────────┘
```

I-frames are large (often 5–10× a P-frame), so a long GOP compresses better. But a decoder can only *start* at a keyframe. That means the GOP length decides:

- **how precisely a viewer can seek.** A seek to 0:13.4 must start decoding at the keyframe at or before it. With a 2-second GOP that's at most 2 seconds of extra decoding.
- **how long a joining viewer waits.** A live viewer can only begin at a keyframe.
- **how finely the stream can be cut into independently playable pieces.** A piece that doesn't start with a keyframe can't be played on its own.

Streaming systems pick a short, **fixed** GOP, typically 1–4 seconds. LANFLIX uses **2 seconds**. We force a keyframe exactly every 2 seconds (`-force_key_frames "expr:gte(t,n_forced*2)"`), and we disable scene-cut detection (`-sc_threshold 0`), which would otherwise insert extra keyframes at hard cuts in the picture. Fixed, predictable keyframe positions are what allow chunk *n* to be labeled "the video from 2n to 2n+2 seconds".

## 2.4 Decode order, display order, and timestamps

B-frames reference a *later* frame, so the decoder must receive that later frame first. The order frames are **decoded** in therefore differs from the order they're **displayed** in:

```
display order:   I0  B1  B2  P3  B4  B5  P6
decode order:    I0  P3  B1  B2  P6  B4  B5
```

Every frame therefore carries two timestamps:

- **DTS**, the decode timestamp: when to decode it.
- **PTS**, the presentation timestamp: when to show it.

Timestamps are integers counted in a **timescale**, a number of ticks per second chosen by the file. For example, with a timescale of 15,360, one frame at 30 fps lasts 512 ticks. Integers avoid floating-point drift over hours of video.

Low-latency systems often **disable B-frames** (`-bf 0`), so that decode order equals display order and every frame can be shown as soon as it's decoded. LANFLIX does this in low-latency mode. WebRTC (chapter 4) doesn't support B-frames at all.

## 2.5 Audio in one page

Audio is compressed separately, usually with **AAC**, and sampled 44,100 or 48,000 times per second. AAC works in frames of 1,024 samples, about 21–23 ms each. Audio and video are separate **tracks** that play in sync because each piece of each track carries a timestamp on the same clock. One detail shows up later: AAC encoders add a short silent "priming" period at the start, so a file's video often starts a few tens of milliseconds after its audio clock does. Players handle this through the timestamps; code that assumes "segment *n* starts at exactly 2n seconds" must allow for it.

WebRTC requires the **Opus** audio codec instead of AAC. Chapter 13 shows why that matters when comparing systems fairly.

## 2.6 Containers: MP4 and its boxes

A codec produces compressed frames. A **container** packages frames from all tracks together, with timing and the information a decoder needs to start. LANFLIX uses **MP4** (ISO Base Media File Format), because every browser can play it.

An MP4 file is a sequence of **boxes** (also called atoms). Each box begins with an 8-byte header, a 4-byte size and a 4-character type, followed by its contents. Some boxes contain other boxes. A classic MP4 looks like this:

```
┌──────┐┌──────────────────────────────────────┐┌─────────────────────────┐
│ ftyp ││ moov                                 ││ mdat                    │
│      ││  ├─ mvhd  (overall timing)            ││ all the compressed      │
│ file ││  ├─ trak  (video track)               ││ frames of every track,  │
│ type ││  │   ├─ tkhd  (track id)              ││ back to back            │
│      ││  │   └─ mdia                          ││                         │
│      ││  │       ├─ mdhd (timescale)          ││                         │
│      ││  │       ├─ hdlr ("vide")             ││                         │
│      ││  │       └─ minf/stbl                 ││                         │
│      ││  │            ├─ stsd  (codec config: ││                         │
│      ││  │            │         avc1 → avcC)  ││                         │
│      ││  │            └─ sample tables: where ││                         │
│      ││  │               every frame is, its  ││                         │
│      ││  │               size and timestamp   ││                         │
│      ││  └─ trak  (audio track) …             ││                         │
└──────┘└──────────────────────────────────────┘└─────────────────────────┘
```

The **`moov`** box holds all the metadata, including a table listing the position, size and timestamp of *every frame in the file*. That's fine for a finished file. It's impossible for a live stream: you can't write the table of every frame before those frames exist.

## 2.7 Fragmented MP4: video in pieces

**Fragmented MP4** (fMP4) solves this. Instead of one big frame table, the file becomes:

```
┌──────┬──────┐┌──────┬──────┐┌──────┬──────┐┌──────┬──────┐
│ ftyp │ moov ││ moof │ mdat ││ moof │ mdat ││ moof │ mdat │  ···
└──────┴──────┘└──────┴──────┘└──────┴──────┘└──────┴──────┘
  init segment    fragment 1     fragment 2     fragment 3
 (sent once)     (0.0–2.0 s)    (2.0–4.0 s)    (4.0–6.0 s)
```

- The **init segment** (`ftyp` + `moov`) describes the tracks and codecs but lists no frames. Inside `moov`, an `mvex/trex` box announces "the frames come later, in fragments" and gives default values for them. A player needs the init segment **once**, before any media.
- Each **fragment** is a `moof` (movie fragment) box followed by an `mdat` (media data) box holding that fragment's compressed frames. The `moof` contains, per track, a `traf` box with:
  - `tfhd`: which track, plus default frame duration and flags;
  - `tfdt`: the **decode time of the fragment's first frame**, its position on the timeline;
  - `trun`: the list of frames in this fragment, each with its size, duration and flags. One flag says whether the frame is a keyframe ("sync sample").

A fragment is self-describing: it carries its own timing and frame table. It can be produced the moment its frames are encoded, stored, sent and appended to a player **on its own**, as long as the player has the init segment and the fragment starts with a keyframe (or the player already has the frames before it).

This is the foundation of every modern streaming system. HLS, DASH, LL-HLS and LANFLIX all deliver fMP4 fragments. They differ only in *how the fragments travel*, which is the subject of chapter 4. The standardized profile of this format used across HLS and DASH is called **CMAF** (Common Media Application Format).

**How big should a fragment be?** It's the same trade-off as the GOP, one level up:

| Fragment length | Upside | Downside |
|---|---|---|
| One GOP (2 s) | Every fragment starts with a keyframe and plays on its own; few, large messages | A fragment can't be sent until all 2 s of it are encoded, adding up to 2 s of delay |
| Sub-GOP (200 ms) | A fragment is ready 200 ms after its first frame, so it can be sent almost immediately | Most fragments start mid-GOP and can't be played alone; 10× as many messages |

LANFLIX supports both, as its standard and low-latency modes. The storage and delivery design in chapters 7 and 8 must handle fragments that can't be played alone, which is why keyframe flags matter.

## 2.8 Codec strings

A browser must be told what's inside a stream *before* it receives it, as a MIME type with a **codec string**:

```
video/mp4; codecs="avc1.64001f,mp4a.40.2"
```

`avc1.64001f` means H.264 with profile `0x64` (High), constraint flags `0x00`, and level `0x1f` (3.1). `mp4a.40.2` means AAC-LC audio. These values aren't guesses: they're stored in the init segment, in the `avcC` box (bytes 1–3 are profile, constraints and level). Reading them from the stream rather than hard-coding them means the player describes each stream accurately, whether it's 720p or 4K, with or without audio.

## 2.9 Encoding with ffmpeg

**ffmpeg** is the standard open-source tool for decoding, encoding and packaging media. LANFLIX drives it as a subprocess. The core command, simplified:

```bash
ffmpeg -re -i input.mkv \
  -map 0:v:0 -map 0:a:0? \
  -c:v libx264 -preset veryfast \
  -force_key_frames "expr:gte(t,n_forced*2)" -sc_threshold 0 \
  -c:a aac -b:a 128k \
  -f mp4 -movflags +frag_keyframe+empty_moov+default_base_moof \
  pipe:1
```

| Part | Meaning |
|---|---|
| `-re` | Read the input at its native frame rate, so a 90-minute file takes 90 minutes. This turns a file into a *live broadcast*. |
| `-map 0:v:0 -map 0:a:0?` | Take the first video track and the first audio track, if there is one (`?`). |
| `-c:v libx264 -preset veryfast` | Encode H.264 with the x264 encoder at a fast speed/quality setting, fast enough to keep up in real time on a laptop. |
| `-force_key_frames …` `-sc_threshold 0` | A keyframe every 2 s exactly, and nowhere else. |
| `-c:a aac -b:a 128k` | AAC audio at 128 kb/s. |
| `-movflags +frag_keyframe+empty_moov+default_base_moof` | Fragmented MP4: an init segment with an empty frame table, then a new fragment at every keyframe, using the timing layout MSE expects. |
| `pipe:1` | Write to standard output, a stream of bytes, rather than a file. |

In low-latency mode, two more options are added: `-frag_duration 200000` (also start a fragment every 200,000 µs) and `-bf 0` (no B-frames).

Why re-encode at all, instead of sending the uploaded file's bytes as they are? Because uploaded files are arbitrary: any codec, any GOP structure, keyframes wherever the camera felt like putting them. Re-encoding **normalizes** every input into the one shape the rest of the system relies on: H.264, a keyframe every 2 seconds, AAC, fragmented. Normalizing at the boundary so the core can make strong assumptions is a design principle that recurs throughout this book.

## Check your understanding

1. Why can't a player start decoding from a P-frame? What would it be missing?
2. A viewer seeks to 1:05.3 in a stream with a 2 s GOP. From which point must decoding start, and how much video is decoded but never shown?
3. Why does a normal MP4 file not work for a live stream, and which box change fixes it?
4. What does a 200 ms fragment gain over a 2 s fragment, and what capability does it lose?
5. Why does LANFLIX re-encode uploads instead of streaming the original file's bytes?
