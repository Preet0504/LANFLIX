# Chapter 13 — Measuring honestly

Designing a system is half the work. The other half is finding out whether it does what you think, and how it compares to the alternatives you rejected. This chapter describes how LANFLIX was benchmarked against HLS, LL-HLS, DASH, WebRTC and raw UDP: the harness, the metrics, the traps that make benchmarks lie, and what the results say. Benchmark design is itself a system design problem, and most of its lessons apply to any performance comparison.

## 13.1 What makes a comparison fair

A benchmark comparing delivery systems must change **only the delivery**. Everything else has to be identical: the video, the encoder, its settings, the machine, the browser, the network path, the time of day. Otherwise the result measures the difference you didn't intend.

The strongest control available here: **one encoder feeds every system at once.** A single ffmpeg process encodes the source in real time, and its `tee` output sends the *same encoded packets*, at the same instant, to:

| System | How it receives the encoder's output |
|---|---|
| **LANFLIX** (2 s chunks) | fMP4 on a pipe → LANFLIX's demuxer → Redis → the real LANFLIX server and player |
| **LANFLIX-LL** (200 ms chunks) | fMP4 over a local TCP connection → fragment demuxer → a second Redis stream → the same server and player |
| **HLS** / **HLS tuned** | fMP4 segments and a playlist uploaded over HTTP to an in-memory origin → hls.js |
| **LL-HLS** | The *same* 200 ms fragments as LANFLIX-LL, handed over at the same instant → an LL-HLS origin (parts, blocking reload, preload hints, delta updates) → hls.js in low-latency mode |
| **DASH** / **DASH tuned** | Segments and a manifest over HTTP → dash.js |
| **WebRTC** | The same H.264 as RTP packets, plus Opus audio → a pion relay → Chrome's WebRTC |
| **UDP** | MPEG-TS over raw UDP, with loss simulated at the receiver |

The encoder runs **without B-frames**, because WebRTC can't carry them and low-latency live encoders don't use them. It applies to every system equally.

Each competitor was measured **at its best**, not just with defaults. Default HLS keeps three segments of margin; a "tuned" configuration keeps one. DASH is tuned the same way. And the low-latency variants (LL-HLS, and LANFLIX-LL with the same chunk size and the same 0.6 s hold-back) were compared *with each other*. Comparing your best configuration against a competitor's default flatters you and teaches nothing.

> **Principle: compare against the strongest version of each alternative, with every uncontrolled variable held equal.** If the alternative has a low-latency mode, test it. If your advantage disappears, you've learned something true.

## 13.2 The metrics, defined precisely

A metric without a precise definition can be argued in any direction. Each was defined once and measured identically for every system:

| Metric | Definition | How it's measured |
|---|---|---|
| **Time to first frame (TTFF)** | From telling the player to start until the first frame is *presented on screen* | `requestVideoFrameCallback` presentation time (chapter 3.6) |
| **Glass-to-glass delay** | For every frame shown during steady playback, how long ago the encoder produced it | Presentation time − encoder output time of that frame |
| **New-media delivery** | From a chunk becoming available at its origin until its bytes reach the viewer | Origin logs availability; the page logs receipt |
| **Seek latency** | From issuing a seek until a frame near the target is presented | Frame callbacks |
| **Freezes** | Gaps longer than 250 ms between consecutive presented frames during steady playback | From the frame timestamps, so it's identical for every player |
| **Origin requests** | HTTP requests per viewer per minute | Counted at the origin |
| **Media downloaded** | Seconds of video delivered per session | Units received × unit duration |

"Available" means something different per system, and the definition has to be honest about that: LANFLIX, when the Redis append returns; HLS and DASH, when the segment appears in the manifest; LL-HLS, when the part can be served.

## 13.3 Clocks: knowing when a frame was produced

Glass-to-glass delay needs the moment each frame left the encoder. The encoder runs in real time, so the frame at video time *m* is produced at `encoder_start + m`. The whole metric depends on knowing `encoder_start` accurately.

