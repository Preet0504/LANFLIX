"""Turn raw benchmark output into bench/results/summary.json.

Inputs (all produced by the benchmark tools):
  browser.json   bench/run.js — per run: TTFF, every presented frame, segment
                 receipts, seeks, stalls, origin requests
  timeline.json  bench/source — per segment, when it became available on each
                 system (ours: XADD returned; HLS: in playlist; DASH: in MPD)
  load.json      bench/load  — our origin under N concurrent viewers
  udp.json       bench/udp   — MPEG-TS over UDP under simulated loss

Definitions (applied identically to every system):
  ttff_ms         player told to start → first frame presented on screen
  latency_ms      glass-to-glass: frame presented − the moment the encoder
                  produced it. Encoder runs at real time (-re), so the frame at
                  media time m is produced at encoder_start + m. encoder_start is
                  calibrated from the earliest segment availability across ALL
                  systems (the encoder's own output timing), not from any one.
  delivery_ms     segment available at the origin → its bytes at the viewer,
                  for segments that became available while the viewer was
                  already watching (steady state; excludes startup & seeks).
  seek_ms         seek issued → first frame near the target presented.
"""

import json
import statistics as st
from pathlib import Path

R = Path(__file__).parent / "results"


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    k = (len(xs) - 1) * p
    lo, hi = int(k), min(int(k) + 1, len(xs) - 1)
    return xs[lo] + (xs[hi] - xs[lo]) * (k - lo)


def stats(xs):
    xs = [x for x in xs if x is not None]
    if not xs:
        return None
    return {
        "n": len(xs), "median": st.median(xs), "mean": st.fmean(xs),
        "p10": pct(xs, .10), "p90": pct(xs, .90), "p95": pct(xs, .95),
        "min": min(xs), "max": max(xs),
    }


