"""Turn raw benchmark output into bench/results/summary.json.

Inputs (all produced by the benchmark tools):
  browser.json   bench/run.js — per run: TTFF, every presented frame, receipts
                 of each delivery unit, seeks, stalls, WebRTC stats
  timeline.json  bench/source — per delivery unit, when it became available on
                 each system (2s systems: per segment — ours: XADD returned;
                 HLS: in playlist; DASH: in MPD. 200ms systems: per fragment —
                 ours-ll: XADD returned; LL-HLS: servable as a part), plus the
                 encoder's output clock measured from RTP
  requests.json  bench/source — every viewer request to the HTTP origin
  load.json      bench/load  — our origin under N concurrent viewers
  udp.json       bench/udp   — MPEG-TS over UDP under simulated loss

Definitions (applied identically to every system):
  ttff_ms         player told to start → first frame presented on screen
  latency_ms      glass-to-glass: frame presented − the moment the encoder
                  produced it. The encoder runs at real time (-re), so the frame
                  at media time m is produced at encoder_start + m. encoder_start
                  comes from the earliest arrival of any frame's RTP packets,
                  which ffmpeg sends the moment a frame is encoded (no muxing or
                  segmenting) — the encoder's own output clock, not any system's.
  delivery_ms     unit available at the origin → its bytes at the viewer, for
                  units that became available while the viewer was already
                  watching (steady state; excludes startup & seeks). A unit is a
                  2s segment or a 200ms fragment; WebRTC has none (per packet).
  freezes         gaps of more than 250ms between consecutive presented frames
                  during the steady-state window (30fps content: ~7 frames).
                  Measured from the frames themselves, so it applies to every
                  player, WebRTC included.
  seek_ms         seek issued → first frame near the target presented.
  media_s         seconds of media downloaded per session (units × duration).
"""

import json
import os
import statistics as st
from pathlib import Path

R = Path(os.environ.get("BENCH_RESULTS", Path(__file__).parent / "results"))

ORDER = ["ours", "ours-ll", "hls", "hls-tuned", "ll-hls", "dash", "dash-tuned", "webrtc"]
AVAIL = {"ours": "ours", "ours-ll": "ours_ll", "hls": "hls", "hls-tuned": "hls", "ll-hls": "llhls",
         "dash": "dash", "dash-tuned": "dash", "webrtc": None}
UNIT_S = {"ours": 2, "ours-ll": 0.2, "hls": 2, "hls-tuned": 2, "ll-hls": 0.2, "dash": 2, "dash-tuned": 2}
ORIGIN = {"hls": "hls", "hls-tuned": "hls", "ll-hls": "llhls", "dash": "dash", "dash-tuned": "dash", "webrtc": "webrtc"}
FREEZE_MS = 250


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


CLOCK_PCTL = 0.01


def encoder_clock(tl):
    """Wall-clock ms at which the encoder produced media time 0.

    Each frame's RTP arrival minus its timestamp is when the encoder emitted
    it. The lower envelope of those is the encoder's schedule; frames above
    it were merely encoded or sent a little late. A plain minimum is wrong
    here: where the looped source restarts, ffmpeg's real-time pacing lets
    a handful of frames out early (~0.3% of frames, up to ~300 ms early), so
    the 1st percentile is used. Returns (encoder_start, diagnostics).
    """
    a, c = tl["webrtc"], tl["encoder_clock"]
    offs = sorted(c["off_ms"])
    lo = pct(offs, CLOCK_PCTL)
    anchor_rel = ((a["rtp_ts"] - c["ts0"]) % 2**32) / 90  # anchor frame, ms after the first RTP frame
    start = c["off_base_ms"] + lo - a["media_ms"] + anchor_rel
    diag = {"frames": len(offs), "percentile": CLOCK_PCTL, "min_below_pctl_ms": offs[0] - lo,
            "median_above_pctl_ms": pct(offs, .5) - lo, "p0_1_below_pctl_ms": pct(offs, .001) - lo}
    return start, diag


