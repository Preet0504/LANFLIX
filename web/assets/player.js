import { clientID, getJSON, SEGMENT_MS } from './app.js';

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
    const conn = { ws: null, sb: null, queue: [], timer: null, closed: false, appends: 0, retry: null };
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
        // The first append is the init segment (no timestamped media); the
        // seekable range only exists after the second. Deferred because
        // touching the element synchronously inside 'updateend' has crashed
        // Chromium's media pipeline here before.
        if (conn.appends === 2 && fromMs > 0) {
          setTimeout(() => { if (!conn.closed) video.currentTime = fromMs / 1000; }, 0);
        }
        this.pump(conn);
      });
      return true;
    };

    mediaSource.addEventListener('sourceopen', () => {
      URL.revokeObjectURL(url);
      if (conn.closed) return;

      const proto = location.protocol === 'https:' ? 'wss' : 'ws';
      const wsUrl = `${proto}://${this.host}/ws?movie=${encodeURIComponent(this.movieId)}` +
        `&client_id=${encodeURIComponent(clientID())}&from_ms=${encodeURIComponent(fromMs)}`;
      const ws = new WebSocket(wsUrl);
      ws.binaryType = 'arraybuffer';
      conn.ws = ws;

      ws.onopen = () => {
        if (conn.closed) return;
        this.stats.connectedAt = performance.now();
        conn.timer = setInterval(() => {
          if (ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ position_ms: Math.floor(video.currentTime * 1000) }));
          }
        }, 2000);
      };
      ws.onmessage = (evt) => {
        if (conn.closed) return;
        if (this.onChunk) this.onChunk(evt.data);
        if (this.stats.firstByteAt == null) this.stats.firstByteAt = performance.now();
        this.stats.chunks++;
        this.stats.bytes += evt.data.byteLength;
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
      const behind = this.video.currentTime - 15;
      if (behind > 1) {
        conn.sb.remove(0, behind); // fires updateend → pump retries
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
    conn.ws.send(JSON.stringify({ seek_ms: ms }));
    this.video.currentTime = ms / 1000;
    return ms;
  }

  // How far playback trails the newest content that exists.
  behindLiveMs() {
    return Math.max(0, this.liveEdgeMs + SEGMENT_MS - this.video.currentTime * 1000);
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
