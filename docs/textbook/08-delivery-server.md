# Chapter 8 — Delivery: the streaming server

Delivery is the hot path: every fragment goes to every viewer watching its stream. It's also where most of the interesting concurrency lives. Each viewer is at a different point in the stream, can jump anywhere at any time, and may be fast, slow or paused. This chapter builds the delivery server one requirement at a time.

## 8.1 One session per viewer

A viewer connects with:

```
ws://<server>:8090/ws?movie=c5d682&client_id=9f3e…&from_ms=65300&window_ms=30000
```

| Parameter | Meaning |
|---|---|
| `movie` | Which stream |
| `client_id` | A random ID the browser generated once and keeps: the viewer's identity for resume points |
| `from_ms` | Where to start. If omitted, the server looks up the viewer's saved resume point |
| `window_ms` | This client does flow control (§8.6), with a 30 s window |

The server then:

1. **Waits for the init segment** if the stream was registered a moment ago and ffmpeg hasn't produced it yet. It polls briefly (up to 15 s) rather than failing: "not ready yet" isn't "not found".
2. **Sends the init segment** as the first binary message. It's sent exactly once per connection, even across seeks, because a SourceBuffer is initialized once.
3. **Starts a stream generation** at `from_ms` (§8.7): backfill, then follow the live edge.
4. **Reads control messages** from the client concurrently: seeks, position reports and flow-control grants.

Inside the server, each session is a few cooperating **goroutines**, lightweight threads (chapter 12):

```
                      ┌─────────────────────────────┐
  client messages ───►│ reader goroutine            │── seek ──► main loop ── starts/cancels ──► stream goroutine
                      │ (seek / position / grant)   │── position ──► position writer goroutine ──► Redis + Kafka
                      └─────────────────────────────┘── grant ──► flow-control credit
                                                                          ▲
                                                 stream goroutine waits ──┘ on credit before each write
```

Two rules keep this safe:

- **Only one goroutine writes to the socket at a time.** The WebSocket library forbids concurrent writes, and during a seek the old and new stream goroutines briefly overlap. All writes go through a small mutex-protected wrapper.
- **Everything is cancellable.** Each session has a *context* (chapter 12), and each stream generation has a child context. A seek cancels the old generation's context; a disconnect cancels the session's, which cancels everything under it.

## 8.2 Backfill, then tail, without losing a fragment

A viewer joining at minute 12 of a stream that's now at minute 20 needs two things: **history** (12 to 20) and **the future** (everything after 20, as it's produced). The obvious implementation has a bug:

```
1. read history from 12:00 up to the newest entry   ← finds entries up to 20:00
2. start following new entries after the last one read
```

A fragment appended *between* step 1 finishing and step 2 starting is in neither: it's after the history read, but before the follower started looking. The viewer's playback hits a hole. The window is small, but at 5 fragments per second and many viewers, small windows get hit.

The fix inverts the order and tolerates overlap instead of a gap:

```
1. SUBSCRIBE to new fragments first (they start queueing)
2. read history from 12:00 to the newest entry, sending it
3. drain the subscription, skipping any fragment whose ID ≤ the last one sent in step 2
```

Anything appended during step 2 is caught by the subscription. Anything that shows up in both places is dropped by the ID comparison. IDs are ordered, so "already sent" is a simple comparison (`<ms>-<seq>` compared as a pair of numbers).

> **Principle: when combining a snapshot with a change feed, subscribe before you snapshot, and deduplicate the overlap.** A gap is silent data loss; an overlap is a duplicate you can detect. The same rule applies to cache warming, database replication (a snapshot plus a change log), and syncing a UI with a server.

## 8.3 Fan-out: one reader per stream

If every viewer ran its own blocking Redis read (chapter 7.5), Redis would hold one blocked connection and do one read per viewer per fragment. With a connection pool of 80, the 81st viewer would queue behind the others. That's the "N × M" shape to avoid: work proportional to viewers × fragments at the shared resource.

Instead, the server runs a **hub**: for each stream with at least one viewer, **one** goroutine does the blocking read and **broadcasts** each new fragment to every subscribed viewer, in memory:

```
                               ┌──► viewer A's buffer (64) ──► A's socket
Redis ── XREAD BLOCK ── hub ───┼──► viewer B's buffer (64) ──► B's socket
       (one per stream)        └──► viewer C's buffer (64) ──► C's socket
```

