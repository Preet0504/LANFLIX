import { clientID, getJSON, SEGMENT_MS } from './app.js';

// Flow control: the server sends media only up to this far ahead of the
// playhead (the buffer hls.js keeps by default). Every position ping
// extends the grant; a seek restarts it at the target.
const WINDOW_MS = 30000;

// How far behind the newest media "Live" lands on a low-latency stream:
// a few chunks of margin, the LL-HLS PART-HOLD-BACK convention.
const LL_HOLD_BACK_MS = 600;

// Scan for a box's 4-char type; returns the index of the type field.
function findBox(u8, type) {
  const [a, b, c, d] = [...type].map((ch) => ch.charCodeAt(0));
  for (let i = 4; i + 3 < u8.length; i++) {
    if (u8[i] === a && u8[i + 1] === b && u8[i + 2] === c && u8[i + 3] === d) return i;
  }
  return -1;
}

// The codec string has to describe the actual stream. It used to be a
// hardcoded "avc1.64001e" (H.264 High@L3.0) — but a 720p source encodes at
// L3.1 and 1080p at L4.x, and Chrome merely tolerates the mismatch. It also
// declared an AAC track even for videos that have no audio. Read both from
// the init segment instead: avcC carries profile/compat/level in bytes 1-3.
export function codecFromInit(buf) {
  const u8 = new Uint8Array(buf);
  const codecs = [];
  const avcC = findBox(u8, 'avcC');
  if (avcC >= 0) {
    const hex = [u8[avcC + 5], u8[avcC + 6], u8[avcC + 7]].map((x) => x.toString(16).padStart(2, '0')).join('');
    codecs.push('avc1.' + hex);
  }
  if (findBox(u8, 'mp4a') >= 0) codecs.push('mp4a.40.2'); // our encoder always emits AAC-LC
  return `video/mp4; codecs="${codecs.join(',')}"`;
}

// StreamPlayer drives one <video> element over the WebSocket bridge.
//
// All per-connection state (socket, SourceBuffer, queue, ping timer)
// lives in a `conn` object rather than on the player. An earlier version
// kept the ping timer in a shared variable, so when a reconnect closed
// the old socket, that socket's late-firing onclose cleared the *new*
// connection's timer — silently stopping resume points and analytics
// after any reconnect. Each conn now only ever touches its own state.
export class StreamPlayer {
  // host: where the WebSocket bridge lives. Defaults to the page's own
  // origin; the benchmark harness serves its pages from another port.
  constructor(video, { onLog = () => {}, host = location.host, onChunk = null } = {}) {
    this.video = video;
    this.onLog = onLog;
    this.host = host;
    this.onChunk = onChunk; // observes every raw message as it arrives
    this.conn = null;
    this.movieId = null;
    this.liveEdgeMs = 0;
    this.chunkMs = SEGMENT_MS; // from the movie's metadata
    this.stats = freshStats();

    video.addEventListener('playing', () => {
      if (this.stats.ttffMs == null && this.stats.requestedAt) {
        this.stats.ttffMs = performance.now() - this.stats.requestedAt;
      }
    });
  }

  static supported() {
    return 'MediaSource' in window && MediaSource.isTypeSupported('video/mp4; codecs="avc1.42E01E,mp4a.40.2"');
  }

  // Resolves this viewer's resume point first: the client has to know the
  // exact offset it starts at to align currentTime once media lands.
  async open(movieId) {
    this.movieId = movieId;
    let resumeMs = 0;
    try {
      const r = await getJSON(`/api/resume?movie=${encodeURIComponent(movieId)}&client_id=${encodeURIComponent(clientID())}`);
      resumeMs = r.position_ms || 0;
    } catch { /* start from the beginning */ }
    this.connect(resumeMs);
    return resumeMs;
  }

  connect(fromMs) {
    this.teardown();
    const video = this.video;
    // awaitAck: the seek (ms) whose {"seek_ack"} hasn't arrived yet; media
    // received meanwhile belongs to the previous position and is dropped.
    // pendingSeek: where playback should be once buffered media covers it.
    const conn = { ws: null, sb: null, queue: [], timer: null, closed: false, appends: 0, retry: null, awaitAck: null,
      pendingSeek: fromMs > 0 ? fromMs / 1000 : null };
    this.conn = conn;
    this.stats = { ...freshStats(), requestedAt: performance.now(), fromMs };

    const mediaSource = new MediaSource();
    const url = URL.createObjectURL(mediaSource);
    video.src = url;

    // The SourceBuffer is created when the init segment (always the first
    // message on a socket) arrives, from the codecs it actually declares.
    const createSourceBuffer = (initSegment) => {
      const codec = codecFromInit(initSegment);
      if (!MediaSource.isTypeSupported(codec)) {
        this.onLog('unsupported stream codec: ' + codec);
        return false;
      }
      const sb = mediaSource.addSourceBuffer(codec);
      sb.mode = 'segments';
      conn.sb = sb;
      this.stats.codec = codec;
      sb.addEventListener('updateend', () => {
        conn.appends++;
        this.applyPendingSeek(conn);
        this.pump(conn);
      });
      return true;
    };

    mediaSource.addEventListener('sourceopen', () => {
      URL.revokeObjectURL(url);
      if (conn.closed) return;

      const proto = location.protocol === 'https:' ? 'wss' : 'ws';
      const wsUrl = `${proto}://${this.host}/ws?movie=${encodeURIComponent(this.movieId)}` +
        `&client_id=${encodeURIComponent(clientID())}&from_ms=${encodeURIComponent(fromMs)}&window_ms=${WINDOW_MS}`;
      const ws = new WebSocket(wsUrl);
      ws.binaryType = 'arraybuffer';
      conn.ws = ws;

      ws.onopen = () => {
        if (conn.closed) return;
        this.stats.connectedAt = performance.now();
        conn.timer = setInterval(() => {
          if (ws.readyState === WebSocket.OPEN) {
            const pos = Math.floor(video.currentTime * 1000);
            ws.send(JSON.stringify({ position_ms: pos, until_ms: pos + WINDOW_MS }));
          }
        }, 2000);
      };
      ws.onmessage = (evt) => {
        if (conn.closed) return;
        if (typeof evt.data === 'string') {
          // The server marks where each position's media begins. Seeks can
          // be coalesced server-side, so only the latest one's ack counts.
          let msg = null;
          try { msg = JSON.parse(evt.data); } catch { /* not ours */ }
          if (msg && msg.seek_ack === conn.awaitAck) conn.awaitAck = null;
          return;
        }
        if (this.onChunk) this.onChunk(evt.data);
        if (this.stats.firstByteAt == null) this.stats.firstByteAt = performance.now();
        this.stats.chunks++;
        this.stats.bytes += evt.data.byteLength;
        if (conn.sb && conn.awaitAck != null) return; // pushed for the position we just left
        if (!conn.sb && !createSourceBuffer(evt.data)) {
          conn.closed = true;
          ws.close();
          return;
        }
        conn.queue.push(evt.data);
        this.pump(conn);
      };
      ws.onclose = () => {
        clearInterval(conn.timer);
        if (!conn.closed) this.onLog('disconnected');
      };
    });
  }

