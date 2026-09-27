// Browser benchmark runner. Needs the origin (bench/source) and the real
// server (cmd/server) running, plus playwright-core resolvable, e.g.:
//   NODE_PATH=<dir containing node_modules> node bench/run.js --trials 8
//
// Systems are interleaved round-robin within each trial so slow drift in
// machine load can't systematically favor one of them. Every run uses a
// fresh browser context (fresh cache, fresh client identity).

const fs = require('fs');
const path = require('path');
const { chromium } = require('playwright-core');

const args = Object.fromEntries(process.argv.slice(2).reduce((acc, a, i, arr) => {
  if (a.startsWith('--')) acc.push([a.slice(2), arr[i + 1]]);
  return acc;
}, []));
const TRIALS = Number(args.trials || 8);
const SAMPLE = Number(args.sample || 15);
const SEEKS = Number(args.seeks || 4);
const OUT = args.out || 'bench/results/browser.json';
const ORIGIN = 'http://localhost:8095';
const EDGE = process.env.BROWSER_PATH || 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';

const SYSTEMS = [
  { id: 'ours', page: 'ours.html', label: 'Ours (Redis Streams + WS push)' },
  { id: 'ours-ll', page: 'ours.html?ll=1', label: 'Ours, 200ms chunks' },
  { id: 'hls', page: 'hls.html', label: 'HLS (hls.js defaults)' },
  { id: 'hls-tuned', page: 'hls.html?tuned=1', label: 'HLS (tuned: 1-segment sync)' },
  { id: 'll-hls', page: 'hls.html?ll=1', label: 'LL-HLS (200ms parts, hls.js)' },
  { id: 'dash', page: 'dash.html', label: 'DASH (dash.js defaults)' },
  { id: 'dash-tuned', page: 'dash.html?tuned=1', label: 'DASH (tuned: 2.5s delay)' },
  { id: 'webrtc', page: 'webrtc.html', label: 'WebRTC (pion relay)' },
].filter((s) => !args.only || args.only.split(',').includes(s.id));

// Which origin request log each system's traffic is filed under.
const ORIGIN_SYSTEM = { hls: 'hls', 'hls-tuned': 'hls', 'll-hls': 'llhls', dash: 'dash', 'dash-tuned': 'dash', webrtc: 'webrtc' };

async function requestsSince(since) {
  const r = await fetch(`${ORIGIN}/requests?since=${since}`);
  return r.json();
}

(async () => {
  const browser = await chromium.launch({
    executablePath: EDGE,
    headless: true,
    // Chrome hides local IPs behind mDNS names in WebRTC candidates; the
    // relay is on this machine, so hand it plain addresses.
    args: ['--autoplay-policy=no-user-gesture-required', '--disable-features=WebRtcHideLocalIpsWithMdns'],
  });
  const results = [];
  let seed = 20260927;
  const rand = () => ((seed = (seed * 1664525 + 1013904223) >>> 0) / 4294967296);

  for (let trial = 1; trial <= TRIALS; trial++) {
    // rotate order each trial as well
    const order = SYSTEMS.map((_, i) => SYSTEMS[(i + trial) % SYSTEMS.length]);
    for (const sys of order) {
      const ctx = await browser.newContext({ viewport: { width: 800, height: 600 } });
      const page = await ctx.newPage();
      const consoleErrors = [];
      page.on('pageerror', (e) => consoleErrors.push(e.message));

      // Random (seeded) 0-2s wait before each session. Without it the whole
      // round of sessions took a near-constant 146s = 73 segments, so each
      // system joined at the same point of the 2s segment cycle in every
      // trial, and latency — which includes how far into the newest segment
      // a viewer joins — came out biased per system instead of averaged.
      const jitterMs = Math.floor(rand() * 2000);
      await new Promise((r) => setTimeout(r, jitterMs));
      const sep = sys.page.includes('?') ? '&' : '?';
      const url = `${ORIGIN}/web/${sys.page}${sep}system=${sys.id}&trial=${trial}&sample=${SAMPLE}&seeks=${SEEKS}`;
      const startedAt = Date.now();
      await page.goto(url);
      try {
        await page.waitForFunction(() => window.__bench && window.__bench.done, null, { timeout: (SAMPLE + SEEKS * 18 + 40) * 1000 });
      } catch {
        consoleErrors.push('runner timeout');
      }
      const b = await page.evaluate(() => window.__bench);
      const endedAt = Date.now();
      await ctx.close();

      const reqs = (await requestsSince(startedAt)).filter((r) => r.at <= endedAt);
      const system = ORIGIN_SYSTEM[sys.id] || null;
      const mine = system ? reqs.filter((r) => r.system === system) : [];

      results.push({
        system: sys.id, label: sys.label, trial,
        wallStart: startedAt, wallEnd: endedAt, jitterMs,
        httpRequests: mine.length,
        httpManifestRequests: mine.filter((r) => r.kind === 'manifest').length,
        ...b,
        pageErrors: consoleErrors,
      });
      const lat = b.frames && b.sampleStart ? 'ok' : 'n/a';
      console.log(`[trial ${trial}] ${sys.id.padEnd(10)} ttff=${b.ttffMs == null ? '—' : b.ttffMs.toFixed(0) + 'ms'} ` +
        `segs=${Object.keys(b.receipts || {}).length} seeks=${(b.seeks || []).map((s) => s.ms == null ? 'X' : s.ms.toFixed(0)).join(',')} ` +
        `stalls=${b.stalls} errors=${(b.errors || []).length + consoleErrors.length} frames=${lat}`);
    }
    fs.mkdirSync(path.dirname(OUT), { recursive: true });
    fs.writeFileSync(OUT, JSON.stringify(results));
  }

  const tl = await (await fetch(`${ORIGIN}/timeline`)).json();
  fs.writeFileSync(path.join(path.dirname(OUT), 'timeline.json'), JSON.stringify(tl));
  await browser.close();
  console.log(`wrote ${results.length} runs to ${OUT}`);
})().catch((e) => { console.error(e); process.exit(1); });
