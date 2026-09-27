# Chapter 9 — The player

The player (`web/assets/player.js`, about 250 lines) turns a WebSocket of pushed bytes into smooth playback. It applies chapter 3's browser machinery to chapter 8's protocol. It's small because the server does the hard work, but each part exists for a reason.

## 9.1 The pipeline

![Player pipeline: WebSocket messages are sorted into text control messages and binary media; the first binary message creates the SourceBuffer from its codec string; media goes into an append queue drained one appendBuffer at a time; after each append the player applies any pending seek and evicts old media when the quota is hit; every 2 s it reports position and grants credit](diagrams/player.png)

Per connection, the player keeps one small state object:

| Field | Purpose |
|---|---|
| `ws` | The WebSocket |
| `sb` | The SourceBuffer, created when the init segment arrives |
| `queue` | Fragments received but not yet appended |
| `pendingSeek` | Where playback should be once media covering it is buffered |
| `awaitAck` | The seek whose `seek_ack` hasn't arrived; media received meanwhile is discarded |
| `timer` | The 2 s position/credit report |

Keeping all of this **per connection**, rather than on the player object, matters. When the player reconnects, the old connection's late events (its `close` event can fire after the new connection opens) can only touch the old connection's state. **State tied to a resource's lifetime should live with that resource.** Then a stale callback can never reach into its successor.

## 9.2 Starting: the codec comes from the stream

The first binary message on every connection is the init segment. The player reads the H.264 profile and level from its `avcC` box and checks whether an `mp4a` (AAC) box exists. From those it builds the exact MIME string, e.g. `video/mp4; codecs="avc1.64001f,mp4a.40.2"`, checks it with `MediaSource.isTypeSupported`, and creates the SourceBuffer. A stream without audio gets a video-only codec string; a 1080p stream gets the right level. **The data describes itself; the client doesn't assume.**

## 9.3 Appending: one at a time, from a queue

Each binary message goes into `queue`, and `pump()` appends the head of the queue if the SourceBuffer isn't busy. Each `updateend` event calls `pump()` again. This is the producer–consumer loop from chapter 3.3. With server-side flow control (chapter 8.6), the queue never holds much more than the 30 s window, so it's naturally bounded.

## 9.4 Positioning: apply the target when the data exists

Two operations set where playback should be: **connecting** at a position (resume, or joining at a chosen point) and **seeking**. Both set `pendingSeek`. After every append, the player checks whether a buffered range now covers the target, and only then sets `video.currentTime`. The reason is chapter 3.4: a seek to an unbuffered position can be clamped to the end of the buffered media and stay there.

The check tolerates the first displayed frame being up to half a second after the target (B-frame reordering and audio priming, chapter 3.4), and starts playback at that frame. The assignment is deferred with `setTimeout(…, 0)`, which moves it out of the `updateend` handler. Changing the media element's position from inside a SourceBuffer event handler has been known to destabilize browser media pipelines, and the deferral costs nothing.

## 9.5 Seeking

A seek (clicking the progress bar, ±10 s, or "Live"):

1. clamps the target to the live edge, since you can't seek into the future;
2. **clears the queue**: those fragments were pushed for the old position;
3. sets `awaitAck` to the target and `pendingSeek` to the target;
4. sends `{"seek_ms": target}`;
5. sets `video.currentTime`, which works immediately if the target happens to be buffered already.

Until the matching `seek_ack` arrives, incoming media is discarded (chapter 8.7). The init segment is never discarded: it's needed once per connection regardless.

If the socket has dropped, a seek becomes a reconnect at the target. The server treats a fresh connection's `from_ms` exactly like a seek.

## 9.6 Live, and how far behind it

The movie's metadata includes `live_edge_ms` (the start of the newest chunk) and `chunk_ms` (2,000 or 200). The newest media therefore ends at `live_edge_ms + chunk_ms`. "Behind live" is that value minus the playhead.

Pressing **Live** targets:

- **Standard streams (2 s chunks):** the start of the newest chunk. Playback runs 2–4 s behind real time: one chunk of packaging delay (chapter 4.8) plus however far into that chunk's lifetime the viewer arrived.
- **Low-latency streams (200 ms chunks):** 0.6 s before the end of the newest chunk, i.e. three chunks of safety margin, the same convention LL-HLS uses (`PART-HOLD-BACK`). Playback runs about 0.7–1 s behind real time.

Before seeking to live, the player fetches the live edge **fresh**, rather than using the value it polls once a second. For a low-latency stream, a one-second-old edge would cost more than the entire safety margin.

## 9.7 Memory and eviction

When an append throws `QuotaExceededError` (chapter 3.5), the player removes buffered media older than 15 s behind the playhead and retries. If there's nothing old to remove, it waits a second and retries, since playback will free space. With flow control, the buffer holds at most about 15 s behind plus 30 s ahead, so quota errors become rare.

## 9.8 Reporting and granting

Every 2 s, while connected:

```json
{ "position_ms": 312480, "until_ms": 342480 }
```

One message does three jobs: the resume point (saved in Redis), the analytics event (published to Kafka) and the flow-control grant (30 s past the playhead). Reusing an existing periodic message for new purposes avoids adding traffic, and keeps grant freshness tied to something that already happens reliably.

## 9.9 Resume

On opening a stream, the player asks `GET /api/resume?movie=…&client_id=…` and connects at the returned position (or 0). Because the client ID is stored in the browser's `localStorage`, resume works across tabs and browser restarts on the same device. The server owns the position, not the browser, so it also appears in the "continue watching" list.

## Check your understanding

1. Why does the player build its codec string from the init segment instead of using a constant?
2. What are the two reasons the player must clear its queue on seek, and what would each look like to a viewer if it didn't?
3. Why is `pendingSeek` rechecked after every append instead of setting `currentTime` once?
4. Why does the Live button fetch a fresh live edge for low-latency streams in particular?
5. The position report serves three purposes. What are they, and what's the advantage of carrying them in one message?
