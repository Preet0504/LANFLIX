"""Render the README's static figures from bench/results/summary.json.

Same data and conventions as the interactive report (bench/report). Color
encodes the protocol family (LANFLIX, HLS, DASH, WebRTC — the first three
slots of the validated palette plus violet); each variant (tuned, LL) is
named on its own row, so identity never rests on color alone. Where lines
overlap, variants are dashed. One axis per chart, direct labels, recessive
grid. Solid white background so the PNGs stay readable on GitHub's dark
theme too.
"""

import json
import statistics as st
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

ROOT = Path(__file__).parent
S = json.loads((ROOT / "results" / "summary.json").read_text())
OUT = ROOT.parent / "docs" / "figures"
OUT.mkdir(parents=True, exist_ok=True)

ORDER = ["ours", "ours-ll", "hls", "hls-tuned", "ll-hls", "dash", "dash-tuned", "webrtc"]
SHORT = {"ours": "LANFLIX", "ours-ll": "LANFLIX-LL", "hls": "HLS", "hls-tuned": "HLS tuned", "ll-hls": "LL-HLS",
         "dash": "DASH", "dash-tuned": "DASH tuned", "webrtc": "WebRTC"}
FAMILY = {"ours": "#2a78d6", "hls": "#eb6834", "dash": "#1baf7a", "webrtc": "#4a3aa7"}
COLOR = {k: FAMILY[k.split("-")[0] if not k.startswith("ll-") else "hls"] for k in ORDER}
VARIANT = {"ours-ll", "hls-tuned", "ll-hls", "dash-tuned"}
OURS = {"ours", "ours-ll"}
INK, INK2, INK3, GRID, BASE = "#16181d", "#4c515c", "#7d828c", "#e4e3dd", "#cfcdc4"
C = {c["key"]: c for c in S["configs"]}

plt.rcParams.update({
    "font.family": ["Segoe UI", "DejaVu Sans"], "font.size": 10.5,
    "axes.edgecolor": BASE, "axes.labelcolor": INK2, "xtick.color": INK3, "ytick.color": INK3,
    "axes.spines.top": False, "axes.spines.right": False, "axes.grid": True,
    "grid.color": GRID, "grid.linewidth": 0.8, "figure.facecolor": "white", "axes.facecolor": "white",
    "savefig.facecolor": "white", "savefig.dpi": 160,
})


def ms(v):
    return f"{v / 1000:.2f} s" if v >= 1000 else f"{v:.0f} ms"


def secs(v):
    return f"{v / 1000:.2f} s" if v < 1000 else f"{v / 1000:.1f} s"


def save(fig, name, title):
    fig.suptitle(title, x=0.01, ha="left", fontsize=13, fontweight="bold", color=INK)
    fig.tight_layout()
    fig.savefig(OUT / name, bbox_inches="tight")
    plt.close(fig)
    print("wrote", OUT / name)


def style_labels(ax, keys):
    for lab, k in zip(ax.get_yticklabels(), keys):
        if k in OURS:
            lab.set_fontweight("bold")
            lab.set_color(INK)


def strip(field, name, title, fmt=ms, xlabel="", log=False, ticks=None):
    keys = [k for k in ORDER if C.get(k) and C[k][field]]
    fig, ax = plt.subplots(figsize=(9, 0.45 * len(keys) + 1.2))
    for i, k in enumerate(keys):
        y = len(keys) - 1 - i
        vals = C[k][field]
        jitter = [((j * 7919) % 13 - 6) * 0.018 for j in range(len(vals))]
        ax.scatter(vals, [y + d for d in jitter], s=26, color=COLOR[k], alpha=0.8, edgecolor="white", linewidth=0.8, zorder=3)
        m = st.median(vals)
        ax.plot([m, m], [y - 0.28, y + 0.28], color=INK, linewidth=2, solid_capstyle="round", zorder=4)
        ax.annotate(fmt(m), (1.01, y), xycoords=("axes fraction", "data"), va="center",
                    fontsize=10, color=INK if k in OURS else INK2, fontweight="bold" if k in OURS else "normal")
    ax.set_yticks(range(len(keys)))
    ax.set_yticklabels([SHORT[k] for k in reversed(keys)])
    style_labels(ax, list(reversed(keys)))
    ax.grid(axis="y", visible=False)
    if log:
        ax.set_xscale("log")
        if ticks:
            ax.set_xticks(ticks)
        ax.xaxis.set_minor_locator(matplotlib.ticker.NullLocator())
    else:
        ax.set_xlim(left=0)
    ax.xaxis.set_major_formatter(matplotlib.ticker.FuncFormatter(lambda v, _: fmt(v)))
    ax.set_xlabel(xlabel)
    ax.tick_params(axis="y", length=0)
    save(fig, name, title)


