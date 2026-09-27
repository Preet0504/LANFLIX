# Chapter 15 — System design principles

Every chapter introduced principles in context. This chapter collects them, each with where it appeared in LANFLIX and where else you'll meet it. They're what you should keep after forgetting the details of MP4 boxes.

## Requirements and trade-offs

**1. Start from requirements and numbers, not technologies.** Chapter 1's estimates predicted that the network, not the server, would limit scale, before any code existed, and chapter 14 confirmed it. *Elsewhere:* every capacity plan and every system design interview.

**2. Every design is a set of trade-offs; name what you give up.** LANFLIX gives up CDN caching, adaptive bitrate, iPhone support and sub-200 ms latency, deliberately, because none were requirements (chapter 4.9). A design that claims no downsides hasn't been examined. *Elsewhere:* consistency vs availability, latency vs throughput, simplicity vs flexibility.

**3. Choose tools by the bottleneck you actually have.** Go over Rust because the workload is concurrency, not computation (chapter 12.1). Redis for video because its data structures match the access patterns; Kafka for events because its guarantees match theirs (chapters 7 and 10). *Elsewhere:* choosing databases, queues, languages.

## Data and storage

**4. Normalize at the boundary so the core can make strong assumptions.** Re-encoding every upload into one shape (chapter 5, decision 1) lets everything downstream assume 2 s keyframes and one codec. *Elsewhere:* input validation, schema enforcement at ingestion, canonical data models.

**5. Choose identifiers that carry the meaning your queries need.** Timestamps as stream IDs turned a log into a time index (chapter 7.3). *Elsewhere:* time-ordered IDs (ULIDs, Snowflake IDs), composite keys in wide-column stores, partition keys in DynamoDB.

**6. Keep secondary indexes consistent by writing them atomically with the data.** The keyframe index is updated in the same transaction as the fragment (chapter 7.4). *Elsewhere:* database indexes, search indexes, denormalized views.

**7. Separate latest-value state from event history, and use each where it fits.** Resume points in Redis, position history in Kafka (chapter 10.1). *Elsewhere:* event sourcing, change data capture, the log vs table duality.

**8. Paginate by the last key seen.** Keyset pagination stays correct while data grows (chapter 7.6). *Elsewhere:* every well-designed list API.

**9. Bound retention by what it means to users.** Hours of video, not entry counts (chapter 7.7). *Elsewhere:* log retention, cache sizing, TTLs.

## Concurrency and correctness

**10. Make create-if-absent atomic with a conditional write.** `HSETNX` for unique room names (chapter 7.8). *Elsewhere:* unique usernames, idempotency keys, distributed locks, `INSERT … ON CONFLICT`.

**11. Make operations idempotent so retries and replays are safe.** Create-or-get rooms (chapter 11.2), `ZADD GT` in analytics (chapter 10.5). *Elsewhere:* payment APIs with idempotency keys, at-least-once message processing, PUT semantics in HTTP.

**12. When combining a snapshot with a change feed, subscribe first and deduplicate the overlap.** Backfill + tail with no gap (chapter 8.2). *Elsewhere:* database replication, cache warming, real-time UI sync, CDC pipelines.

**13. When requests overlap, tag responses with the request they answer.** Stream generations and `seek_ack` (chapter 8.7). *Elsewhere:* fencing tokens for distributed locks, request IDs, optimistic concurrency version numbers.

**14. Keep last-writer-wins updates ordered.** One writer per connection for resume points (chapter 8.8). *Elsewhere:* per-key ordering in Kafka partitions, single-writer principles, actor models.

**15. Detect failure by the absence of a renewed signal.** The encoding lease with a 10 s expiry (chapter 6.7). *Elsewhere:* heartbeats, leader leases, service discovery health checks, session timeouts.

**16. Make a write visible before acknowledging it.** Register the movie before responding to the upload (chapter 6.2). *Elsewhere:* read-your-writes consistency, write-through caches.

**17. Establish a precondition before acting, not after hoping.** Apply a seek once buffered media covers it (chapter 3.4 and 9.4). *Elsewhere:* retries with checks, reconciliation loops, Kubernetes controllers.

**18. Every wait needs a way out.** Context cancellation on every blocking operation (chapter 12.4). *Elsewhere:* timeouts on every network call, deadline propagation in RPC systems.

## Flow and scale

**19. Stream, don't slurp.** Uploads go to disk in chunks, backfill reads pages of four (chapters 6.1 and 7.6). *Elsewhere:* streaming parsers, chunked transfer, cursors.

**20. Every buffer needs a bound and an overflow policy.** Hub buffers, player queue, media buffer (chapter 5, decision 5). An unbounded queue is a memory leak with a delay. *Elsewhere:* thread pools, message queues, TCP buffers, rate limiters.

**21. A broadcast must never wait for its slowest receiver.** Lagging viewers move to their own path, losing nothing (chapter 8.4). *Elsewhere:* pub/sub systems, chat fan-out, market data feeds, logging pipelines.

**22. In a push system, the receiver sets the pace.** Credit-based flow control, in media time (chapter 8.6). And transport-level flow control doesn't protect application-level buffers. *Elsewhere:* TCP windows, HTTP/2 and QUIC flow control, Reactive Streams, gRPC.

**23. Remove the term that multiplies by users.** One Redis reader per stream, not per viewer (chapter 14.2). *Elsewhere:* fan-out on write vs read in feeds, connection pooling, request coalescing, caching.

**24. Keep request-handling processes stateless.** All durable state in Redis and Kafka makes delivery servers restartable and horizontally scalable (chapters 5.4 and 14.5). *Elsewhere:* twelve-factor apps, horizontally scaled web tiers.

**25. Keep non-essential dependencies off the hot path.** Kafka down means stale counts, not stopped video (chapter 10.7). *Elsewhere:* graceful degradation, bulkheads, async analytics.

## Measurement

**26. Measure against the strongest alternatives, under identical conditions.** One encoder for every system, and the best configurations of each competitor, including the low-latency ones (chapter 13). *Elsewhere:* A/B tests, performance regressions, vendor evaluations.

**27. Decompose a metric before optimizing it.** The latency budget (chapter 4.8) predicted that chunk size, not push vs pull, dominates latency. The measurements agreed. *Elsewhere:* profiling, critical-path analysis, tail-latency work.

**28. Randomize what you don't control, and check your instruments.** Randomized join times, a clock calibrated from the encoder's own output, and causality checks on the data (chapter 13). *Elsewhere:* experimental design in any field.

**29. Report what you lose.** The comparison table shows where LANFLIX is slower or costlier. That's what makes the rest of it believable.

## A closing thought

Most of these principles aren't about video. They're about **time** (ordering, staleness, windows), **sharing** (one resource, many users), and **failure** (processes die, networks drop, clients retry). Those three appear in every distributed system. A streaming platform just makes them visible, because every mistake shows up on screen.
