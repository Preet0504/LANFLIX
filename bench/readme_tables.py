"""Print the README's benchmark tables as Markdown, straight from
bench/results/summary.json, so no number in the README is typed by hand."""

import json
from pathlib import Path

S = json.loads((Path(__file__).parent / "results" / "summary.json").read_text())
C = {c["key"]: c for c in S["configs"]}
ORDER = ["ours", "ours-ll", "hls", "hls-tuned", "ll-hls", "dash", "dash-tuned", "webrtc"]
NAME = {"ours": "**LANFLIX**", "ours-ll": "**LANFLIX-LL**", "hls": "HLS", "hls-tuned": "HLS tuned", "ll-hls": "LL-HLS",
        "dash": "DASH", "dash-tuned": "DASH tuned", "webrtc": "WebRTC"}


def ms(v):
    if v is None:
        return "—"
    return f"{v / 1000:.2f} s" if v >= 1000 else f"{v:.0f} ms"


def get(k, field, stat="median"):
    s = C[k].get(field)
    return None if not s else s[stat]


rows = [
    ("Time to first frame", lambda k: ms(get(k, "ttff"))),
    ("Glass-to-glass delay", lambda k: ms(get(k, "latency"))),
    ("New-media delivery, median / p95", lambda k: "— (per packet)" if k == "webrtc" else f"{ms(get(k, 'delivery'))} / {ms(get(k, 'delivery', 'p95'))}"),
    ("Seek into history", lambda k: "not possible" if k == "webrtc" else ms(get(k, "seek"))),
    ("Freezes per 15 s (>250 ms)", lambda k: f"{get(k, 'freezes_per_run', 'mean'):.1f}"),
    ("HTTP requests / min", lambda k: "1 per session" if k in ("ours", "ours-ll", "webrtc") else f"{get(k, 'requests_per_min'):.0f}"),
    ("Media downloaded / session", lambda k: "—" if k == "webrtc" else f"{get(k, 'media_s_per_session'):.0f} s"),
]
print("| Metric | " + " | ".join(NAME[k] for k in ORDER) + " |")
print("|---|" + "---|" * len(ORDER))
for label, f in rows:
    print(f"| {label} | " + " | ".join(f(k) for k in ORDER) + " |")

w = C["webrtc"].get("webrtc", {})
if w:
    print(f"\nWebRTC: connect {ms(w['connect_ms']['median'])}, jitter buffer {ms(w['jitter_buffer_ms']['median'])}, "
          f"packets lost {w['packets_lost']['max']:.0f}, PLIs/session {w['pli_count']['median']:.0f}")
m = S["meta"]
print(f"\nmeta: {json.dumps(m)}")
print("seek by position:", {k: [round(x) for x in v if x is not None] for k, v in S["seek_by_position"].items()})
for k in ORDER:
    v = C[k]["latency_run_medians_ms"]
    print(f"{k:10} latency per trial: {[round(x / 1000, 2) for x in sorted(v)]}")
