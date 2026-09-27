# Chapter 4 — Getting video from A to B

We know what to send (fMP4 fragments) and how the browser plays it (MSE). This chapter is about how the bytes *travel*. It surveys the standard approaches, HLS, DASH, Low-Latency HLS, WebRTC and multicast UDP, and extracts the few dimensions along which they really differ. Knowing the alternatives thoroughly is what lets you justify choosing a different one.

## 4.1 The two transports: TCP and UDP

Everything on an IP network travels in **packets** of up to about 1,500 bytes. Two transport protocols sit on top of IP:

**UDP** (User Datagram Protocol) sends individual packets, called datagrams, and nothing else. There's no connection, no acknowledgement, no retransmission and no ordering. A lost packet is simply gone. UDP is fast and simple and supports **multicast**, where one packet sent to a group address is delivered by the network to every member.

**TCP** (Transmission Control Protocol) builds a reliable, ordered **byte stream** between two endpoints on top of IP:

- Every byte is acknowledged, and lost packets are **retransmitted**.
- Bytes are delivered **in order**, and the application never sees a gap.
- **Congestion control** slows the sender down when the network is overloaded.
- **Flow control** stops a sender from overwhelming a slow receiver, through the receiver's advertised window.

The price is that loss turns into **delay**. If packet 5 is lost, packets 6–20 wait at the receiver until 5 is retransmitted, because TCP must deliver in order. This is called **head-of-line blocking**.

For video this is *the* fundamental trade-off:

| | TCP | UDP |
|---|---|---|
| A lost packet becomes… | a short delay (retransmission) | missing data: corrupted or frozen frames until the next keyframe |
| Latency | slightly higher, with spikes under loss | minimal |
| Delivery to N receivers | N copies (unicast) | 1 copy with multicast |
| Needs the app to handle | nothing: bytes arrive intact | loss, reordering, jitter |

Wi-Fi commonly loses 1% or more of packets under load. Raw UDP streaming turns that directly into picture corruption: chapter 13 measures 167 decoder errors per minute at 1% loss. That's why every approach below except raw multicast either uses TCP or rebuilds reliability on top of UDP.

## 4.2 HTTP streaming: HLS and DASH

The dominant way to stream video on the internet is to cut it into fMP4 **segments**, publish them as ordinary files on an HTTP server, and publish an index of the segments, called a **manifest**. The player downloads the manifest, then the segments, over normal HTTP (on TCP). There are two standards.

**HLS** (HTTP Live Streaming, from Apple) uses a text **playlist** (`.m3u8`):

```
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:2
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-MAP:URI="init.mp4"
#EXTINF:2.000,
seg0.m4s
#EXTINF:2.000,
seg1.m4s
#EXTINF:2.000,
seg2.m4s
```

For a live stream, the playlist grows as segments are added. The player **re-downloads the playlist periodically** (about once per target duration) to discover new segments, then fetches each new one. An `EVENT` playlist keeps every segment from the start, which enables rewinding; a sliding-window playlist keeps only the most recent few.

**DASH** (Dynamic Adaptive Streaming over HTTP, an international standard) uses an XML manifest (the **MPD**). Instead of listing each segment, it usually gives a **template** (`seg-$Number$.m4s`) and a timeline. The player computes, from the clock and the manifest's `availabilityStartTime`, which segment *should* exist now, and requests it.

Both were designed for the internet, and their design choices make sense there:

- **Everything is a plain file over HTTP**, so any web server, cache or **CDN** (content delivery network) can serve it. A CDN can serve millions of viewers because it caches identical files close to them.
- **Adaptive bitrate** (ABR): the manifest can list the same video at several qualities, and the player switches between them as the network allows.
- **Native support**: iPhones play HLS natively, with no JavaScript. Elsewhere, libraries such as **hls.js** and **dash.js** implement the players on top of MSE.

What this design costs is **discovery latency**. The player only learns a segment exists when it next polls (HLS) or when its clock says so (DASH). Players also keep a safety margin behind the newest segment: hls.js by default starts **three segments** (6 s at 2 s segments) behind live, so that a late segment doesn't cause a stall. Every live HLS/DASH viewer also generates a constant stream of HTTP requests, even when nothing new exists.