The most direct signal available is the encoder's **RTP output**. RTP packets leave ffmpeg the moment each frame is encoded, with no muxing or segmenting in between. For each frame, `arrival_time − frame_timestamp` estimates `encoder_start`. Those estimates vary a little (some frames take longer to encode), so the **lower envelope** is the encoder's schedule. The 1st percentile over all ~55,000 frames is used rather than the absolute minimum. Where the looped test video restarts, ffmpeg's real-time pacing lets a few frames out early, and a plain minimum would lock onto those outliers. The distribution is tight: the median is only 29 ms above the 1st percentile.

WebRTC frames have no MP4 timeline, so each presented frame's RTP timestamp is mapped onto the shared timeline by aligning the same keyframe on both sides. The mapping is checked: the spacing between keyframes is exactly 2,000.0 ms in both.

And the calibration is **checked against the data**. No 200 ms fragment can become available before its last frame was encoded; if the clock were wrong, some would appear to. The 1st percentile of "available − last frame encoded" is **+34 ms**, which is physically plausible.

> **Principle: calibrate against the most direct signal available, use robust statistics for it, and check it with an invariant the data must satisfy.** Causality (effects after causes) is the cheapest sanity check in any timing measurement.

## 13.4 Randomize what you don't control

Latency in a segment-based system depends on *when in the segment cycle* the viewer happens to join. Join just after a segment appears and you start almost two seconds behind; join just before the next and you start almost four behind. If every session of a system joined at the same point in the cycle, that system's result would be biased by an accident of timing.

A benchmark that runs sessions back to back with fixed durations can do exactly that: if one round of all systems takes a near-constant time, each system joins at the same phase every trial. The harness therefore waits a **seeded random 0–2 s** before each session, spreading join points across the cycle, reproducibly.

Three more controls:

- **Interleaving.** Trials run round-robin: every system once, then every system again, in rotating order. Slow drift in machine load (a background task, thermal throttling) is then spread across all systems instead of landing on whichever ran last.
- **Fresh state.** Every session uses a fresh browser profile: no cache, no stored identity.
- **Medians.** Each system ran 10 trials, and results are reported as medians. They resist the occasional outlier better than means.

> **Principle: anything you don't control, randomize; anything that drifts, interleave.** These are the foundations of experimental design, and they apply to A/B tests and performance work as much as to science.

## 13.5 Results

Medians of 10 trials per configuration, measured on one laptop (Intel i5-1135G7, Windows 11, everything on loopback) with headless Chromium:

| Metric | **LANFLIX** | **LANFLIX-LL** | HLS | HLS tuned | LL-HLS | DASH | DASH tuned | WebRTC |
|---|---|---|---|---|---|---|---|---|
| Time to first frame | 64 ms | 85 ms | 224 ms | 214 ms | 233 ms | 214 ms | 218 ms | 1.40 s |
| Glass-to-glass delay | 3.11 s | 979 ms | 7.28 s | 2.87 s | 914 ms | 4.71 s | 3.93 s | 121 ms |
| New-media delivery, median / p95 | 7 / 30 ms | 3 / 4 ms | 808 ms / 1.65 s | 507 ms / 1.86 s | 7 / 11 ms | 750 ms / 1.73 s | 1.60 / 1.95 s | per packet |
| Seek into history | 31 ms | 25 ms | 19 ms | 20 ms | 22 ms | 31 ms | 33 ms | not possible |
| Freezes per 15 s | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 | 1.9 | 3.9 | 0.1 |
| HTTP requests / min | 1 per session | 1 per session | 247 | 240 | 717 | 786 | 196 | 1 per session |
| Media downloaded / session | 146 s | 141 s | 152 s | 148 s | 149 s | 234 s | 36 s | — |

![Glass-to-glass latency, log scale](../figures/latency.png)

## 13.6 What the results mean

**The latency budget predicted the ranking** (chapter 4.8). Glass-to-glass delay falls into three clear bands, set by **the size of the unit delivered** and **the safety margin**:

- **Packets (WebRTC): ~0.12 s.** No chunking; only a jitter buffer.
- **200 ms chunks with a 0.6 s margin (LANFLIX-LL, LL-HLS): ~0.9–1.0 s.**
- **2 s segments with a one-segment margin (LANFLIX, HLS tuned, DASH tuned): ~3 s.** Default HLS, which keeps three segments, sits at ~7 s.

