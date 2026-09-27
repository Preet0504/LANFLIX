# Chapter 14 — Scaling

"Does it scale?" is meaningless without numbers: scale in *what*, to *how much*, limited by *which resource*? This chapter measures how LANFLIX behaves as viewers are added, identifies which resource runs out first, and sketches how the design would grow past one machine.

## 14.1 How to load-test a streaming server

The load generator (`bench/load`) opens N WebSocket connections to one live stream, all joining at the live edge, and for each new fragment records:

- **delivery delay**: from the moment the fragment was appended to Redis until each viewer received it;
- **integrity**: a SHA-256 hash of every received fragment, compared with the hash of what was published;
- **completeness**: fragments received versus fragments that should have been received;
- **server CPU**: sampled from the operating system for the server process only.

Each step (1, 10, 25, 50, 100, 200 viewers) runs for a fixed window, and all steps run against the same stream. The load generator runs on the same machine as the server, so its CPU use (about 25% of a core at 200 viewers) competes with the server's. The numbers are conservative.

## 14.2 Two designs, measured

The fan-out question from chapter 8.3 was settled by measuring both designs under identical load.

**Design A: one Redis reader per viewer.** Each viewer's session runs its own blocking `XREAD`.

**Design B: one reader per stream (the hub).** One `XREAD` per stream, broadcast in memory.

| Viewers | A: median delay | A: p95 | A: server CPU | B: median delay | B: p95 | B: server CPU |
|---|---|---|---|---|---|---|
| 1 | 4 ms | 32 ms | 0.1% | 4 ms | 17 ms | 0.1% |
| 10 | 2 ms | 12 ms | 1.9% | 6 ms | 15 ms | 0.5% |
| 50 | 10 ms | 41 ms | 6.1% | 24 ms | 35 ms | 3.4% |
| 100 | 67 ms | 104 ms | 15.9% | 43 ms | 117 ms | 5.2% |
| 200 | 159 ms | 338 ms | 31.4% | **89 ms** | **230 ms** | **12.0%** |

(CPU is a percentage of one core.) Up to about 50 viewers the two designs are close: Redis is fast, and a few dozen blocked reads are nothing. Past that, design A degrades sharply. Each blocked `XREAD` holds a connection from the client's pool (80 connections on this machine), so beyond 80 viewers the rest *queue for a connection*, and a new fragment reaches viewers in waves. At 200 viewers, Redis showed 81 blocked clients in design A and 2 in design B.

Design B halves the median delay at 200 viewers and cuts server CPU by about 60%, and **its Redis load no longer depends on the number of viewers at all**.

> **Principle: find the term in your cost model that multiplies by users, and remove it.** Here, "Redis reads = viewers × fragments" became "streams × fragments". The biggest scaling wins usually come from changing which variable something is proportional to, not from making each operation faster.

## 14.3 Correctness under load

Across all steps: **4,196 of 4,196 expected fragment deliveries arrived, with 0 hash mismatches and 0 connection failures.** Scaling tests should always check correctness as well as speed. A system that gets faster by silently dropping or corrupting data under load hasn't scaled.

## 14.4 Which resource runs out first

| Resource | At 200 viewers of one 720p stream | Headroom |
|---|---|---|
| Server CPU (delivery) | 12% of one core | Large: several cores unused |
| Redis | 1 blocked read per stream; microsecond operations | Very large |
| Server memory | Fragments are shared across viewers; per-viewer state is small | Large |
| **Network egress** | **570 Mb/s** | **Small: at or beyond real Wi-Fi capacity** |
| Encoder CPU | One ffmpeg per live stream, independent of viewers | Limits the number of *streams*, not viewers |

As the chapter 1 estimate predicted, **the network is the bottleneck.** A typical home Wi-Fi network can't carry 200 simultaneous 720p streams, whatever the server does. Beyond this point, making the server faster buys nothing. This is also why flow control (chapter 8.6) matters beyond the browser's memory: bytes sent ahead that nobody watches are taken from everyone else's share of the network.

The per-stream costs are different. Each live stream needs an encoder process (the heaviest CPU consumer in the system) and about 1.35 GB of Redis memory per hour of retained video (chapter 1). Those limit how many streams one machine can host at once.

## 14.5 Growing past one machine

LANFLIX targets one machine on one network, but the architecture was chosen so that growth has clear paths. Each step removes the current bottleneck:

**More viewers than one server's network card: several delivery servers.** The delivery server holds no durable state (chapter 5.4), so several identical instances can run behind a load balancer, all reading the same Redis. Each instance runs its own hub, i.e. one Redis read per stream *per instance*, so Redis load grows with instances × streams, still independent of viewers. A viewer's connection lives on one instance for its lifetime, and a reconnect can land on any instance, because resume points are in Redis.

**More streams than one machine can encode: separate ingest workers.** Ingest and delivery already communicate only through Redis, so encoding can move to dedicated machines without changing delivery at all.

**More data than one Redis: shard by stream.** Streams are independent. No operation ever touches two streams' fragments. So the stream ID is a natural **shard key**: hash the ID to pick one of several Redis instances (Redis Cluster does exactly this). Metadata and indexes shard the same way. Choosing a shard key that no query ever needs to cross is what makes sharding easy.

**Viewers spread over many networks: a relay tree.** Push delivery can't use HTTP caches or CDNs the way HLS can. The equivalent is a tree of relays: edge servers that each receive a stream once from upstream and push it to local viewers, the same fan-out pattern as the hub one level up. That's how large live-streaming and WebRTC platforms scale out.

**Viewers with different bandwidth: adaptive bitrate.** Encode several renditions (e.g. 1080p, 720p, 480p), each as its own stream with keyframes at the *same* timestamps, and let the server switch a viewer between renditions at keyframe boundaries based on how fast their socket drains. The time-aligned keyframe index makes switching straightforward.

Each of these is a real project. The point is that none requires redesigning the core, because the core's boundaries (stateless delivery, a shared log, independent streams) were drawn where growth would need them.

## Check your understanding

1. In design A, why did delay jump between 50 and 100 viewers rather than growing smoothly?
2. Why is checking hash matches as important in a load test as measuring delay?
3. At 200 viewers the server used 12% of a core. Why doesn't that mean it could serve 1,600 viewers on a laptop?
4. Why is the stream ID a good shard key? Give an example of a feature that would make it a worse one.
5. If LANFLIX ran three delivery servers for one stream with 600 viewers, how many blocked Redis reads would there be? How many with design A?
