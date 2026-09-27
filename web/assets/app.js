// Shared helpers for every page. Plain ES module, no build step.

// Every name and title here is typed in by some host on the LAN and
// rendered into innerHTML on everyone else's screen — escape all of it.
export function esc(value) {
  return String(value ?? '').replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

export function fmtTime(ms) {
  const total = Math.max(0, Math.floor((ms || 0) / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = String(total % 60).padStart(2, '0');
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${s}` : `${m}:${s}`;
}

export function timeAgo(ms) {
  const s = Math.max(0, Math.floor((Date.now() - ms) / 1000));
  if (s < 60) return 'just now';
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} hr ago`;
  return `${Math.floor(h / 24)} d ago`;
}

// Not crypto.randomUUID(): that needs a secure context (HTTPS or
// localhost) and throws on a plain http://LAN-IP origin — exactly how
// other devices on the network reach this app.
export function clientID() {
  let id = localStorage.getItem('client_id');
  if (!id) {
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    id = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
    localStorage.setItem('client_id', id);
  }
  return id;
}

export async function getJSON(url) {
  const res = await fetch(url);
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  return res.json();
}

export async function send(url, method = 'POST') {
  const res = await fetch(url, { method });
  if (!res.ok) throw new Error((await res.text()) || res.statusText);
  return res.status === 204 ? null : res.json();
}

// The live "watching now" view comes from the dashboard service — a
// separate process on :8091, fed by its own Kafka consumer group.
export async function watchingNowByMovie() {
  try {
    const viewers = await getJSON(`http://${location.hostname}:8091/api/viewers`);
    const counts = {};
    for (const v of Object.values(viewers)) counts[v.movie_id] = (counts[v.movie_id] || 0) + 1;
    return counts;
  } catch {
    return {};
  }
}

// A segment is 2s, and live_edge_ms is the *start* of the newest one.
export const SEGMENT_MS = 2000;
export const cachedDuration = (m) => (m.has_live_edge ? m.live_edge_ms + SEGMENT_MS : 0);

// navigator.clipboard, like crypto.randomUUID, only exists in a secure
// context — undefined on http://<LAN-IP>. Fall back to the legacy path.
export async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  document.execCommand('copy');
  ta.remove();
}

export function toast(message, kind = '') {
  let host = document.querySelector('.toast-host');
  if (!host) {
    host = document.createElement('div');
    host.className = 'toast-host';
    document.body.appendChild(host);
  }
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.textContent = message;
  host.appendChild(el);
  setTimeout(() => el.remove(), 3200);
}

export const icon = {
  play: '<svg viewBox="0 0 24 24"><path d="M8 5.14v13.72a1 1 0 0 0 1.5.86l11-6.86a1 1 0 0 0 0-1.72l-11-6.86A1 1 0 0 0 8 5.14Z"/></svg>',
  pause: '<svg viewBox="0 0 24 24"><path d="M7 5h3.5v14H7zM13.5 5H17v14h-3.5z"/></svg>',
  back10: '<svg viewBox="0 0 24 24"><path d="M12 5V2L7 6l5 4V7a6 6 0 1 1-6 6H4a8 8 0 1 0 8-8Z"/><text x="12" y="16.5" font-size="7" text-anchor="middle" font-family="sans-serif" font-weight="700">10</text></svg>',
  fwd10: '<svg viewBox="0 0 24 24"><path d="M12 5V2l5 4-5 4V7a6 6 0 1 0 6 6h2a8 8 0 1 1-8-8Z"/><text x="12" y="16.5" font-size="7" text-anchor="middle" font-family="sans-serif" font-weight="700">10</text></svg>',
  volume: '<svg viewBox="0 0 24 24"><path d="M4 9v6h4l5 4V5L8 9H4Zm12.5 3a4.5 4.5 0 0 0-2.5-4v8a4.5 4.5 0 0 0 2.5-4ZM14 3.2v2.1a7 7 0 0 1 0 13.4v2.1a9 9 0 0 0 0-17.6Z"/></svg>',
  muted: '<svg viewBox="0 0 24 24"><path d="M4 9v6h4l5 4V5L8 9H4Zm15.6 3 2.4 2.4-1.4 1.4-2.4-2.4-2.4 2.4-1.4-1.4 2.4-2.4-2.4-2.4 1.4-1.4 2.4 2.4 2.4-2.4 1.4 1.4-2.4 2.4Z"/></svg>',
  fullscreen: '<svg viewBox="0 0 24 24"><path d="M4 4h6v2H6v4H4V4Zm10 0h6v6h-2V6h-4V4ZM4 14h2v4h4v2H4v-6Zm14 4v-4h2v6h-6v-2h4Z"/></svg>',
  stats: '<svg viewBox="0 0 24 24"><path d="M4 20h16v-2H4v2Zm1-4h3V9H5v7Zm5.5 0h3V4h-3v12Zm5.5 0h3v-5h-3v5Z"/></svg>',
  eye: '<svg viewBox="0 0 24 24"><path d="M12 5C6.5 5 2.7 9.4 1.5 12c1.2 2.6 5 7 10.5 7s9.3-4.4 10.5-7C21.3 9.4 17.5 5 12 5Zm0 11a4 4 0 1 1 0-8 4 4 0 0 1 0 8Zm0-6a2 2 0 1 0 0 4 2 2 0 0 0 0-4Z"/></svg>',
  users: '<svg viewBox="0 0 24 24"><path d="M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8Zm7 0a3 3 0 1 0 0-6 3 3 0 0 0 0 6ZM9 13c-3.3 0-7 1.6-7 4v2h14v-2c0-2.4-3.7-4-7-4Zm7 0c-.6 0-1.3.1-2 .2 1.2.9 2 2.1 2 3.8v2h6v-2c0-2.2-3.3-4-6-4Z"/></svg>',
  clock: '<svg viewBox="0 0 24 24"><path d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20Zm1 10.4 3.2 3.2-1.4 1.4L11 13.2V6h2v6.4Z"/></svg>',
  film: '<svg viewBox="0 0 24 24"><path d="M4 3h2v2h2V3h8v2h2V3h2v18h-2v-2h-2v2H8v-2H6v2H4V3Zm2 4v2h2V7H6Zm10 0v2h2V7h-2ZM6 11v2h2v-2H6Zm10 0v2h2v-2h-2ZM6 15v2h2v-2H6Zm10 0v2h2v-2h-2Z"/></svg>',
  upload: '<svg viewBox="0 0 24 24"><path d="M12 3 7 8h3.5v7h3V8H17l-5-5ZM5 17v3h14v-3h2v5H3v-5h2Z"/></svg>',
  broadcast: '<svg viewBox="0 0 24 24"><path d="M12 10a2 2 0 1 0 0 4 2 2 0 0 0 0-4Zm-4.2-2.8 1.4 1.4a4 4 0 0 0 0 5.6l-1.4 1.4a6 6 0 0 1 0-8.4Zm8.4 0a6 6 0 0 1 0 8.4l-1.4-1.4a4 4 0 0 0 0-5.6l1.4-1.4ZM5 4.4l1.4 1.4a8.5 8.5 0 0 0 0 12.4L5 19.6a10.5 10.5 0 0 1 0-15.2Zm14 0a10.5 10.5 0 0 1 0 15.2l-1.4-1.4a8.5 8.5 0 0 0 0-12.4L19 4.4Z"/></svg>',
  copy: '<svg viewBox="0 0 24 24"><path d="M8 4h10a2 2 0 0 1 2 2v10h-2V6H8V4ZM4 8h10a2 2 0 0 1 2 2v10H6a2 2 0 0 1-2-2V8Zm2 2v8h8v-8H6Z"/></svg>',
  arrow: '<svg viewBox="0 0 24 24" style="width:16px;height:16px;fill:currentColor"><path d="M13.2 5.3 19.9 12l-6.7 6.7-1.4-1.4 4.3-4.3H4v-2h12.1l-4.3-4.3 1.4-1.4Z"/></svg>',
  trash: '<svg viewBox="0 0 24 24"><path d="M9 3h6l1 2h4v2H4V5h4l1-2Zm-3 6h12l-1 12H7L6 9Z"/></svg>',
};

// Header shared by every page; `active` highlights the current section.
export function renderHeader(active) {
  const header = document.createElement('header');
  header.className = 'app-header';
  header.innerHTML = `
    <a class="brand" href="index.html">
      <span class="brand-mark">${icon.play}</span>
      LANFLIX
    </a>
    <nav class="nav">
      <a href="watch.html" class="${active === 'watch' ? 'active' : ''}">Watch</a>
      <a href="host.html" class="${active === 'host' ? 'active' : ''}">Host</a>
    </nav>
    <div class="header-spacer"></div>
  `;
  document.body.prepend(header);
}

// Deterministic gradient from a name, for tiles without a poster yet.
function placeholderStyle(seed) {
  let h = 0;
  for (const ch of String(seed)) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  const a = h % 360;
  const b = (a + 50) % 360;
  return `background: linear-gradient(135deg, hsl(${a} 55% 32%), hsl(${b} 60% 18%));`;
}

export function artHTML({ posterId, seed, label }) {
  const initial = esc((label || '?').trim().charAt(0).toUpperCase() || '?');
  const img = posterId
    ? `<img src="/api/movie/poster?id=${encodeURIComponent(posterId)}" alt="" loading="lazy"
         onerror="this.remove()">`
    : '';
  return `<div class="placeholder" style="${placeholderStyle(seed)}">${initial}</div>${img}`;
}

export function statusPill(status) {
  if (status === 'live') return '<span class="pill live"><span class="live-dot"></span>Live</span>';
  if (status === 'encoding') return '<span class="pill encoding">Starting</span>';
  return '<span class="pill ended">Replay</span>';
}

export function emptyHTML(title, body) {
  return `<div class="empty">${icon.film}<strong>${esc(title)}</strong><span>${esc(body)}</span></div>`;
}