Within a band, **push versus pull barely matters for latency.** LANFLIX and tuned HLS overlap at 2 s segments. LANFLIX-LL and LL-HLS are within 70 ms of each other, with LL-HLS slightly ahead. That's the most important lesson of the benchmark: *latency is decided by chunking and buffering policy, not by who initiates the transfer.*

**Where push does matter: delivery and startup.** Classic polling players learn about a new segment only on their next poll, so delivery takes 0.5–1.6 s, while LANFLIX pushes it in 7 ms. LL-HLS matches push (7 ms) by *emulating* it: blocking requests that the origin completes the moment a part exists. LANFLIX reaches the first frame in 64–85 ms against ~215–235 ms for the HTTP players, because one socket delivers the init segment and newest chunk together, where HTTP players first fetch a manifest.

**The cost of HTTP-based low latency is requests.** LL-HLS needs ~717 HTTP requests per viewer per minute (a blocking playlist request and a part request for every 200 ms). LANFLIX needs one connection per session. Across 200 viewers that's ~2,400 requests per second of pure overhead, avoided.

**Flow control makes push as frugal as pull.** Each viewer downloads about as much media per session as with HLS (146 s vs 152 s), because the server never sends more than 30 s past the playhead (chapter 8.6).

**Freezes are a policy trade-off.** dash.js tuned for low delay keeps a very short buffer: it downloads least (36 s per session) and freezes most (3.9 times per 15 s). A small buffer is a bet that the network never hiccups.

**WebRTC is in a different category.** It wins live latency by about 8×, and it's the right tool for interactive video. It can't rewind or resume, and a joining viewer waits for a keyframe (1.40 s here, because the shared encoder can't produce a keyframe on request the way a dedicated WebRTC sender would).

**LANFLIX's remaining loss is seeking**: 25–31 ms against 19 ms for HLS. All are imperceptible, but the gap is consistent. An HTTP player fetches exactly one segment; LANFLIX starts a new server-side stream generation, looks up a keyframe and reads a page from Redis.

## 13.7 Packet loss and raw UDP

Raw UDP was tested separately: the same clip streamed as MPEG-TS, with a controlled fraction of datagrams dropped at the receiver.

| Datagram loss | Decoder errors per minute | Frames lost |
|---|---|---|
| 0% | 0 | 0% |
| 0.5% | 71 | 0.5% |
| 1% | 167 | 1.6% |
| 5% | 565 | 5.3% |

At 1% loss, which is ordinary for busy Wi-Fi, raw UDP produces a visible error almost three times a second. The TCP-based systems turn loss into small delays instead, and WebRTC repairs it with retransmission requests and its jitter buffer. That's why raw multicast isn't a practical choice despite its bandwidth advantage (chapter 4.5).

## 13.8 Limits of this study

A benchmark should state what it didn't show:

- **One machine, loopback network.** Real Wi-Fi adds delay and jitter to every system. WebRTC's jitter buffer and the HTTP players' margins would grow. Relative ordering is what these results support; absolute numbers are optimistic.
- **No loss on the benchmarked systems.** Loss was applied only to raw UDP.
- **One synthetic source, one bitrate, 10 trials per system.** Enough to separate the bands clearly, but not to resolve small differences within them with high confidence.
- **Not tested:** LL-DASH, WebRTC tuned for a minimum jitter buffer, adaptive bitrate.

## Check your understanding

1. Why does feeding every system from one encoder matter more than using identical encoder *settings* in separate encoders?
2. Why is the 1st percentile a better estimate of the encoder's clock than the minimum here? What would make the median a bad choice?
3. Explain how running sessions back to back could bias latency for segment-based systems, and how randomization fixes it.
4. The results show push and pull at near-parity on latency within each band. Using the latency budget, explain why.
5. LL-HLS delivers parts as fast as push. What does it pay for that, and how does the cost scale with viewers?
6. Name one conclusion these results support strongly and one they don't support at all.