def main():
    runs = json.loads((R / "browser.json").read_text())
    tl = json.loads((R / "timeline.json").read_text())
    gop = tl["gop_ms"]
    avail = {
        "ours": {int(k): v for k, v in tl["ours"].items()},
        "hls": {int(k): v for k, v in tl["hls"].items()},
        "dash": {int(k): v for k, v in tl["dash"].items()},
    }

    # Encoder output timing: segment n is complete once the encoder has
    # produced media up to (n+1)*gop. The earliest availability across all
    # systems bounds when that happened; its minimum offset from t0 is the
    # encoder's startup delay.
    offsets = []
    for n in avail["ours"]:
        first = min(avail[s][n] for s in avail if n in avail[s])
        offsets.append(first - tl["t0_ms"] - (n + 1) * gop)
    encoder_start = tl["t0_ms"] + min(offsets)

    # Sanity: every system should agree on segment boundaries (same encoder).
    agree = {s: len(avail[s]) for s in avail}

    # Origin request log from the harness (authoritative). Runs were strictly
    # sequential, so a run's requests are exactly those inside its window.
    req_log = None
    if (R / "requests.json").exists():
        req_log = json.loads((R / "requests.json").read_text())
        for r in runs:
            base = r["system"].split("-")[0]
            mine = [q for q in req_log if q["system"] == base and r["wallStart"] <= q["at"] <= r["wallEnd"]]
            r["httpRequests"] = len(mine)
            r["httpManifestRequests"] = sum(q["kind"] == "manifest" for q in mine)

    configs = []
    order = ["ours", "hls", "hls-tuned", "dash", "dash-tuned"]
    by = {k: [r for r in runs if r["system"] == k] for k in order}
    out = {"configs": [], "meta": {}}

    for key in order:
        rs = by[key]
        if not rs:
            continue
        base = key.split("-")[0]
        ttff, lat_run_medians, lat_all, delivery, seeks = [], [], [], [], []
        stalls, stall_ms, reqs_per_min, manifest_per_min, failures = [], [], [], [], 0
        segs_fetched = []
        for r in rs:
            if r.get("ttffMs") is None:
                failures += 1
                continue
            ttff.append(r["ttffMs"])

            s0, s1 = r["sampleStart"], r["sampleEnd"]
            lats = [shown - (encoder_start + m * 1000)
                    for shown, m in r["frames"] if s0 + 3000 <= shown <= s1]
            if lats:
                lat_run_medians.append(st.median(lats))
                lat_all.extend(lats[::10])

            for n, got in r["receipts"].items():
                n = int(n)
                a = avail[base].get(n)
                if a is not None and a >= r["tStart"] and got <= s1:
                    delivery.append(got - a)

            seeks += [s["ms"] for s in r["seeks"] if s["ms"] is not None]
            segs_fetched.append(len(r["receipts"]))
            stalls.append(r["stalls"])
            stall_ms.append(r["stallMs"])
            minutes = (r["wallEnd"] - r["wallStart"]) / 60000
            if base != "ours":
                reqs_per_min.append(r["httpRequests"] / minutes)
                manifest_per_min.append(r["httpManifestRequests"] / minutes)
            else:
                # One WebSocket upgrade per session; seeks and position pings
                # ride the same socket. Zero polling.
                reqs_per_min.append(1 / minutes)
                manifest_per_min.append(0)

        out["configs"].append({
            "key": key, "label": rs[0]["label"],
            "runs": len(rs), "failures": failures,
            "ttff_ms": ttff, "ttff": stats(ttff),
            "latency_run_medians_ms": lat_run_medians, "latency": stats(lat_run_medians),
            "latency_samples_ms": lat_all,
            "delivery_ms": delivery, "delivery": stats(delivery),
            "seek_ms": seeks, "seek": stats(seeks),
            "stalls_per_run": stats(stalls), "stall_ms_per_run": stats(stall_ms),
            "requests_per_min": stats(reqs_per_min),
            "segments_fetched_per_session": stats(segs_fetched),
            "manifest_requests_per_min": stats(manifest_per_min),
        })

    out["meta"] = {
        "trials": max(r["trial"] for r in runs),
        "segments_per_system": agree,
        "encoder_startup_ms": min(offsets),
        "gop_ms": gop,
    }

    # Seek latency by position within a session (1st..4th seek): tests whether
    # in-flight backfill from an earlier seek delays the next one.
    out["seek_by_position"] = {}
    for key in order:
        rs = by[key]
        cols = [[r["seeks"][i]["ms"] for r in rs if len(r["seeks"]) > i and r["seeks"][i]["ms"] is not None] for i in range(4)]
        out["seek_by_position"][key] = [st.median(c) if c else None for c in cols]

    load_path = R / "load.json"
    if load_path.exists():
        load = json.loads(load_path.read_text())
        out["load"] = [{
            "n": s["n"], "p50": s["P50"], "p95": s["P95"], "p99": s["P99"], "max": s["Max"],
            "received": s["segments_received"], "expected": s["segments_expected"],
            "hash_mismatches": s["hash_mismatches"], "connect_failures": s["connect_failures"],
            "server_cpu_pct": s["server_cpu_pct"], "client_cpu_pct": s.get("client_cpu_pct"),
            "aggregate_mbps": s.get("aggregate_mbps"), "delivered": s.get("segments_delivered"),
        } for s in load]
    before = R / "load_before_hub.json"
    if before.exists():
        out["load_before_hub"] = json.loads(before.read_text())

    udp_path = R / "udp.json"
    if udp_path.exists():
        out["udp"] = json.loads(udp_path.read_text())

    (R / "summary.json").write_text(json.dumps(out, indent=1))

    print(f"encoder startup offset: {min(offsets)} ms   segments per system: {agree}")
    print(f"{'config':12} {'runs':>4} {'fail':>4} {'TTFF med':>9} {'lat med':>8} {'deliv med':>9} {'deliv p95':>9} {'seek med':>8} {'stalls':>6} {'req/min':>7} {'segs/sess':>9}")
    for c in out["configs"]:
        f = lambda s, k="median": "—" if not s else f"{s[k]:.0f}"
        print(f"{c['key']:12} {c['runs']:>4} {c['failures']:>4} {f(c['ttff']):>9} {f(c['latency']):>8} "
              f"{f(c['delivery']):>9} {f(c['delivery'], 'p95'):>9} {f(c['seek']):>8} "
              f"{f(c['stalls_per_run'], 'mean'):>6} {f(c['requests_per_min']):>7} {f(c['segments_fetched_per_session']):>9}")
    for s in out.get("load", []):
        print(f"load n={s['n']:<4} p50={s['p50']:.0f} p95={s['p95']:.0f} p99={s['p99']:.0f}ms "
              f"delivered {s['delivered']}/{s['expected']} mismatches={s['hash_mismatches']} cpu={s['server_cpu_pct']:.1f}% {s['aggregate_mbps']:.0f}Mbps")
    for u in out.get("udp", []):
        print(f"udp loss {u['loss_pct']}%: frames lost {u['frame_loss_pct']:.1f}%  cc errors {u['continuity_errors']}  decode errors {u['decode_errors']}")


if __name__ == "__main__":
    main()
