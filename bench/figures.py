"""Render the README's static figures from bench/results/summary.json.

Same data, palette and conventions as the interactive report
(bench/report): ours is always series 1 (blue), one axis per chart,
direct labels, recessive grid. Solid white background so the PNGs stay
readable on GitHub's dark theme too.
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

ORDER = ["ours", "hls", "hls-tuned", "dash", "dash-tuned"]
SHORT = {"ours": "LANFLIX", "hls": "HLS", "hls-tuned": "HLS tuned", "dash": "DASH", "dash-tuned": "DASH tuned"}
# Validated categorical palette (light mode), fixed order.
COLOR = {"ours": "#2a78d6", "hls": "#eb6834", "hls-tuned": "#1baf7a", "dash": "#eda100", "dash-tuned": "#e87ba4",
         "before": "#eb6834", "after": "#2a78d6", "udp": "#4a3aa7"}
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
    return f"{v / 1000:.1f} s"


def save(fig, name, title):
    fig.suptitle(title, x=0.01, ha="left", fontsize=13, fontweight="bold", color=INK)
    fig.tight_layout()
    fig.savefig(OUT / name, bbox_inches="tight")
    plt.close(fig)
    print("wrote", OUT / name)


def strip(field, name, title, fmt=ms, xlabel=""):
    fig, ax = plt.subplots(figsize=(9, 3.4))
    keys = [k for k in ORDER if C.get(k) and C[k][field]]
    for i, k in enumerate(keys):
        y = len(keys) - 1 - i
        vals = C[k][field]
        jitter = [((j * 7919) % 13 - 6) * 0.018 for j in range(len(vals))]
        ax.scatter(vals, [y + d for d in jitter], s=26, color=COLOR[k], alpha=0.8, edgecolor="white", linewidth=0.8, zorder=3)
        m = st.median(vals)
        ax.plot([m, m], [y - 0.28, y + 0.28], color=INK, linewidth=2, solid_capstyle="round", zorder=4)
        ax.annotate(fmt(m), (1.01, y), xycoords=("axes fraction", "data"), va="center",
                    fontsize=10, color=INK if k == "ours" else INK2, fontweight="bold" if k == "ours" else "normal")
    ax.set_yticks(range(len(keys)))
    ax.set_yticklabels([SHORT[k] for k in reversed(keys)])
    for lab in ax.get_yticklabels():
        if lab.get_text() == "LANFLIX":
            lab.set_fontweight("bold")
            lab.set_color(INK)
    ax.grid(axis="y", visible=False)
    ax.set_xlim(left=0)
    ax.xaxis.set_major_formatter(matplotlib.ticker.FuncFormatter(lambda v, _: fmt(v)))
    ax.set_xlabel(xlabel)
    ax.tick_params(axis="y", length=0)
    save(fig, name, title)


def cdf():
    fig, ax = plt.subplots(figsize=(9, 3.8))
    for k in ORDER:
        v = sorted(max(x, 1) for x in C[k]["delivery_ms"])
        n = len(v)
        ax.step(v, [(i + 1) / n for i in range(n)], where="post", color=COLOR[k], linewidth=2.4 if k == "ours" else 1.8,
                label=f"{SHORT[k]}  (median {ms(st.median(v))})")
    ax.set_xscale("log")
    ax.xaxis.set_major_formatter(matplotlib.ticker.FuncFormatter(lambda v, _: ms(v)))
    ax.yaxis.set_major_formatter(matplotlib.ticker.PercentFormatter(1.0))
    ax.set_ylim(0, 1.02)
    ax.set_xlabel("delay from segment available at the origin → bytes at the viewer (log scale)")
    ax.legend(frameon=False, loc="upper left", fontsize=9.5)
    save(fig, "delivery-cdf.png", "New segments reach LANFLIX viewers in milliseconds, polling players in ~1 s")


def bars(values, name, title, fmt):
    fig, ax = plt.subplots(figsize=(9, 2.9))
    keys = [k for k in ORDER if C.get(k)]
    vals = [values(C[k]) for k in keys]
    ys = list(range(len(keys)))[::-1]
    ax.barh(ys, vals, color=[COLOR[k] for k in keys], height=0.62, zorder=3)
    for y, v, k in zip(ys, vals, keys):
        ax.annotate(fmt(v), (v, y), xytext=(6, 0), textcoords="offset points", va="center",
                    color=INK if k == "ours" else INK2, fontweight="bold" if k == "ours" else "normal")
    ax.set_yticks(ys)
    ax.set_yticklabels([SHORT[k] for k in keys])
    ax.grid(axis="y", visible=False)
    ax.tick_params(axis="y", length=0)
    ax.set_xlim(0, max(vals) * 1.15)
    save(fig, name, title)


def scaling():
    load, before = S["load"], S.get("load_before_hub", {}).get("steps", [])
    for field, name, title, fmt in [
        ("p50", "scaling-delay.png", "Median delivery delay vs concurrent viewers: before/after the fan-out hub", ms),
        ("server_cpu_pct", "scaling-cpu.png", "Server CPU (% of one core) vs concurrent viewers", lambda v: f"{v:.0f}%"),
    ]:
        fig, ax = plt.subplots(figsize=(9, 3.4))
        for key, data, label in [("before", before, "Before: one Redis read per viewer"), ("after", load, "After: fan-out hub")]:
            if not data:
                continue
            xs, ys = [d["n"] for d in data], [d[field] for d in data]
            ax.plot(xs, ys, color=COLOR[key], linewidth=2, marker="o", markersize=5, markeredgecolor="white", label=label, zorder=3)
            ax.annotate(f"{fmt(ys[-1])}", (xs[-1], ys[-1]), xytext=(8, 0), textcoords="offset points", va="center",
                        color=INK if key == "after" else INK2, fontweight="bold" if key == "after" else "normal")
        ax.set_xlabel("concurrent viewers")
        ax.yaxis.set_major_formatter(matplotlib.ticker.FuncFormatter(lambda v, _: fmt(v)))
        ax.set_ylim(bottom=0)
        ax.legend(frameon=False, loc="upper left")
        save(fig, name, title)


def udp():
    u = [r for r in S["udp"] if r]
    fig, ax = plt.subplots(figsize=(9, 3.2))
    xs, ys = [r["loss_pct"] for r in u], [r["decode_errors"] for r in u]
    ax.plot(xs, ys, color=COLOR["udp"], linewidth=2, marker="o", markersize=5, markeredgecolor="white", zorder=3)
    for x, y in zip(xs, ys):
        if x in (1, 5):
            ax.annotate(f"{y} errors", (x, y), xytext=(8, -2), textcoords="offset points", color=INK2)
    ax.set_xlabel("datagrams lost (%)")
    ax.set_ylabel("decoder errors per minute")
    ax.set_ylim(bottom=0)
    save(fig, "udp-loss.png", "Raw UDP (MPEG-TS) under packet loss: decoder errors in a 60 s clip")


strip("ttff_ms", "ttff.png", "Time to first frame at the live edge (lower is better)")
strip("latency_run_medians_ms", "latency.png", "Glass-to-glass delay behind real time (lower is better)", fmt=secs)
cdf()
strip("seek_ms", "seek.png", "Seek latency: the one latency measure LANFLIX loses (lower is better)")
bars(lambda c: c["requests_per_min"]["median"], "requests.png", "HTTP requests per viewer per minute at the origin",
     lambda v: f"{v:.1f}/min" if v < 10 else f"{v:.0f}/min")
bars(lambda c: c["segments_fetched_per_session"]["median"], "segments.png",
     "Segments downloaded per session with 4 seeks: LANFLIX's bandwidth weakness", lambda v: f"{v:.0f}")
scaling()
udp()
