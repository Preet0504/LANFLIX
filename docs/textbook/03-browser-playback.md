# Chapter 3 — Playing video in a browser

The viewer's side of LANFLIX is a web page, and "zero install" means we get only what browsers provide. This chapter covers the browser machinery for playing video you deliver yourself, and the rules it imposes, which the rest of the design must respect.

## 3.1 The simple way, and why it isn't enough

The simplest way to play video in a page is:

```html
<video src="movie.mp4" controls></video>
```

The browser downloads the file over HTTP and plays it, fetching ranges of the file as needed when you seek. This is called **progressive download**. It works beautifully for a finished file on a web server, and fails every live requirement from chapter 1:

- The file must exist in full, with its frame table in `moov`. A live stream never "exists in full".
- The browser decides what to fetch and when. We can't push new video to it.
- There's no concept of "the live point" or of a file that grows while you watch.

To play a live stream, the page needs to feed the video element *itself*, piece by piece, from any source it likes. That's what Media Source Extensions provide.

## 3.2 Media Source Extensions (MSE)

**Media Source Extensions** (MSE) is a browser API that lets JavaScript supply the bytes a `<video>` element plays. The page creates a `MediaSource` object, attaches it to the video element, and appends fragments to it. The browser demuxes, decodes and plays them as if they came from a file.

```js
const video = document.querySelector('video');
const mediaSource = new MediaSource();
video.src = URL.createObjectURL(mediaSource);   // attach: the video now plays from mediaSource

mediaSource.addEventListener('sourceopen', () => {
  // One SourceBuffer per stream format; the codec string comes from the init segment.
  const sb = mediaSource.addSourceBuffer('video/mp4; codecs="avc1.64001f,mp4a.40.2"');
  sb.appendBuffer(initSegment);        // first: the init segment (ftyp + moov)
  // later, repeatedly: sb.appendBuffer(fragment)   (moof + mdat)
});
```

The key objects:

| Object | Role |
|---|---|
| `MediaSource` | The "virtual file" the video element plays. Fires `sourceopen` once attached. |
| `SourceBuffer` | Where appended bytes go. The browser parses them into decoded-ready frames on a timeline. |
| `appendBuffer(bytes)` | Adds one chunk of bytes. **Asynchronous**: `sb.updating` is true until the `updateend` event fires, and you may not append again until then. |
| `sb.buffered` | A list of time ranges (e.g. `[40.0–71.2], [120.0–122.0]`) currently held, ready to play. |
| `sb.remove(start, end)` | Frees a range of buffered media. Also asynchronous, and also fires `updateend`. |
| `video.currentTime` | The playhead. Setting it is a **seek**. |

**Segments mode.** A SourceBuffer in `segments` mode places each fragment on the timeline by the timestamps inside it (the `tfdt` from chapter 2). That's what makes random-order appends work: append the fragment for minute 40, then the one for minute 2, and each lands in the right place. LANFLIX uses segments mode for exactly this reason. Seeking means "start sending from a different point in the stream", and the fragments place themselves correctly.

## 3.3 The append queue

Because `appendBuffer` is asynchronous and forbids overlapping calls, any player that receives data faster than it appends must **queue** incoming fragments and append them one at a time:

```
network ──► [ queue: f7 f8 f9 ... ] ──► appendBuffer(f7) … updateend ──► appendBuffer(f8) … ──► SourceBuffer ──► decoder ──► screen
```

This is the classic **producer–consumer** pattern. The network produces, the SourceBuffer consumes, and the queue absorbs the difference in their speeds. Chapter 8 explains why this queue must not be allowed to grow without limit, and how the *server* is told to slow down: flow control.

## 3.4 Buffered ranges and seeking

A player can only *play* media that's buffered. When you set `currentTime` to a time inside a buffered range, playback continues from there. When you set it to a time that isn't buffered, the element enters a waiting state until media covering that time is appended.