- **Reference-counted lifecycle.** The hub's reader for a stream starts when the first viewer subscribes and stops when the last one leaves. No viewers means no Redis work.
- **Zero-copy broadcast.** Every viewer receives a reference to the *same* byte slice. The fragment exists once in memory, however many viewers there are.
- **Scales with streams, not viewers.** Redis sees one blocked read per live stream. Chapter 14 measures what this buys.

## 8.4 The slow-consumer problem

Broadcasting raises a hard question: **what happens when one viewer can't keep up?** A viewer on a weak Wi-Fi link, a paused viewer (§8.6), or a viewer whose socket is congested stops draining its buffer. If the hub waited for that viewer before moving on, **one slow viewer would stall every other viewer**. That's unacceptable.

The hub's rule: **never block the broadcast.** Each subscriber has a bounded buffer of 64 fragments (about two minutes at 2 s fragments). Delivery into it is non-blocking:

- If there's room, the fragment is queued.
- If the buffer is full, the subscriber is marked **lagged** and the hub moves on.

A lagged viewer isn't dropped and loses nothing. Its session notices the lag signal, **unsubscribes from the hub**, and switches to its **own** direct Redis read, starting exactly after the last fragment it sent (`XREAD` after its last ID). It continues at its own pace from the durable log, and the broadcast never waited for it.

This works because there's a durable log behind the broadcast. The hub is an optimization for the common case (viewers at the live edge); correctness comes from the log. A pure pub/sub system with no log could only drop a slow subscriber's messages.

> **Principle: a broadcast must never wait for its slowest receiver.** Bound each receiver's queue, and when it overflows, degrade that receiver (disconnect it, drop messages, or, best, move it to a path that reads at its own pace). Design the degraded path so it loses nothing, which usually means having a durable log to fall back on.

## 8.5 Backpressure: why the sender needs a brake

Push has a structural weakness that polling doesn't: **the sender decides how much to send.** An HLS player asks for the next segment only when it needs it, so its pace is naturally set by playback. A push server backfilling history will send it as fast as the network allows.

Suppose a viewer is 20 minutes into a stream that's now at 2 hours, and seeks back to the start. A naive push server immediately sends 2 hours of video, about 2.7 GB:

- The shared Wi-Fi is saturated for everyone.
- The browser receives far more than its media buffer can hold (chapter 3.5), and the excess piles up in the page's memory.
- Most of it will never be watched, because the viewer seeks again or leaves.

"TCP has flow control, doesn't that handle it?" No. TCP's flow control stops the sender from overrunning the receiver's **socket buffer**. The browser reads from the socket eagerly and hands every message to JavaScript, so from TCP's point of view the receiver is keeping up fine. The bytes just pile up one layer higher, in the page's queue. **Transport-level flow control protects the transport, not the application.** When the application has its own buffer with a meaningful limit ("30 seconds ahead of the playhead"), it needs its own flow control.

## 8.6 Credit-based flow control

LANFLIX uses the classic solution, **credit-based flow control**: the receiver grants the sender permission ("credit") to send up to some limit, and extends it as it consumes.

- **The unit of credit is media time, not bytes.** The client grants "you may send fragments up to video time `until_ms`". Time is what the player's buffer policy is about, and it's independent of bitrate.
- **Initial credit** comes with the connection: `window_ms=30000` means "up to 30 s past where I'm starting".
- **Grants** ride on the position reports the player already sends every 2 s: `{"position_ms": 312000, "until_ms": 342000}`. There are no extra messages.
- **The server checks before every write**: a fragment starting at `pts` is sent only if `pts ≤ until_ms`. Otherwise that viewer's stream goroutine **waits** until a grant extends the limit (or a seek or disconnect cancels it). Only that viewer's goroutine waits; nothing else is affected.
- **A seek resets the window** to the seek target plus 30 s, in either direction.
- **Grants only ever extend.** A grant computed before a seek can arrive after the reset; if it were allowed to *shrink* the window, it could stall the new position. Taking the maximum makes stale grants harmless. The reset is applied at the moment the seek message is read, in message order, so the only grants that can arrive "late" are exactly those that should be ignored.
- **Backward compatible.** A client that doesn't send `window_ms` (an older player, the load-testing tool) gets unlimited credit, i.e. the old behavior.

What it does, concretely: a viewer who starts at 0:00 of a stream that has been live for 109 seconds receives about 36 seconds of video in its first 8 seconds (the 8 seconds it played plus the 30-second window), not all 109. A paused viewer's server-side stream simply stops at 30 seconds ahead of the pause point.