  // Chromium clamps a seek past the end of the buffered media to that end,
  // and it stays there: the media for the real target arrives later and
  // the playhead never moves to it. So a start position or seek target is
  // (re)applied once buffered media actually covers it. Found twice by the
  // benchmark: joins with sub-GOP chunks started up to a GOP early, and a
  // forward seek after a long backfill froze playback for good. Deferred
  // because touching the element synchronously inside 'updateend' has
  // crashed Chromium's media pipeline here before.
  //
  // The server starts at a keyframe at or before the target, but that
  // chunk's first *displayed* frame can sit slightly after it: B-frames
  // and audio priming shift presentation by up to a few frames. So a range
  // starting within half a second after the target counts, and playback
  // starts at its first frame.
  applyPendingSeek(conn) {
    const t = conn.pendingSeek;
    if (t == null || conn.awaitAck != null) return;
    const b = conn.sb.buffered;
    for (let i = 0; i < b.length; i++) {
      if (b.start(i) <= t + 0.5 && b.end(i) > t) {
        conn.pendingSeek = null;
        const to = Math.max(t, b.start(i));
        if (Math.abs(this.video.currentTime - to) > 0.25) {
          setTimeout(() => { if (!conn.closed) this.video.currentTime = to; }, 0);
        }
        return;
      }
    }
  }

  pump(conn) {
    if (conn.closed || !conn.sb || conn.sb.updating || conn.queue.length === 0) return;
    const chunk = conn.queue[0];
    try {
      conn.sb.appendBuffer(chunk);
      conn.queue.shift();
    } catch (e) {
      if (e.name !== 'QuotaExceededError') {
        conn.queue.shift();
        this.onLog('append error: ' + e.message);
        return;
      }
      // Buffer full — typical when backfilling a long video from the start.
      // Keep the chunk, free what's already been watched, and try again.
      // Only if something is actually buffered behind the playhead: a
      // remove() over an empty range still fires updateend, which retried
      // the append at once — a hot loop the benchmark caught at ~30,000
      // failed appends a second once pushed-ahead media filled the quota.
      const behind = this.video.currentTime - 15;
      const b = conn.sb.buffered;
      if (b.length && b.start(0) < behind - 0.5) {
        conn.sb.remove(b.start(0), behind); // fires updateend → pump retries
      } else {
        clearTimeout(conn.retry);
        conn.retry = setTimeout(() => this.pump(conn), 1000);
      }
    }
  }

  seekTo(ms) {
    ms = Math.floor(Math.max(0, Math.min(ms, this.liveEdgeMs)));
    const conn = this.conn;
    if (!conn || !conn.ws || conn.ws.readyState !== WebSocket.OPEN) {
      this.connect(ms);
      return ms;
    }
    // Media still queued for appending was pushed for the old position;
    // appending it first is what used to make seeks wait seconds.
    conn.queue.length = 0;
    conn.awaitAck = ms;
    conn.pendingSeek = ms / 1000;
    conn.ws.send(JSON.stringify({ seek_ms: ms }));
    this.video.currentTime = ms / 1000;
    return ms;
  }

  // How far playback trails the newest content that exists.
  behindLiveMs() {
    return Math.max(0, this.liveEdgeMs + this.chunkMs - this.video.currentTime * 1000);
  }

  // Where "Live" should play from. With 2s chunks: the start of the newest
  // one. With low-latency chunks: just behind the end of the newest, so a
  // viewer plays about a second behind real time instead of two to four.
  liveTargetMs() {
    if (this.chunkMs >= SEGMENT_MS) return this.liveEdgeMs;
    return Math.max(0, this.liveEdgeMs + this.chunkMs - LL_HOLD_BACK_MS);
  }

  teardown() {
    const c = this.conn;
    if (!c) return;
    c.closed = true;
    clearInterval(c.timer);
    clearTimeout(c.retry);
    if (c.ws) c.ws.close();
    this.conn = null;
  }
}

function freshStats() {
  return { requestedAt: 0, connectedAt: 0, firstByteAt: null, ttffMs: null, chunks: 0, bytes: 0, fromMs: 0 };
}
