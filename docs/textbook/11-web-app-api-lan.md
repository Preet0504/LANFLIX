# Chapter 11 — The web app, the API and the LAN

Around the streaming core sits an ordinary web application: pages, a JSON API, identity, and the practicalities of reaching a laptop from a phone. Ordinary doesn't mean unimportant. Most of a system's surface area is here, and so are most of its security holes.

## 11.1 The pages

| Page | Who | What it does |
|---|---|---|
| `index.html` | Everyone | Landing page: host or watch, and what's live now |
| `host.html` | Hosts | Create or reopen a room; upload videos (with a low-latency option); live stats per stream; share link; delete |
| `watch.html` | Viewers | Browse rooms and streams, "continue watching", and the player with its controls |

The front end is **plain HTML, CSS and JavaScript modules**, with no framework and no build step. The browser loads `app.js` (shared helpers) and `player.js` (the streaming player) directly as ES modules. For three pages and a few thousand lines this is a deliberate choice: nothing to install, nothing to compile, and any browser's developer tools show exactly the code that runs. A framework pays off when UI state gets complex. Here, most state lives on the server.

## 11.2 API design

The JSON API follows a few conventions worth copying:

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/room/create?name=` | Create a room, **or return the existing one** with that name |
| GET | `/api/rooms` | All rooms, with stream and live counts |
| GET | `/api/room?id=` | One room |
| POST | `/api/room/delete?id=` | Delete a room and its streams |
| POST | `/api/host/start?room=` | Upload a video (multipart) and start streaming it |
| GET | `/api/movies?room=` | Streams in a room |
| GET | `/api/movie?id=` | One stream: status, live edge, chunk duration, viewers |
| POST | `/api/movie/delete?id=` | Delete a stream: data, index, analytics, uploaded file |
| GET | `/api/movie/poster?id=` | Poster image |
| GET | `/api/resume?movie=&client_id=` | A viewer's saved position |
| GET | `/api/continue-watching?client_id=` | Unfinished streams for a viewer |
| GET | `/api/server-info` | The server's LAN address, for share links |
| WS | `/ws?movie=&client_id=&from_ms=&window_ms=` | The stream itself (chapter 8) |

**Create-or-get is idempotent.** `POST /api/room/create?name=Movie Night` returns `{"room_id": "…", "created": true}` the first time and `{"room_id": "…" , "created": false}` every time after. The client can retry safely, and double-clicks don't create duplicates. Under the hood it's the atomic `HSETNX` claim from chapter 7.8. Building "do it only once" into the operation itself, rather than hoping clients call it once, is idempotency.

**Status codes carry meaning.**
- `400 Bad Request`: the request is malformed, e.g. a missing parameter.
- `404 Not Found`: no such room or movie.
- `405 Method Not Allowed`: a GET where only POST makes sense.
- **`409 Conflict`**: the request is valid, but the resource's current state forbids it. Deleting a stream that's still live returns 409 with "wait for it to end". Deleting a room with a live stream in it does the same. The client can show a precise message instead of a generic failure.

**Responses carry what the client needs next.** `GET /api/movie` includes `chunk_ms`, so the player knows where live is (chapter 9.6) without a second request, and `total_viewers_ever` from the analytics view. Upload responses echo whether low-latency mode was applied.

## 11.3 Identity without accounts

LANFLIX has no login, but resume and "continue watching" need to know *who* a viewer is. Each browser generates a **random 128-bit ID** once, stores it in `localStorage` and sends it as `client_id`. It's anonymous (no personal data), stable across visits on the same device, and cheap.

It's generated with `crypto.getRandomValues`, not `crypto.randomUUID`, because of the next section.

This is **not authentication**. Anyone can send any client ID. On a trusted home network that's an acceptable trade-off; anything beyond that would need real accounts.

## 11.4 Reaching the server from other devices

For a phone to watch a stream from a laptop, three things must line up.

**1. Listen on the right interface.** A server bound to `127.0.0.1` (loopback) accepts only connections from the same machine. LANFLIX binds `:8090`, meaning all interfaces, so the laptop's Wi-Fi address (e.g. `192.168.1.20`) accepts connections too.

**2. Know your own LAN address.** The host needs a link to share. A machine often has several addresses (Wi-Fi, Ethernet, virtual adapters for Docker or VPNs). To find the one that faces the network, the server creates a UDP socket "connected" to an outside address and asks which local address the operating system picked. For UDP, "connect" just selects a route and sends nothing, so this works offline and never sends a packet. The API returns it as `http://192.168.1.20:8090`.

**3. Get through the firewall.** Operating systems block inbound connections by default. On Windows the network must be marked *Private*, and a rule must allow TCP 8090 (and 8091 for the dashboard). The README gives the exact commands.

## 11.5 Browser security rules that shape the code

**Secure contexts.** Browsers restrict powerful APIs to **secure contexts**: pages served over HTTPS, or from `localhost`. A page at `http://192.168.1.20:8090` is not a secure context, so these are simply undefined there:

| API | Used for | LANFLIX's alternative |
|---|---|---|
| `crypto.randomUUID()` | Random IDs | `crypto.getRandomValues()`, which is available everywhere |
| `navigator.clipboard` | "Copy link" | A hidden text field + `document.execCommand('copy')` fallback |
| WebTransport | Faster transport than WebSocket | Not used; WebSocket works everywhere |

The general lesson: **test from a second device.** Code that works on `localhost` can fail on the LAN address, because `localhost` gets privileges the real deployment doesn't.

**Same-origin policy and CORS.** The dashboard service runs on port 8091, a different **origin** (scheme + host + port) from the app on 8090. By default a page may not read responses from another origin. The dashboard therefore sends `Access-Control-Allow-Origin: *` on `/api/viewers`, an explicit opt-in called **CORS** (cross-origin resource sharing) that allows pages from other origins to read it.

**Cross-site scripting (XSS).** Room names and titles are typed by hosts and displayed to everyone. If a title were `<img src=x onerror="…">` and the page inserted it as HTML, that script would run in every viewer's browser. Every user-supplied string is **escaped** (`&`, `<`, `>`, `"` and `'` become entities) before it's placed into HTML. The rule is simple and absolute: **text from users is text, never markup.**

**Path traversal.** The poster endpoint maps a movie ID to a file path. IDs are validated against a strict pattern (six hex characters) before touching the filesystem, so `?id=../../secret` can't escape the uploads folder. The same idea protects uploads (`filepath.Base`, chapter 6.1). **Validate identifiers against what they must look like, not against what they must not.**

## 11.6 Keeping pages fresh

The watch page refreshes the current stream's metadata every second (live edge, status, viewer counts) and the side list every 3 seconds. That's plain polling. Isn't that exactly what chapter 4 argued against? The difference is scale and cost. This is a few hundred bytes of metadata, and a second of staleness is harmless (except for the Live button, which fetches fresh, chapter 9.6). Video fragments are megabytes, where every millisecond of discovery delay counts. **Use the simplest mechanism that meets each requirement.** Push is worth its complexity for the video, not for a viewer count.

## Check your understanding

1. Why is "create or get" safer for clients than "create", and what makes it atomic?
2. When should an API return 409 rather than 400? Give an example from LANFLIX.
3. A feature works on `http://localhost:8090` but fails on a phone at `http://192.168.1.20:8090`. Name two possible reasons from this chapter.
4. Why does the dashboard need a CORS header, and why doesn't the main app?
5. A host names a room `<script>alert(1)</script>`. Trace what happens with and without escaping.
