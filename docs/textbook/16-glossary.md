# Chapter 16 — Glossary

Chapter numbers show where each term is explained in full.

| Term | Meaning |
|---|---|
| **AAC** | The standard compressed audio codec in MP4 files. (2) |
| **ABR (adaptive bitrate)** | Offering several quality levels and switching between them as bandwidth changes. (4, 14) |
| **At-least-once delivery** | A message may be delivered more than once, never zero times; consumers must be idempotent. (10) |
| **avcC** | The box in an MP4 init segment holding H.264 configuration, including profile and level. (2, 9) |
| **B-frame** | A frame predicted from both an earlier and a later frame; makes decode order differ from display order. (2) |
| **Backfill** | Sending a viewer the stored history from a chosen point up to the live edge. (8) |
| **Backpressure** | A consumer's ability to slow down a producer. (8) |
| **Bitrate** | Bits per second a stream needs; about 3 Mb/s for 720p H.264. (1, 2) |
| **Box (atom)** | The unit of an MP4 file: a size, a type and contents. (2) |
| **Broker** | A Kafka server that stores partitions. (10) |
| **CDN** | A content delivery network: caches serving files close to users; the reason HTTP streaming scales on the internet. (4) |
| **Chunk / fragment** | A `moof` + `mdat` pair: an independently deliverable piece of fragmented MP4. LANFLIX uses 2 s (standard) or 200 ms (low latency). (2) |
| **Codec** | An algorithm that compresses and decompresses media, e.g. H.264. (2) |
| **Codec string** | A MIME parameter like `avc1.64001f,mp4a.40.2` describing exactly what a stream contains. (2, 9) |
| **Consumer group** | A set of Kafka consumers sharing a topic's partitions, with committed offsets; different groups are independent. (10) |
| **Container** | A file format packaging media tracks with timing, e.g. MP4. Also: an isolated packaged process (Docker). (2, 12) |
| **Context (Go)** | A value carrying cancellation and deadlines through a call tree. (12) |
| **CORS** | Cross-origin resource sharing: a server's opt-in allowing pages from other origins to read its responses. (11) |
| **Credit-based flow control** | The receiver grants the sender permission to send up to a limit, extended as it consumes. (8) |
| **DASH** | Dynamic Adaptive Streaming over HTTP: segment-based streaming described by an XML manifest (MPD). (4) |
| **Decode timestamp (DTS)** | When a frame must be decoded. (2) |
| **Demuxer** | Code that splits a container stream into its parts; here, an init segment and fragments. (6) |
| **DVR** | The ability to pause, rewind and seek within a live stream's history. (1) |
| **Epoch / generation** | A counter that tags which request a response belongs to, so stale responses can be discarded. (8) |
| **Fan-out** | Delivering one message to many recipients from a single read. (8, 14) |
| **ffmpeg** | The standard open-source tool for decoding, encoding and packaging media. (2, 6) |
| **Fragmented MP4 (fMP4)** | MP4 split into an init segment and self-describing fragments, so it can be produced and played incrementally. (2) |
| **Framing** | How message boundaries are found in a byte stream, e.g. length prefixes. (6) |
| **Glass-to-glass latency** | Time from a frame being produced to it appearing on a viewer's screen. (4, 13) |
| **Goroutine** | A lightweight concurrent function managed by Go's runtime. (12) |
| **GOP** | Group of pictures: a keyframe and the frames that depend on it, up to the next keyframe. (2) |
| **H.264 / AVC** | The most widely supported video codec. (2) |
| **Head-of-line blocking** | In TCP, a lost packet delays all later data until it's retransmitted. (4) |
| **HLS** | HTTP Live Streaming: segments plus a text playlist that players poll. (4) |
| **Hold-back (safety margin)** | How far behind the newest media a player deliberately stays to absorb jitter. (4, 9) |
| **Hub** | LANFLIX's per-stream fan-out: one Redis reader broadcasting to all viewers of a stream. (8) |
| **I-frame / IDR / keyframe** | A frame decodable on its own; an IDR guarantees nothing after it references anything before it. The only place decoding can start. (2) |
| **ICE** | The WebRTC process of finding a working network path between peers. (4) |
| **Idempotent** | Safe to apply more than once with the same result. (7, 10, 11) |
| **Init segment** | `ftyp` + `moov`: codec and track information a player needs once, before media. (2) |
| **Jitter buffer** | A small receive buffer that smooths out uneven packet arrival (WebRTC). (4) |
| **Kafka** | A distributed, durable, partitioned event log. (10) |
| **Keyset pagination** | Paging by "entries after the last key seen" rather than by page number. (7) |
| **KRaft** | Kafka's built-in metadata consensus mode, replacing ZooKeeper. (10, 12) |
| **Lease / heartbeat** | A claim that expires unless renewed; its absence signals failure. (6) |
| **Live edge** | The newest media that exists in a live stream. (9) |
| **LL-HLS** | Low-Latency HLS: partial segments, blocking playlist reloads and preload hints. (4) |
| **Long poll** | A request the server holds open until there's something to return. (4, 7) |
| **Materialized view** | A read-optimized structure derived from a log or other data. (10) |
| **mdat / moof** | The media-data box, and the movie-fragment box describing it. (2) |
| **MSE (Media Source Extensions)** | The browser API that lets JavaScript supply the bytes a video element plays. (3) |
| **Multicast** | Sending one packet that the network delivers to every member of a group. (4) |
| **NACK** | A receiver's request to retransmit a specific lost packet (WebRTC). (4) |
| **Offset** | A message's position in a Kafka partition. (10) |
| **Opus** | The audio codec WebRTC requires. (2, 4) |
| **Partition** | An independently ordered slice of a Kafka topic. (10) |
| **PLI** | Picture loss indication: a WebRTC receiver asking for a new keyframe. (4) |
| **Presentation timestamp (PTS)** | When a frame must be shown. (2) |
| **Pull vs push** | Whether the client requests each piece (pull) or the server sends it when ready (push). (4) |
| **Redis** | An in-memory data store with rich data structures. (7) |
| **Redis Stream** | An append-only log in Redis with ordered IDs, range reads and blocking reads. (7) |
| **Retention** | How long data is kept before being trimmed. (7, 10) |
| **RTP / RTCP** | The packet format for real-time media, and its control and feedback protocol. (4) |
| **SDP** | Session Description Protocol: the offer/answer format WebRTC uses to agree on codecs and addresses. (4) |
| **Secure context** | A page served over HTTPS or from localhost; required for some browser APIs. (3, 11) |
| **SFU** | Selective forwarding unit: a server that forwards WebRTC media from one sender to many receivers. (4) |
| **Shard key** | The value used to decide which machine stores a piece of data. (14) |
| **Sorted set** | A Redis set whose members are ordered by a numeric score. (7) |
| **SourceBuffer** | The MSE object that media bytes are appended to. (3) |
| **TCP / UDP** | Reliable ordered byte streams vs individual unreliable datagrams. (4) |
| **Timescale** | The number of timestamp ticks per second in an MP4 track. (2) |
| **TTFF** | Time to first frame: from pressing play to the first frame on screen. (13) |
| **TTL** | Time to live: an expiry after which a key is deleted automatically. (7) |
| **WebRTC** | The browser's real-time media stack over UDP, built for calls. (4) |
| **WebSocket** | A long-lived, two-way message channel between browser and server over TCP. (4, 8) |
| **WHEP** | WebRTC-HTTP Egress Protocol: a convention for starting WebRTC playback with one HTTP POST. (4) |
| **XSS** | Cross-site scripting: user-supplied text executed as code in other users' browsers. (11) |