## 4.3 Low-Latency HLS

**LL-HLS** (Low-Latency HLS) attacks those delays with three mechanisms, and understanding them explains a surprising result in chapter 13:

1. **Partial segments ("parts")**. Each 2 s segment is also published in pieces as small as 200 ms (`#EXT-X-PART`), each listed as soon as it's encoded. Players download parts instead of waiting for whole segments.
2. **Blocking playlist reload**. The player asks for the playlist *with a condition*: `index.m3u8?_HLS_msn=41&_HLS_part=3` means "reply when segment 41, part 3 exists". The server **holds the request open** until then. Polling becomes a long-poll: the reply arrives the instant the part exists.
3. **Preload hints**. The playlist names the *next* part before it exists (`#EXT-X-PRELOAD-HINT`). The player requests it right away, and the server holds that request until the part is produced, then streams it back.

A long-held HTTP request that completes the moment data exists is, in effect, **server push built from requests**. LL-HLS also keeps a smaller safety margin, `PART-HOLD-BACK`, typically three parts (0.6 s), and supports **delta playlists** (`_HLS_skip`) so the player doesn't re-download hours of history five times a second. The cost is request volume: roughly two requests per part, five parts per second, *per viewer*.

(**LL-DASH** does something similar with chunked HTTP transfer of segments still being written. It isn't covered further here.)

## 4.4 WebRTC

**WebRTC** is the browser's real-time communication stack, built for video calls. It's designed around one goal: **minimum delay**.

- **Transport**: media travels as **RTP** packets (Real-time Transport Protocol) over **UDP**. Each packet carries a sequence number and a timestamp, and is encrypted with SRTP.
- **Connection setup** ("signaling"): the two sides exchange **SDP** (Session Description Protocol) descriptions, an offer and an answer listing codecs and addresses. Then **ICE** tries candidate network paths until one works. The web standard doesn't define how the SDP is exchanged. A common HTTP convention for "watch this stream" is **WHEP**: POST your offer, receive the answer.
- **Loss handling**: instead of TCP's retransmit-everything, WebRTC uses selective repair. The receiver can request a lost packet again (**NACK**) if there's still time to use it, and a **jitter buffer** holds packets briefly (typically tens of milliseconds) to smooth out uneven arrival. When the picture can't be repaired, the receiver sends a **PLI** (picture loss indication) asking the sender for a new keyframe.
- **Adaptation**: the sender continuously adjusts the encoder's bitrate to the measured network capacity.
- **Codecs**: H.264 without B-frames (or VP8, VP9, AV1) for video, and **Opus** for audio.
- **Many viewers**: a server that receives one stream and forwards it to many viewers is called an **SFU** (selective forwarding unit).

WebRTC reaches glass-to-glass delays of around 100 ms. But everything about it is built around *now*. There's no concept of a timeline you can seek in, no rewinding, no resuming tomorrow. A joining viewer must wait for (or request) a keyframe, and an SFU needs per-viewer encryption and packet-level work. For a live call it's the right tool. For live-with-DVR it covers only the "live" half.

## 4.5 Multicast UDP

On a LAN, one could send each packet **once** to a multicast group, and the switch or access point delivers it to every subscribed device. Network traffic stays constant no matter how many viewers there are, which is multicast's great strength. Traditionally the payload is MPEG-TS, a container designed for broadcast. But multicast inherits all of raw UDP's problems (no recovery from loss), browsers can't receive it at all, and Wi-Fi handles multicast poorly (it's often sent at the lowest data rate). And, like WebRTC, it has no past: a late joiner never gets what was already sent.

## 4.6 WebSockets

A **WebSocket** is a long-lived, full-duplex message channel between a browser and a server, over TCP. It starts as an ordinary HTTP request with an `Upgrade: websocket` header. If the server agrees (status `101 Switching Protocols`), the same TCP connection becomes a two-way message pipe:

- Either side can send a message at any time. There's no request/response pairing.
- Messages are **binary** or **text**, and message boundaries are preserved: one `send` equals one receive.
- It works in every browser, over plain HTTP on a LAN, with no special network configuration.

