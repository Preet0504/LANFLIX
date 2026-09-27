"""Build the single-page HTML edition of the textbook from the chapter
Markdown files: python docs/textbook/build.py  →  docs/textbook/textbook.html

Diagrams referenced as diagrams/<name>.png are replaced by their Mermaid
source (diagrams/<name>.mmd), which the page renders natively. Links
between chapter files become links between sections of the one page.
"""

import base64
import html
import re
from pathlib import Path

import markdown

HERE = Path(__file__).parent
CHAPTERS = sorted(p for p in HERE.glob("[0-9][0-9]-*.md"))
MERMAID = {p.stem: p.read_text(encoding="utf-8") for p in (HERE / "diagrams").glob("*.mmd")}
MERMAID["architecture"] = (HERE.parent / "architecture.mmd").read_text(encoding="utf-8")


def slug(path):
    return "ch-" + path.stem


def convert(md_text):
    # Diagram images → Mermaid blocks (before Markdown, so they're left alone).
    def diagram(m):
        name = Path(m.group(2)).stem
        src = MERMAID.get(name)
        if src is None:
            return m.group(0)
        return f'\n<figure class="diagram"><pre class="mermaid">{html.escape(src)}</pre><figcaption>{html.escape(m.group(1))}</figcaption></figure>\n'
    md_text = re.sub(r"!\[([^\]]*)\]\(((?:\.\./)?(?:diagrams/)?[\w-]+\.png)\)", diagram, md_text)
    # Links between chapters → in-page anchors.
    md_text = re.sub(r"\]\((\d\d-[\w-]+)\.md\)", lambda m: f"](#ch-{m.group(1)})", md_text)
    body = markdown.markdown(md_text, extensions=["tables", "fenced_code", "sane_lists"])
    # Principle call-outs are blockquotes that start with "Principle".
    body = body.replace("<blockquote>\n<p><strong>Principle", '<blockquote class="principle">\n<p><strong>Principle')
    # Remaining images (charts) are embedded, so the page is self-contained.
    def embed(m):
        f = (HERE / m.group(1)).resolve()
        if not f.exists():
            return m.group(0)
        return 'src="data:image/png;base64,' + base64.b64encode(f.read_bytes()).decode() + '"'
    body = re.sub(r'src="([^"]+\.png)"', embed, body)
    # Wide tables and code scroll inside their own box.
    body = re.sub(r"<table>", '<div class="tw"><table>', body).replace("</table>", "</table></div>")
    return body


sections, toc = [], []
for path in CHAPTERS:
    text = path.read_text(encoding="utf-8")
    title = text.splitlines()[0].lstrip("# ").strip()
    body = convert(text)
    # "Check your understanding" becomes an exercise block.
    body = re.sub(r"<h2>Check your understanding</h2>\s*<ol>(.*?)</ol>",
                  r'<aside class="exercises"><h3>Check your understanding</h3><ol>\1</ol></aside>', body, flags=re.S)
    num, _, name = title.partition(" — ")
    toc.append(f'<li><a href="#{slug(path)}"><span class="n">{html.escape(num.replace("Chapter ", ""))}</span>{html.escape(name or title)}</a></li>')
    body = re.sub(r"^<h1>.*?</h1>", f'<header class="ch-head"><div class="eyebrow">{html.escape(num)}</div><h1>{html.escape(name or title)}</h1></header>', body, count=1, flags=re.S)
    sections.append(f'<section class="chapter" id="{slug(path)}">{body}</section>')

intro = convert((HERE / "README.md").read_text(encoding="utf-8"))
intro = re.sub(r"^<h1>.*?</h1>", "", intro, count=1, flags=re.S)

page = (HERE / "template.html").read_text(encoding="utf-8")
page = page.replace("<!--TOC-->", "\n".join(toc)).replace("<!--INTRO-->", intro).replace("<!--CHAPTERS-->", "\n".join(sections))
out = HERE / "textbook.html"
out.write_text(page, encoding="utf-8")
print(f"wrote {out} ({out.stat().st_size // 1024} KB, {len(CHAPTERS)} chapters)")
