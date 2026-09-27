# Chapter 10 — Events and Kafka

Video bytes are one kind of data. **What viewers do** is another: who is watching what, and how far they got. It's small, but it has consumers the video path doesn't: a live dashboard, an analytics service, and in future, services that don't exist yet. This chapter explains event logs and Kafka, and why LANFLIX records viewer positions in *two* places.

## 10.1 State versus events

There are two ways to record that a viewer is at 5:12:

- **State**: overwrite a value. `resume:<movie>:<viewer> = 312000`. You always know the *latest* position, and the past is gone.
- **Events**: append a fact. `{viewer, movie, position: 312000, time: 20:41:07}`. You can reconstruct the latest position by reading the newest event, and you can also answer questions about the past: how many people reached minute 12, where do viewers stop watching, which streams do people finish.

State answers "what is true now" cheaply. Events answer "what happened" completely. Many systems need both, and the design question is which consumers read which. LANFLIX writes each position report to both:

| Consumer need | Store | Why |
|---|---|---|
| "Where should this viewer resume?" | **Redis** key, overwritten | One lookup; only the latest value matters |
| "Who's watching right now?" | **Kafka** → dashboard's memory | Built from recent events |
| "How many ever watched, and how far?" | **Kafka** → analytics → Redis sorted set | Needs the *full* history |

## 10.2 What Kafka is

**Apache Kafka** is a distributed, durable **event log**. The core concepts:

- A **topic** is a named log of messages, like `viewer-positions`. Producers append to the end; messages are never modified.
- A topic is split into **partitions**, independent ordered logs that can live on different servers. Order is guaranteed *within* a partition, not across partitions. A message's **key** chooses its partition (by hash), so all messages with the same key (say, the same viewer) stay in order.
- Each message in a partition has an **offset**, its position: 0, 1, 2, …
- A **broker** is a Kafka server that stores partitions on disk. Messages are kept for a configured **retention** period (days, by default), whether or not anyone has read them.
- **Consumers** read by offset, at their own pace. A consumer can re-read old messages by resetting its offset.
- A **consumer group** is a set of consumers sharing work. Each partition is read by exactly one member of the group, and the group **commits** its offsets to Kafka so a restarted member continues where the group left off. **Different groups are completely independent**: each gets the entire topic and has its own offsets.

That last property is the reason to use Kafka at all. Two different services read the same topic without knowing about each other, each at its own pace, and a third can be added tomorrow and read the history from the start, without touching the producer or the other two.

Kafka traditionally needed a separate coordination service (ZooKeeper). Since version 3.3 it can run in **KRaft** mode, managing its own metadata with the Raft consensus algorithm. LANFLIX's Docker setup runs a single broker in KRaft mode: one container, no ZooKeeper.

## 10.3 Producing: fire and forget

The server publishes one event per position report:

```json
{ "client_id": "9f3e…", "movie_id": "c5d682", "position_ms": 312000, "event_time_ms": 1790500867000 }
```

The producer is configured for the realities of this data:

- **Asynchronous.** Publishing doesn't wait for Kafka to confirm. Playback must never slow down because the analytics path is slow. A lost event costs little: another arrives 2 s later.
- **`RequiredAcks: one`.** When the broker confirms a write, only the partition leader has to have stored it. That's the right durability level for telemetry. (A payment system would wait for all replicas.)
- **Keyed by viewer.** Each event carries the client ID as its key. With several partitions, a key-hashing partitioner keeps each viewer's events in order in one partition while spreading viewers across partitions. (The current single-partition topic is totally ordered anyway; the key is what keeps ordering per viewer once the topic is split for scale.)

The position report also goes to the Redis resume key, through the ordered per-connection writer from chapter 8.8. The same fact goes into two stores for two access patterns.

## 10.4 Consumer 1: the live dashboard

`cmd/dashboard` is a separate process in its own consumer group, `dashboard`. For each event it updates an in-memory map `client → {movie, position, last seen}`, and it serves that map at `/api/viewers`. Anyone not heard from in 10 s (five missed reports) is considered gone. The host page and room pages call this endpoint to show "3 watching" on each stream.

This is a **materialized view**: a read-optimized structure derived from the event log. It lives in memory and can be rebuilt at any time by re-reading recent events.

## 10.5 Consumer 2: analytics and replay

`cmd/analytics` is another process, in another group, `analytics`. For each event it records the furthest point that viewer has reached in that stream:

```
ZADD analytics:<movie>:reach GT <position_ms> <client_id>
```

From that sorted set, two questions become single commands:

- **Total distinct viewers ever:** the size of the set (`ZCARD`).
- **Audience retention, "how many viewers reached minute 12?":** count the members with a score ≥ 720,000 (`ZCOUNT … 720000 +inf`). Evaluated at every minute, that's a retention curve: the classic chart of where an audience drops off.

This consumer shows three properties of log-based design:

- **Replay.** A new consumer group starts from the *beginning* of the topic. Deploy the analytics service a week after launch, and it computes a week of history on its first run. Resume points in Redis could never provide this: they only hold the latest value per viewer.
- **Independence.** Adding analytics required no change to the server or the dashboard. They don't know it exists.
- **Idempotence.** Kafka delivers **at least once**: after a crash, a consumer may re-read events it already processed but hadn't committed yet. `ZADD GT` keeps the maximum, so processing an event twice, or out of order, gives the same result (chapter 7.8). With at-least-once delivery, **make every consumer idempotent**; that's what makes the whole pipeline effectively exactly-once.

## 10.6 Why not just Redis? Why not just Kafka?

**Why not use Redis for everything?** Redis Streams are also logs, and could carry these events. But the durable, replayable, multi-consumer event history is Kafka's core purpose. It's on disk, with retention in days and consumer groups with committed offsets, and it grows without eating RAM needed for video. Redis here holds hot, memory-resident, latest-value data.

**Why not use Kafka for resume points?** Answering "where did viewer X stop?" from Kafka means scanning for X's newest event. That's the wrong shape for a query that must answer in milliseconds when a page opens. It's a key lookup, which is what Redis is for.

**Why not use Kafka for video?** Covered in chapter 7.9: delivery needs millisecond hand-off and time-indexed random access more than durability.

The general pattern, a fast **hot path** for serving and a durable **event log** feeding separately evolving **cold path** consumers, is how large streaming services structure their data. Playback never depends on analytics being up. Analytics sees everything playback did.

## 10.7 What happens when Kafka is down

Because publishing is asynchronous and nothing in playback reads from Kafka, a Kafka outage means only that the dashboard's counts go stale and analytics pauses. Video keeps flowing and resume points keep saving, because those go to Redis. When Kafka returns, consumers continue from their committed offsets.

> **Principle: decide, per dependency, whether the hot path needs it.** Write down which features degrade when each component fails. If the answer for a non-essential component is "everything stops", the dependency is in the wrong place.

## Check your understanding

1. Give one question that can only be answered from events, not from latest-value state, and one that's much cheaper to answer from state.
2. Two consumer groups read the same topic. If one crashes for an hour, what happens to the other? What happens to the crashed one when it restarts?
3. Why does the producer publish asynchronously, and what does it give up?
4. Why must a Kafka consumer be idempotent? Show why `ZADD GT` is, and give an example of an update that wouldn't be.
5. With the retention sorted set, write the command for "how many viewers watched at least half of a 90-minute stream?"