There's an important subtlety that shapes LANFLIX's player. A seek past the **end** of all buffered media can be **clamped to that end** by the browser (Chromium does this). The element then sits at the clamped position, even if media for the real target arrives a moment later. A player that delivers its own data therefore shouldn't trust a single `currentTime` assignment made before the data arrives. The robust pattern is:

1. Remember the target position.
2. After each append completes, check whether a buffered range now covers the target.
3. Once one does, set `currentTime`, then stop tracking the target.

This "apply when possible" pattern turns up in many systems. It's a small instance of a general idea: **make an operation's precondition true before performing it, rather than issuing it early and hoping.**

Two more details from real encodings:

- The server starts sending at the keyframe *at or before* the target (chapter 7), but the first *displayed* frame of that fragment can land a few frames after its keyframe's decode time. B-frames reorder display, and audio priming shifts the video slightly. So "the buffered range covers the target" must allow a small tolerance: LANFLIX accepts a range starting up to half a second after the target and starts playback at its first frame.
- Decoding always starts at a keyframe. If the target is 1.6 s into a 2 s GOP, the browser decodes 1.6 s of frames it never shows, to reconstruct the one it does. This is invisible, but it's why seek time depends on GOP length.

## 3.5 Memory: quotas and eviction

Browsers cap how much media a SourceBuffer may hold, typically around 100–150 MB for video in Chromium. At 3 Mb/s that's roughly 5–6 minutes. When an append would exceed the cap, `appendBuffer` throws a `QuotaExceededError`, and it's the page's job to free space with `remove()` and try again.

LANFLIX's policy is simple: keep 15 seconds behind the playhead (for small rewinds) and evict everything older. If nothing is behind the playhead to evict, wait and retry. Space will free up as playback advances. Two rules keep this correct:

- **Only remove what exists.** Calling `remove()` on an empty range still fires `updateend`, which would trigger an immediate retry, which fails again. That's a busy loop.
- **Don't let the buffer fill with media far ahead of the playhead in the first place.** If the server sends much more than the viewer will watch soon, the buffer fills with future media that can't be evicted. That's the player's side of the flow-control story in chapter 8.

## 3.6 Knowing when frames actually appear

To measure a video system you need to know *when each frame was shown*, not just when it arrived. **`requestVideoFrameCallback`** calls a function for every frame the compositor presents. It passes metadata including the `presentationTime` (when the frame reached the screen) and the frame's `mediaTime` (its position on the video timeline). For WebRTC streams, it also passes the frame's RTP timestamp. This API is how the benchmark in chapter 13 measures time-to-first-frame and glass-to-glass latency precisely.

## 3.7 Browser policies that affect design

- **Autoplay.** Browsers block video with sound from playing until the user interacts with the page. Muted video may autoplay. A player should attempt `play()` and, if it's refused, show a play button rather than silently stalling.
- **Secure contexts.** Some APIs exist only on HTTPS pages or `localhost`: `crypto.randomUUID()`, `navigator.clipboard`, WebTransport, camera access. On a LAN, pages are served over plain `http://192.168.x.x`, so LANFLIX avoids these APIs (chapter 11).
- **iPhone.** Safari on iPhone doesn't offer classic `MediaSource`. Since iOS 17.1 it offers `ManagedMediaSource`, a variant where the browser, not the page, decides when to fetch and evict. LANFLIX's player uses classic MSE, so iPhones aren't supported yet; iPads and desktop Safari are. Native HLS playback is the reason HLS works on every iPhone (chapter 4).

## Check your understanding

1. Why can't a live stream be played with `<video src="...">` pointing at a growing file?
2. Why must a player queue fragments instead of calling `appendBuffer` as each one arrives?
3. What goes wrong if a player sets `currentTime` to a live-edge position before any media for it has been appended? Describe the fix in terms of preconditions.
4. A viewer seeks far back, and the server immediately sends ten minutes of video. What happens to the SourceBuffer, and why can't eviction behind the playhead fix it?
5. Which API would you use to measure the delay between a frame being encoded and it appearing on screen, and why not use the `playing` event?
