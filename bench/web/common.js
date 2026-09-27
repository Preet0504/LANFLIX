// Identical instrumentation for every system under test. Each page only
// supplies how to start its player, how to seek, and how to report when a
// segment's bytes have arrived; everything measured is measured here.

export const q = new URLSearchParams(location.search);
export const wall = () => performance.timeOrigin + performance.now();
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export const B = (window.__bench = {
  system: q.get('system'),
  trial: Number(q.get('trial') || 0),
  tStart: null,        // wall ms when the player was told to start
  ttffMs: null,        // tStart → first frame actually presented
  firstMediaTime: null,
  frames: [],          // [presented wall ms, media time s], every frame
  receipts: {},        // segment n → wall ms the client had its bytes
  sampleStart: null,
  sampleEnd: null,
  seeks: [],           // { target, ms }
  stalls: 0,
  stallMs: 0,
  errors: [],
  done: false,
});

export async function timeline() {
  const r = await fetch('/timeline', { cache: 'no-store' });
  return r.json();
}

export function markReceipt(n) {
  if (Number.isInteger(n) && n >= 0 && !(n in B.receipts)) B.receipts[n] = wall();
}

export function instrument(video) {
  let waitingAt = null;
  video.addEventListener('waiting', () => {
    // Only steady-state playback counts: startup buffering is TTFF, and
    // buffering right after a seek is seek latency.
    if (B.sampleStart == null || B.sampleEnd != null) return;
    waitingAt = wall();
    B.stalls++;
  });
  video.addEventListener('playing', () => {
    if (waitingAt != null) { B.stallMs += wall() - waitingAt; waitingAt = null; }
  });
  video.addEventListener('error', () => B.errors.push('video error: ' + (video.error && video.error.message)));

  // presentationTime is when the compositor actually showed the frame —
  // the closest a browser gets to "photons on screen".
  const onFrame = (_now, meta) => {
    const shownAt = performance.timeOrigin + meta.presentationTime;
    if (B.ttffMs == null && B.tStart != null) {
      B.ttffMs = shownAt - B.tStart;
      B.firstMediaTime = meta.mediaTime;
    }
    B.frames.push([shownAt, meta.mediaTime]);
    video.requestVideoFrameCallback(onFrame);
  };
  video.requestVideoFrameCallback(onFrame);
}

// Deterministic per-trial seek targets, identical across systems.
function rng(seed) {
  let s = seed >>> 0 || 1;
  return () => ((s = (s * 1664525 + 1013904223) >>> 0) / 4294967296);
}

async function frameNear(targetS, afterMs, timeoutMs) {
  const deadline = wall() + timeoutMs;
  while (wall() < deadline) {
    for (let i = B.frames.length - 1; i >= 0 && B.frames[i][0] > afterMs; i--) {
      if (Math.abs(B.frames[i][1] - targetS) < 2.5) return B.frames[i][0];
    }
    await sleep(5);
  }
  return null;
}

// Protocol shared by every page: wait for first frame, sample steady-state
// playback, then seek to the same pseudo-random points in the DVR window.
export async function protocol({ seek, sampleS = 15, seeks = 4 }) {
  const t0 = wall();
  while (B.ttffMs == null && wall() - t0 < 25000) await sleep(20);
  if (B.ttffMs == null) { B.errors.push('no first frame within 25s'); B.done = true; return; }

  B.sampleStart = wall();
  await sleep(sampleS * 1000);
  B.sampleEnd = wall();

  const tl = await timeline();
  const edgeS = (Math.max(...Object.keys(tl.ours).map(Number)) * tl.gop_ms) / 1000;
  const rand = rng(B.trial * 7919 + 17);
  for (let i = 0; i < seeks && edgeS > 30; i++) {
    const target = 5 + rand() * (edgeS - 25);
    const start = wall();
    seek(target);
    const shown = await frameNear(target, start, 15000);
    B.seeks.push({ target, ms: shown == null ? null : shown - start });
    await sleep(1500);
  }
  B.done = true;
}