A WebSocket gives the server what HTTP polling lacks: the ability to **push** data the moment it exists, with TCP's reliability and ordering. And because it's two-way, the same connection carries the client's control messages ("seek to 12:34", "I'm at 5:02") back to the server.

## 4.7 Push versus pull

The approaches above differ along a few independent dimensions. Separating them is the most useful idea in this chapter.

| Dimension | Options |
|---|---|
| **Transport** | TCP (reliable, ordered; loss becomes delay) vs UDP (timely; loss becomes damage unless repaired) |
| **Who initiates each transfer** | **Pull**: the client asks (HLS, DASH, LL-HLS) vs **Push**: the server sends when ready (WebSocket, WebRTC, multicast) |
| **Unit of delivery** | Whole segments (2 s) · parts or fragments (200 ms) · individual packets |
| **Safety margin** | How far behind the newest media the player deliberately stays |
| **History** | Can a viewer go back in time? (HLS/DASH EVENT, LANFLIX: yes; WebRTC, multicast: no) |

## 4.8 The latency budget

**Glass-to-glass latency** is the time from a frame being captured (for us, encoded) to it appearing on a viewer's screen. It's a sum of stages, and each design choice from the table above controls one of them:

```
 encode ─► package ─► discover ─► transfer ─► safety margin ─► decode & show
   ~0       ≤ chunk     poll/push    ~ms on a     chosen by       ~tens of ms
            duration    delay        LAN          the player
```

- **Package**: a chunk can't be sent until its last frame is encoded, so this costs up to one chunk duration: 2 s for whole segments, 0.2 s for parts, about 0 for packets.
- **Discover**: how long before the viewer's side learns the chunk exists. With polling, up to a polling interval; with blocking requests or push, about 0.
- **Transfer**: on a LAN, milliseconds.
- **Safety margin**: the buffer the player keeps against jitter. 6 s for default HLS, one segment for a tuned player, 0.6 s for LL-HLS, tens of milliseconds for WebRTC's jitter buffer.

This decomposition predicts the benchmark results before any are measured. **Chunk duration and the safety margin dominate.** Push versus pull only affects the *discover* term. With equal chunk sizes and margins, a push system and a well-built long-polling system (LL-HLS) should land close together, and only packet-level delivery (WebRTC) can get far below them. Chapter 13 confirms exactly this.

## 4.9 Where LANFLIX sits, and why

LANFLIX sends **fMP4 fragments** (like HLS and DASH), **pushed** over a **WebSocket** (TCP), with a **DVR timeline** kept on the server. Each choice follows from the requirements in chapter 1:

| Requirement | Choice | Rejected alternative, and why |
|---|---|---|
| Rewind, resume, join late | A server-side timeline of fragments | WebRTC and multicast have no past |
| Integrity on lossy Wi-Fi | TCP | Raw UDP corrupts frames at 1% loss |
| Zero install, any browser | MSE + WebSocket | Multicast can't reach browsers |
| Fast start, low overhead | Push: one connection, data arrives as it exists | Polling costs a request stream per viewer and adds discovery delay |
| LAN, not internet | A single origin server | CDN compatibility (HLS/DASH's big advantage) isn't needed |
| Close to live when wanted | Optional 200 ms fragments | Only WebRTC goes lower, and it gives up the DVR |

Just as important is what this choice gives up: CDN caching, adaptive bitrate, native iPhone playback and sub-200 ms latency. None of those are requirements here. A design is always a set of trade-offs; the skill is choosing them deliberately.

## Check your understanding

1. Why does TCP turn packet loss into delay rather than corruption? What mechanism causes head-of-line blocking?
2. In HLS, what two separate delays happen between a segment being written and a player starting to download it?
3. Explain how LL-HLS's preload hints make it behave like push, and what it pays for that.
4. Using the latency budget, predict which of these improves latency most for a 2 s-segment system, and why: (a) making delivery push-based, or (b) cutting segments to 200 ms.
5. List two requirements from chapter 1 that WebRTC can't meet, and one it meets better than anything else.
