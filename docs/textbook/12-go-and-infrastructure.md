# Chapter 12 — Go and the infrastructure

This chapter covers the tools the system is built with: the Go language and the concurrency features the server depends on, the libraries, the project's structure, and the Docker setup that runs Redis and Kafka.

## 12.1 Why Go

The server's job is **many long-lived connections, each mostly waiting**: waiting on Redis, on a flow-control grant, on the network. That workload decides the language more than raw speed does. The candidates discussed at the start of the project:

| Language | Strengths for this workload | Weaknesses for this workload |
|---|---|---|
| **Go** | Cheap concurrency built into the language; excellent networking standard library; one static binary; fast compile; simple language | Garbage collector (no manual memory control); less expressive type system |
| **Rust** | Maximum performance and memory control; no GC; strong safety guarantees | Async ecosystem is powerful but complex; slower to write and iterate on; a steeper learning curve |
| **C++** | Performance, mature media libraries | Memory safety is on you; concurrency is hard to get right |
| **Node.js** | Great at I/O concurrency; same language as the browser | Single-threaded CPU; binary protocol parsing is clumsier |
| **Python** | Fastest to prototype | Slow for per-byte work; concurrency is limited |

The expensive work, video encoding, is done by ffmpeg in its own process, whatever language the server is written in. The server itself mostly moves byte slices between sockets and Redis. Go makes that concurrency *simple to write correctly*, and it's fast enough that the network, not the CPU, is the limit (chapter 14 measures 12% of one core at 200 viewers). Rust would buy performance this system doesn't need, at a real cost in development speed. **Choose tools by the bottleneck you actually have.**

## 12.2 Goroutines

A **goroutine** is a function running concurrently, started with the `go` keyword:

```go
go streamFrom(ctx, conn, epoch, credit, store, hub, movieID, fromMs)
```

Goroutines are not operating-system threads. They start with a few kilobytes of stack, and Go's runtime schedules them onto a small number of OS threads. A goroutine blocked on a network read or a channel costs almost nothing, and the runtime parks it and runs others. A server can therefore afford **several goroutines per viewer** (reader, stream, position writer), written as straightforward blocking code, with no callbacks and no manual event loops. With 200 viewers that's under a thousand goroutines, a trivial number for Go.

## 12.3 Channels and `select`

A **channel** is a typed pipe between goroutines. `select` waits on several channel operations at once and proceeds with whichever is ready first. The delivery server's core loop (chapter 8) is almost entirely `select`:

```go
for {
    select {
    case <-ctx.Done():          // viewer left or seeked: stop
        return
    case <-sub.lagged:          // fell behind the hub: switch to own Redis read
        ...
    case lc := <-sub.ch:        // next live fragment from the hub
        if err := credit.wait(ctx, lc.pts); err != nil { return }
        conn.WriteBinary(epoch, lc.data)
    }
}
```

Two channel idioms appear repeatedly:

**Non-blocking send.** Used where blocking would be harmful, such as the hub broadcasting to a full subscriber buffer (chapter 8.4) or a position report arriving while the writer is busy (chapter 8.8):

```go
select {
case sub.ch <- fragment:   // room in the buffer: deliver
default:                   // full: don't wait, handle the overflow
    sub.markLagged()
}
```

**Broadcast by closing.** A closed channel is readable by *any number* of waiters, all at once. The credit gate (chapter 8.6) and the LL-HLS benchmark origin use a "changed" channel: waiters block on it, and a writer that changes the state closes it and replaces it with a fresh one, waking every waiter exactly once. This is a simple, allocation-cheap condition variable that composes with `select` (so waits can also be cancelled).

## 12.4 Context: cancellation as a tree

A **`context.Context`** carries a cancellation signal (and deadlines) down a call tree. Cancelling a parent cancels all its children:

```
request context (the WebSocket session)
 ├── stream generation 1   ← cancelled by a seek
 ├── stream generation 2   ← current
 │     ├── Redis XRANGE / XREAD calls
 │     └── credit.wait(...)
 └── position writer
```

When a viewer disconnects, the session's context is cancelled, and every goroutine and every in-flight Redis call beneath it stops. When a viewer seeks, only the old generation's context is cancelled. Blocking operations take the context as their first argument and return promptly when it's cancelled. That convention is what lets code wait freely without leaking goroutines.

> **Principle: every wait needs a way out.** Anything that blocks (a network read, a queue, a lock wait, a flow-control gate) should also be interruptible by cancellation or a timeout. Goroutine leaks and hung requests usually come from waits that can't be cancelled.

## 12.5 Mutexes and when to use them