def cdf():
    panels = [("2 s segments", ["ours", "hls", "hls-tuned", "dash", "dash-tuned"]),
              ("200 ms chunks", ["ours-ll", "ll-hls"])]
    fig, axes = plt.subplots(1, 2, figsize=(10, 3.8), sharey=True, gridspec_kw={"width_ratios": [1.4, 1]})
    for ax, (label, keys) in zip(axes, panels):
        for k in keys:
            if not C.get(k) or not C[k]["delivery_ms"]:
                continue
            v = sorted(max(x, 1) for x in C[k]["delivery_ms"])
            n = len(v)
            ax.step(v, [(i + 1) / n for i in range(n)], where="post", color=COLOR[k],
                    linewidth=2.4 if k in OURS else 1.8, linestyle="--" if k in VARIANT and k not in OURS else "-",
                    label=f"{SHORT[k]}  (median {ms(st.median(v))})")
        ax.set_xscale("log")
        ax.xaxis.set_major_formatter(matplotlib.ticker.FuncFormatter(lambda v, _: ms(v)))
        ax.yaxis.set_major_formatter(matplotlib.ticker.PercentFormatter(1.0))
        ax.set_ylim(0, 1.02)
        ax.set_title(label, loc="left", fontsize=10.5, color=INK2)
        if len(keys) <= 2:
            ax.legend(frameon=False, fontsize=9, loc="lower right")
    # Five curves leave no clear space inside the left panel: legend below it.
    h, l = axes[0].get_legend_handles_labels()
    fig.legend(h, l, frameon=False, fontsize=9, ncol=3, loc="upper left", bbox_to_anchor=(0.06, 0.0))
    axes[0].set_xlabel("unit available at the origin → bytes at the viewer (log scale)")
    save(fig, "delivery-cdf.png", "New media reaches the viewer: push and LL-HLS in milliseconds, classic polling in ~1 s")


def bars(values, name, title, fmt, keys=None):
    keys = [k for k in (keys or ORDER) if C.get(k) and values(C[k]) is not None]
    vals = [values(C[k]) for k in keys]
    fig, ax = plt.subplots(figsize=(9, 0.4 * len(keys) + 1.0))
    ys = list(range(len(keys)))[::-1]
    ax.barh(ys, vals, color=[COLOR[k] for k in keys], height=0.62, zorder=3)
    for y, v, k in zip(ys, vals, keys):
        ax.annotate(fmt(v, k), (v, y), xytext=(6, 0), textcoords="offset points", va="center",
                    color=INK if k in OURS else INK2, fontweight="bold" if k in OURS else "normal")
    ax.set_yticks(ys)
    ax.set_yticklabels([SHORT[k] for k in keys])
    style_labels(ax, keys)
    ax.grid(axis="y", visible=False)
    ax.tick_params(axis="y", length=0)
    ax.set_xlim(0, max(vals) * 1.18 or 1)
    save(fig, name, title)


def scaling():
    load, before = S["load"], S.get("load_before_hub", {}).get("steps", [])
    for field, name, title, fmt in [
        ("p50", "scaling-delay.png", "Median delivery delay vs concurrent viewers: before/after the fan-out hub", ms),
        ("server_cpu_pct", "scaling-cpu.png", "Server CPU (% of one core) vs concurrent viewers", lambda v: f"{v:.0f}%"),
    ]:
        fig, ax = plt.subplots(figsize=(9, 3.4))
        for color, data, label, strong in [("#eb6834", before, "Before: one Redis read per viewer", False),
                                           ("#2a78d6", load, "After: fan-out hub", True)]:
            if not data:
                continue
            xs, ys = [d["n"] for d in data], [d[field] for d in data]
            ax.plot(xs, ys, color=color, linewidth=2, marker="o", markersize=5, markeredgecolor="white", label=label, zorder=3)
            ax.annotate(f"{fmt(ys[-1])}", (xs[-1], ys[-1]), xytext=(8, 0), textcoords="offset points", va="center",
                        color=INK if strong else INK2, fontweight="bold" if strong else "normal")
        ax.set_xlabel("concurrent viewers")
        ax.yaxis.set_major_formatter(matplotlib.ticker.FuncFormatter(lambda v, _: fmt(v)))
        ax.set_ylim(bottom=0)
        ax.legend(frameon=False, loc="upper left")
        save(fig, name, title)


def udp():
    u = [r for r in S["udp"] if r]
    fig, ax = plt.subplots(figsize=(9, 3.2))
    xs, ys = [r["loss_pct"] for r in u], [r["decode_errors"] for r in u]
    # One series, and not a system in the comparison charts: neutral ink.
    ax.plot(xs, ys, color=INK2, linewidth=2, marker="o", markersize=5, markeredgecolor="white", zorder=3)
    for x, y in zip(xs, ys):
        if x in (1, 5):
            ax.annotate(f"{y} errors", (x, y), xytext=(8, -2), textcoords="offset points", color=INK2)
    ax.set_xlabel("datagrams lost (%)")
    ax.set_ylabel("decoder errors per minute")
    ax.set_ylim(bottom=0)
    save(fig, "udp-loss.png", "Raw UDP (MPEG-TS) under packet loss: decoder errors in a 60 s clip")


strip("ttff_ms", "ttff.png", "Time to first frame at the live edge (lower is better)")
strip("latency_run_medians_ms", "latency.png", "Glass-to-glass delay behind real time (lower is better, log scale)",
      fmt=secs, log=True, ticks=[30, 100, 300, 1000, 3000, 10000])
cdf()
strip("seek_ms", "seek.png", "Seek latency into stream history (lower is better; WebRTC cannot seek)")
# Connection-based systems make one request per session; per minute that
# only reflects how short a test session is (~25 s), so say what it is.
bars(lambda c: c["requests_per_min"]["median"], "requests.png", "HTTP requests per viewer per minute at the origin",
     lambda v, k: "1 per session" if k in ("ours", "ours-ll", "webrtc") else f"{v:.0f}/min")
bars(lambda c: c["media_s_per_session"]["median"] if c.get("media_s_per_session") else None, "segments.png",
     "Media downloaded per session with 4 seeks (seconds of video): LANFLIX's bandwidth weakness",
     lambda v, k: f"{v:.0f} s")
bars(lambda c: c["freezes_per_run"]["mean"], "freezes.png",
     "Playback freezes (>250 ms without a new frame) per 15 s of steady playback", lambda v, k: f"{v:.1f}")
scaling()
udp()
