"""Inject bench/results/summary.json into the report template."""
import json
from pathlib import Path

here = Path(__file__).parent
summary = json.loads((here.parent / "results" / "summary.json").read_text())
for c in summary["configs"]:
    c.pop("latency_samples_ms", None)  # per-frame samples: large, unused by the charts
html = (here / "template.html").read_text(encoding="utf-8")
out = here / "index.html"
out.write_text(html.replace("/*__DATA__*/null", json.dumps(summary, separators=(",", ":"))), encoding="utf-8")
print(f"wrote {out} ({out.stat().st_size / 1024:.0f} KB)")