Channels coordinate goroutines; **mutexes** protect shared data. The WebSocket wrapper uses a mutex so only one goroutine writes at a time, and checks the generation number under the same lock (chapter 8.7), which makes "check generation, then write" atomic. The hub uses a mutex around its subscriber map. The rule of thumb: **channels for passing ownership of work, mutexes for guarding a piece of shared state**, and keep locked sections short. Never do network I/O while holding a lock other goroutines need often. (The socket write lock is the exception that proves the rule: it's *meant* to serialize writes to that one socket, and nothing else waits on it.)

## 12.6 Libraries

| Library | Used for |
|---|---|
| Go standard library `net/http` | The HTTP server, the multipart upload reader, the static file server |
| `github.com/gorilla/websocket` | WebSocket upgrade and message framing |
| `github.com/redis/go-redis/v9` | The Redis client: streams, sorted sets, transactions, a connection pool |
| `github.com/segmentio/kafka-go` | The Kafka producer and consumer groups |
| `github.com/pion/webrtc/v4` | Benchmark only: the WebRTC relay compared against in chapter 13 |

The MP4 box parser is hand-written (about 300 lines) rather than taken from a library. It needs only a handful of boxes, and understanding every byte of the format it depends on is part of the point.

## 12.7 Project structure

```
cmd/                   one directory per executable (package main)
  server/              web app, API, WebSocket delivery, hub, flow control
  dashboard/           Kafka consumer → live viewer view
  analytics/           Kafka consumer → retention data in Redis
  producer/            command-line ingest (the same pipeline as uploads)
  wsclient/            headless test viewer
  consumer/            dump a stream's fragments from Redis
internal/              shared packages, importable only inside this module
  chunker/             ffmpeg driver, MP4 box parser, fragment timing
  ingest/              register → encode → publish → lifecycle, heartbeat
  redisstream/         every Redis key and command in one place
  positionlog/         Kafka event type, producer, consumer
web/                   static front end
bench/                 the benchmark harness (chapter 13)
```

Two structural choices stand out. **All Redis access lives in one package** (`redisstream`): key names, data types, transactions. No other package knows how data is laid out, so changing the layout (adding the keyframe index, switching retention to time-based) touches one file. And **ingest is a package, not part of the server**, so the web upload and the command-line producer run the identical pipeline. **Put a boundary where a decision might change, and share code where behavior must be identical.**

## 12.8 Testing the invariants

The unit tests target the properties the design relies on, not line coverage:

| Test | Invariant it protects |
|---|---|
| Demuxer vs real ffmpeg output | Fragments are contiguous in time; keyframes are exactly one GOP apart; only GOP-boundary fragments are marked decodable |
| stderr flood | The encoder never blocks when ffmpeg writes a lot of diagnostics (chapter 6.3) |
| Stream-ID ordering | "Already sent" comparisons order `<ms>-<seq>` IDs correctly (chapter 8.2) |
| Seek generations | After a seek, no write from the old generation reaches the client, and the ack precedes the new media (chapter 8.7) |
| Flow-control credit | Unlimited without a window; blocks past the window; grants release; seeks reset; stale grants can't shrink (chapter 8.6) |

Testing **invariants**, the statements that must always hold, gives the most protection per test. They're exactly the properties a future change is most likely to break without noticing.

## 12.9 Infrastructure: Docker Compose

Redis and Kafka run in **containers**. A container packages a program with everything it needs into an isolated, reproducible unit, described by an **image** (`redis:7-alpine`, `apache/kafka:3.8.0`). **Docker Compose** starts a set of containers from one file:

```yaml
services:
  redis:
    image: redis:7-alpine
    ports: ["6379:6379"]
    command: ["redis-server", "--save", "", "--appendonly", "no"]   # in-memory only
  kafka:
    image: apache/kafka:3.8.0
    ports: ["9092:9092"]
    environment:
      KAFKA_PROCESS_ROLES: broker,controller     # KRaft: one node does both jobs
      ...                                        # listeners for inside and outside Docker
```

`docker compose up -d` gives every developer the same Redis and Kafka versions with the same configuration, with nothing installed on the host machine. The Go programs run directly on the host (`go run ./cmd/server`), which keeps the edit–run loop fast. The Kafka configuration declares two **listeners**, one address for containers talking to each other and one for programs on the host. That's the most common source of "can't connect to Kafka" confusion in Docker setups.

## Check your understanding

1. Why does the choice of language matter less for this system's throughput than you might expect? Where does the heavy computation actually happen?
2. Write a `select` that sends a value on a channel if there's room and otherwise increments a "dropped" counter.
3. Explain how closing a channel can wake several goroutines at once, and why the credit gate replaces the channel after closing it.
4. A viewer seeks three times in one second. Using the context tree, explain which goroutines are running afterwards.
5. Why keep all Redis key names and commands in one package?
