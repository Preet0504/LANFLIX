# Chapter 1 — What we are building

Every system design starts the same way: before choosing any technology, pin down **what the system must do** (functional requirements), **how well it must do it** (non-functional requirements), and **roughly how big the numbers are** (estimates). Most bad designs are good answers to the wrong question. This chapter makes sure we're asking the right one.

## 1.1 The idea

Picture a house, a dorm, an office or a café where everyone shares one Wi-Fi network. Someone has a video file (a movie, a lecture recording, a match) and wants everyone else on the network to watch it together, from their own phone or laptop, without installing anything and without the internet.

We call this person the **host**, and the people watching the **viewers**. A host creates a **room** ("Friday Movie Night") and drops video files into it. Each file becomes a **stream** that starts playing immediately, as if it were a TV channel. Viewers open a link in their browser, see the rooms, pick a stream and watch.

The twist that makes this interesting is that a stream is **live and recorded at the same time**. The moment a host starts a stream, it plays forward in real time like a broadcast. But everything already broadcast stays available, so a viewer who arrives twenty minutes late can:

- start from the beginning and catch up,
- jump straight to the live point,
- rewind ten seconds to rewatch something,
- skip forward, but only as far as the broadcast has actually reached,
- close the tab and come back tomorrow to the exact second they left.

Television engineers call this combination **live with DVR** (digital video recorder). Most streaming systems are built for one or the other: a live system (a video call, a sports broadcast) cares about being as close to real time as possible and forgets the past, while a library system (a video-on-demand service) has the whole file ready and no "now". LANFLIX must be both. That tension shapes almost every decision in this book.

## 1.2 Functional requirements

Functional requirements are the things users can *do*. Writing them down precisely stops a design from quietly dropping one.

**Hosts can:**

1. Create a room with a name, or reopen an existing room by typing the same name. Room names are unique: typing "Movie Night" twice leads back to the same room, never to a duplicate.
2. Upload one or many video files into a room. Each becomes a live stream immediately. Any common format should work (MP4, MKV, MOV, WebM, and so on).
3. See, per room, how many streams are live, how many people are watching each one right now, and how many have ever watched.
4. Share a link that works on other devices on the network.
5. Delete a stream or a room, which frees everything it used.
6. Choose low-latency mode for a stream when being close to real time matters more than efficiency.

**Viewers can:**

1. Browse rooms and the streams in them, with a preview image for each.
2. Watch any stream from the beginning, or jump to live.
3. Pause, play, rewind, fast-forward within what has been broadcast so far, and seek anywhere in that range.
4. Leave and come back later (even after closing the browser) and resume at the same position.
5. See a "continue watching" list of streams they started.

## 1.3 Non-functional requirements

Non-functional requirements describe *qualities*. They're harder to state and usually drive the architecture far more than the features do.

| Quality | Target | Why it matters here |
|---|---|---|
| **Zero install** | Works in any modern browser, on any device on the network | Guests at a movie night won't install an app |
| **Fast start** | First frame within a fraction of a second of pressing play | Slow starts feel broken |
| **Low latency** | "Live" means seconds behind real time, not tens of seconds | Viewers in the same room shouldn't see a goal a minute apart |
| **Smooth playback** | No freezes during steady playback | The most noticeable failure of any video system |
| **Integrity** | Every byte arrives intact; no corrupted frames | Wi-Fi drops packets; the picture must not break up |
| **Scale** | Dozens to a couple of hundred concurrent viewers on one laptop | That's the size of a house, office or event |
| **Efficiency** | Don't send viewers much more video than they'll watch | Wi-Fi capacity is shared by everyone |
| **Resilience** | A viewer who drops off the network recovers without losing their place | Phones switch networks and sleep constantly |
| **Offline** | Nothing depends on the internet | The whole point is a local network |
| **Simple to run** | One machine, a few commands | The host is not a sysadmin |

**Out of scope**, deliberately: internet-scale distribution, content delivery networks, DRM (copy protection), multiple quality levels (adaptive bitrate), user accounts and authentication. A design that tries to serve every audience serves none of them well. Naming what we won't do is as important as naming what we will.

## 1.4 Back-of-the-envelope estimates

Estimates tell you which resources are tight and which are effectively free. They don't need to be precise, just within a factor of two or so.

**Bitrate.** A video's bitrate is how many bits per second it takes. For 720p video at 30 frames per second, compressed with H.264 (chapter 2), a typical bitrate is 2.5–3 megabits per second (Mb/s). We'll use 3 Mb/s. At 1080p it's about double.

**Network.** Every viewer needs their own copy of the stream, because we'll send it over TCP (chapter 4), which is one-to-one.

| Viewers | Total traffic at 3 Mb/s each |
|---|---|
| 10 | 30 Mb/s |
| 50 | 150 Mb/s |
| 200 | 600 Mb/s |

A typical home Wi-Fi access point delivers a few hundred Mb/s of real throughput, *shared by every device*. Gigabit Ethernet carries 1,000 Mb/s. So **the network, not the server, is the first bottleneck** once there are more than a hundred or so viewers. That's a useful thing to know before writing any code: we shouldn't over-engineer the server's raw throughput, and we should be careful never to waste bandwidth, for example by sending video a viewer will never watch.

**Memory.** 3 Mb/s is 0.375 megabytes per second, or about **1.35 GB per hour** of video. If we keep three hours of each stream available for rewinding, each stream costs about 4 GB. A laptop with 16 GB of RAM can hold a few such streams in memory at once, but not dozens. That tells us two things. First, keeping recent video in memory (chapter 7) is feasible. Second, retention needs a limit.

**Message rates.** If we cut video into 2-second chunks, a stream produces 0.5 chunks per second, and with 200 viewers the server sends 100 chunk messages per second. In low-latency mode, with 200 ms chunks, that becomes 1,000 per second. Both are small numbers for a modern server. The work per message (copying bytes to a socket) is cheap. The work we must avoid is anything that grows with *viewers × something*, such as every viewer asking the database for the same data.

**Requests.** In a polling design (chapter 4), each viewer asks "is there anything new?" every couple of seconds. Two hundred viewers polling every two seconds is 100 requests per second of *overhead*, even when nothing has changed. Low-latency polling schemes multiply this several times over. We'll measure it in chapter 13.

## 1.5 The questions the rest of the book answers

With requirements and numbers in hand, the design problem breaks into a sequence of questions:

1. **How do we represent video** so that small pieces of it can be sent and played independently? (Chapters 2 and 3)
2. **How do we get those pieces to a browser** quickly and reliably? What are the standard ways, and what do they trade off? (Chapter 4)
3. **Where do we keep the pieces** so that "give me everything from minute 12" and "give me the next piece the instant it exists" are both cheap? (Chapter 7)
4. **How does one server feed hundreds of viewers** who are each at different points in the same stream, without any of them slowing the others down? (Chapter 8)
5. **How do we remember where each viewer is**, and learn from what they do, without slowing down video delivery? (Chapter 10)
6. **How do we know it works**, and how do we compare it fairly against the alternatives? (Chapter 13)

## Check your understanding

1. Why is "live with DVR" harder than either pure live or pure on-demand? Name one requirement that each pure form doesn't have.
2. Using the estimates above, roughly how much memory does keeping the last hour of four simultaneous 1080p streams take?
3. Why does the estimate suggest that the network, not the server, will be the first bottleneck? What design rule follows from that?
4. Pick one out-of-scope item. What would change in the design if it were in scope?