How flow control interacts with the hub: a paused viewer at the live edge stops accepting fragments, its hub buffer fills, and it's marked lagged (§8.4). When playback resumes and grants flow again, it reads from Redis on its own path and catches up. The two mechanisms compose: the hub never waits for anyone, and flow control decides when each viewer's own path proceeds.

> **Principle: in a push system, the receiver must set the pace.** Credit-based flow control appears everywhere once you look: TCP's receive window, HTTP/2 and QUIC stream windows, Reactive Streams' `request(n)`, gRPC flow control, Kafka consumers pulling at their own pace. Whenever a producer can outrun a consumer, some mechanism must carry "how much more can you take" back upstream.

## 8.7 Seeking: stream generations and the ordering problem

A seek looks simple: the client sends `{"seek_ms": 754000}`, and the server stops what it was sending and starts from 12:34. But there's a subtle ordering problem. When the seek arrives, some fragments for the **old** position are already on their way:

- in the server's socket write (a write can't be taken back),
- in TCP buffers between the machines,
- in the browser, received but not yet appended.

If the player appended those, it would decode media for the old position after the seek, spend time and memory on it, and delay the media it actually wants. The player needs a way to tell **which bytes belong to which request**. LANFLIX does this with **stream generations** (epochs):

1. The server keeps a generation number per connection. Each seek increments it and starts a new stream goroutine tagged with the new number.
2. Every write carries the writer's generation. The write wrapper checks it under the same lock that serializes writes, and **drops writes from any older generation**. An old goroutine that slipped past its cancellation check can't get a single byte through after the new generation begins.
3. Before the new generation's first fragment, the server sends a text marker: `{"seek_ack": 754000}`.
4. On seek, the player clears its append queue and **discards every binary message until it sees the ack** for the seek it most recently requested. (If the viewer seeks twice quickly, the server may skip straight to the second, so only the latest seek's ack counts.)

Because a WebSocket is a single ordered TCP stream, the ack is a precise boundary: everything before it belongs to an old position, and everything after it to the new one.

> **Principle: when requests can overlap, tag responses with the request they answer.** Generation numbers, epochs, sequence IDs and **fencing tokens** (used by distributed locks so a stale lock holder's writes are rejected) are all versions of this idea. Cancelling the old work is necessary but never sufficient: some of it is always already in flight.

## 8.8 Position reports: ordered, and never in the way

Every 2 s the player reports its position. The server does three things with it: overwrite the viewer's resume point in Redis, record that the viewer has started this stream, and publish an event to Kafka. Two design points:

- **Order matters for last-writer-wins data.** The resume point is overwritten by each report, so whichever write lands last wins. If reports were written by independent concurrent tasks, a slow write of an *older* position could land after a newer one, and the resume point would go backwards. So all reports for a connection are written by **one** writer goroutine, in arrival order.
- **Reports must never block video control.** The reader goroutine hands reports to the writer through a small buffered queue. If the writer is stuck (Redis or Kafka momentarily slow), a report is **dropped** rather than blocking the reader, because the next report arrives in 2 s anyway, and a blocked reader would delay a seek. Drop the data that's cheap to lose, protect the path that isn't.

## 8.9 Putting it together: one seek, end to end

![Sequence of a seek: the client sends seek_ms; the server resets the credit window, bumps the generation, cancels the old stream goroutine and sends seek_ack; the new goroutine finds the keyframe, backfills page by page up to the window, and continues as grants arrive; the player drops bytes until the ack and applies the target position once media covers it](diagrams/seek-sequence.png)

## Check your understanding

1. Explain the gap that "backfill, then subscribe" can create, and why "subscribe, then backfill, then deduplicate" can't lose a fragment.
2. The hub's subscriber buffer holds 64 fragments. What happens, step by step, when a viewer's Wi-Fi stalls for five minutes at the live edge? Does the viewer miss anything?
3. Why doesn't TCP's own flow control stop a push server from flooding a browser?
4. Why is credit measured in *media time* rather than bytes? Name a situation where a byte-based window would behave badly.
5. A grant sent just before a seek arrives just after it. With "grants only extend", what happens in a forward seek? In a backward seek?
6. Why isn't cancelling the old stream goroutine enough to guarantee that no old-position bytes reach the player after a seek?
