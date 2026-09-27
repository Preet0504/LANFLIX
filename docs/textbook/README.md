# Building LANFLIX: System Design Through a Video Streaming Platform

A textbook that teaches system design by building one real system from the ground up: a platform that streams video to everyone on a local network. Viewers can join a stream at any moment, rewind, jump back to live and pick up tomorrow where they left off today.

Every chapter explains a piece of the system and, more importantly, *why* it is built that way. That includes which alternatives existed and what each one would have cost. The same ideas carry over to systems that have nothing to do with video: chat, trading, telemetry, multiplayer games, job queues.

## Who this is for

Readers who can program in some language and want to understand how real systems are designed. No prior knowledge of video, Redis, Kafka, Go or networking is assumed. Every term is explained the first time it appears, and the [glossary](16-glossary.md) collects them.

## How to read it

The chapters build on each other, so read them in order the first time. Each ends with **check your understanding** questions. If you can answer them without looking back, you've got the chapter.

| Part | Chapter | What you learn |
|---|---|---|
| **I. The problem** | [1. What we are building](01-the-problem.md) | Requirements, constraints, back-of-the-envelope estimates |
| **II. Video from first principles** | [2. How digital video works](02-video-fundamentals.md) | Frames, codecs, keyframes, GOPs, containers, fragmented MP4, timestamps |
| | [3. Playing video in a browser](03-browser-playback.md) | The `<video>` element, Media Source Extensions, buffers, seeking |
| | [4. Getting video from A to B](04-delivery-protocols.md) | TCP vs UDP, HLS, DASH, LL-HLS, WebRTC, multicast, push vs pull, the latency budget |
| **III. The LANFLIX design** | [5. Architecture](05-architecture.md) | The components, the data flow, and the decisions behind them |
| | [6. Ingest](06-ingest.md) | Streaming uploads, driving ffmpeg, parsing MP4 boxes, chunking |
| | [7. Storage: Redis and Redis Streams](07-redis-storage.md) | In-memory data structures, append-only logs, meaningful IDs, indexes, retention, leases |
| | [8. Delivery: the streaming server](08-delivery-server.md) | WebSockets, backfill + tail, fan-out, slow consumers, flow control, stream generations |
| | [9. The player](09-the-player.md) | Turning pushed bytes into smooth playback |
| | [10. Events and Kafka](10-events-kafka.md) | Event logs, consumer groups, replay, hot state vs history |
| | [11. The web app, API and the LAN](11-web-app-api-lan.md) | API design, idempotency, rooms, reaching other devices, browser security |
| | [12. Go and the infrastructure](12-go-and-infrastructure.md) | Why Go, goroutines and channels, context cancellation, Docker Compose |
| **IV. Proving it** | [13. Measuring honestly](13-measuring.md) | Benchmark design, clocks, randomization, and what the results mean |
| | [14. Scaling](14-scaling.md) | Load testing, where the limits are, how the design would grow |
| **V. Lessons** | [15. System design principles](15-design-lessons.md) | The ideas from this project that transfer to any system |
| | [16. Glossary](16-glossary.md) | Every term, in one place |

## The system in one paragraph

A host uploads a video file. The server encodes it in real time with **ffmpeg** into small, independently deliverable pieces of video called **chunks**. Each chunk is appended to a **Redis Stream**, an append-only log in memory. The log entry's ID is the chunk's position in the video, so the log doubles as an index by time. Each viewer's browser holds one **WebSocket** connection. The server **pushes** chunks down it: first the history the viewer asked for, then every new chunk the moment it exists, never more than 30 seconds ahead of what the viewer is watching. The browser feeds the chunks to the video element through **Media Source Extensions**. Every two seconds the browser reports its position. The server saves it (so the viewer can resume later) and publishes it to **Kafka**, where a live dashboard and an analytics service consume it independently.

The rest of this book explains each clause of that paragraph.