def segment_calibration(tl):
    """The previous method (kept as a cross-check): earliest 2s-segment availability."""
    gop = tl["gop_ms"]
    offs = []
    for k in tl["ours"]:
        first = min(tl[s][k] for s in ("ours", "hls", "dash") if k in tl[s])
        offs.append(first - tl["t0_ms"] - (int(k) + 1) * gop)
    return tl["t0_ms"] + min(offs)


def main():
    runs = json.loads((R / "browser.json").read_text())
    tl = json.loads((R / "timeline.json").read_text())
    encoder_start, clock_diag = encoder_clock(tl)
    avail = {k: {int(n): v for n, v in tl[v_key].items()} for k, v_key in AVAIL.items() if v_key}

    # Causality check: no fragment may be available before its last frame
    # was encoded. A negative low percentile would mean the clock is wrong.
    frag_lead = [tl["llhls"][str(i)] - (encoder_start + f["start_ms"] + f["dur_ms"])
                 for i, f in enumerate(tl["ll_frags"]) if str(i) in tl["llhls"]]

    # Origin request log (authoritative). Runs were strictly sequential, so a
    # run's requests are exactly those inside its window.
    req_log = json.loads((R / "requests.json").read_text()) if (R / "requests.json").exists() else []
    for r in runs:
        base = ORIGIN.get(r["system"])
        mine = [q for q in req_log if base and q["system"] == base and r["wallStart"] <= q["at"] <= r["wallEnd"]]
        r["httpRequests"] = len(mine)
        r["httpManifestRequests"] = sum(q["kind"] == "manifest" for q in mine)

    by = {k: [r for r in runs if r["system"] == k] for k in ORDER}
    out = {"configs": [], "meta": {}}

    for key in ORDER:
        rs = by[key]
        if not rs:
            continue
        ttff, lat_run_medians, lat_all, delivery, seeks = [], [], [], [], []
        stalls, stall_ms, freezes, freeze_ms = [], [], [], []
        reqs_per_min, manifest_per_min, media_s, failures = [], [], [], 0
        rtc = []
        for r in rs:
            if r.get("ttffMs") is None:
                failures += 1
                continue
            ttff.append(r["ttffMs"])

            s0, s1 = r["sampleStart"], r["sampleEnd"]
            lats = [shown - (encoder_start + m * 1000) for shown, m in r["frames"] if s0 + 3000 <= shown <= s1]
            if lats:
                lat_run_medians.append(st.median(lats))
                lat_all.extend(lats[::10])

            shown = [f[0] for f in r["frames"] if s0 <= f[0] <= s1]
            gaps = [b - a for a, b in zip(shown, shown[1:]) if b - a > FREEZE_MS]
            freezes.append(len(gaps))
            freeze_ms.append(sum(gaps))

            if key in avail:
                for n, got in r["receipts"].items():
                    a = avail[key].get(int(n))
                    if a is not None and a >= r["tStart"] and got <= s1:
                        delivery.append(got - a)

            seeks += [s["ms"] for s in r["seeks"] if s["ms"] is not None]
            if key in UNIT_S:
                media_s.append(len(r["receipts"]) * UNIT_S[key] + (r.get("fullSegments") or 0) * 2)
            stalls.append(r["stalls"])
            stall_ms.append(r["stallMs"])
            minutes = (r["wallEnd"] - r["wallStart"]) / 60000
            if key in ORIGIN:
                reqs_per_min.append(r["httpRequests"] / minutes)
                manifest_per_min.append(r["httpManifestRequests"] / minutes)
            else:
                # One WebSocket upgrade per session; seeks and position pings
                # ride the same socket. Zero polling.
                reqs_per_min.append(1 / minutes)
                manifest_per_min.append(0)
            if r.get("rtc"):
                rtc.append(r["rtc"])

        cfg = {
            "key": key, "label": rs[0]["label"],
            "runs": len(rs), "failures": failures,
            "ttff_ms": ttff, "ttff": stats(ttff),
            "latency_run_medians_ms": lat_run_medians, "latency": stats(lat_run_medians),
            "latency_samples_ms": lat_all,
            "delivery_ms": delivery, "delivery": stats(delivery),
            "seek_ms": seeks, "seek": stats(seeks),
            "stalls_per_run": stats(stalls), "stall_ms_per_run": stats(stall_ms),
            "freezes_per_run": stats(freezes), "freeze_ms_per_run": stats(freeze_ms),
            "freezes_by_run": freezes,
            "requests_per_min": stats(reqs_per_min),
            "manifest_requests_per_min": stats(manifest_per_min),
            "media_s_per_session": stats(media_s),
        }
        if rtc:
            cfg["webrtc"] = {
                "connect_ms": stats([r.get("connectedMs") for r in rs]),
                "jitter_buffer_ms": stats([x["jitterBufferDelayMs"] for x in rtc]),
                "freeze_count": stats([x["freezeCount"] for x in rtc]),
                "packets_lost": stats([x["packetsLost"] for x in rtc]),
                "frames_dropped": stats([x["framesDropped"] for x in rtc]),
                "pli_count": stats([x["pliCount"] for x in rtc]),
            }
        out["configs"].append(cfg)

    out["meta"] = {
        "trials": max(r["trial"] for r in runs),
        "units_per_system": {k: len(v) for k, v in avail.items()},
        "encoder_start_vs_segment_calibration_ms": encoder_start - segment_calibration(tl),
        # p1, not min: the same loop-boundary frames that are emitted early
        # make their fragments look early too.
        "fragment_available_after_encode_ms": {"p1": pct(frag_lead, .01), "median": st.median(frag_lead)},
        "encoder_clock": clock_diag,
        "gop_ms": tl["gop_ms"],
        "freeze_threshold_ms": FREEZE_MS,
    }

    # Seek latency by position within a session (1st..4th seek): tests whether
    # in-flight backfill from an earlier seek delays the next one.
    out["seek_by_position"] = {}
    for key in ORDER:
        rs = [r for r in by[key] if r["seeks"]]
        if not rs:
            continue
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

    m = out["meta"]
    print(f"encoder clock vs old segment calibration: {m['encoder_start_vs_segment_calibration_ms']:+.0f} ms; "
          f"fragments available after encode: p1 {m['fragment_available_after_encode_ms']['p1']:.0f} "
          f"median {m['fragment_available_after_encode_ms']['median']:.0f} ms")
    print(f"{'config':11} {'runs':>4} {'fail':>4} {'TTFF':>6} {'latency':>8} {'deliv':>6} {'p95':>6} {'seek':>5} "
          f"{'freeze':>6} {'stall':>5} {'req/min':>7} {'media s':>7}")
    for c in out["configs"]:
        f = lambda s, k="median": "—" if not s else f"{s[k]:.0f}"
        print(f"{c['key']:11} {c['runs']:>4} {c['failures']:>4} {f(c['ttff']):>6} {f(c['latency']):>8} "
              f"{f(c['delivery']):>6} {f(c['delivery'], 'p95'):>6} {f(c['seek']):>5} "
              f"{'—' if not c['freezes_per_run'] else format(c['freezes_per_run']['mean'], '.1f'):>6} "
              f"{'—' if not c['stalls_per_run'] else format(c['stalls_per_run']['mean'], '.1f'):>5} "
              f"{f(c['requests_per_min']):>7} {f(c['media_s_per_session']):>7}")
        if "webrtc" in c:
            w = c["webrtc"]
            print(f"   webrtc: connect {f(w['connect_ms'])}ms, jitter buffer {w['jitter_buffer_ms']['median']:.0f}ms, "
                  f"lost {f(w['packets_lost'], 'max')} pkts max, freezes (Chrome) {w['freeze_count']['mean']:.1f}, PLIs {f(w['pli_count'])}")
    for s in out.get("load", []):
        print(f"load n={s['n']:<4} p50={s['p50']:.0f} p95={s['p95']:.0f} p99={s['p99']:.0f}ms "
              f"delivered {s['delivered']}/{s['expected']} mismatches={s['hash_mismatches']} cpu={s['server_cpu_pct']:.1f}% {s['aggregate_mbps']:.0f}Mbps")
    for u in out.get("udp", []):
        print(f"udp loss {u['loss_pct']}%: frames lost {u['frame_loss_pct']:.1f}%  cc errors {u['continuity_errors']}  decode errors {u['decode_errors']}")


if __name__ == "__main__":
    main()
