// Renders every diagrams/*.mmd to a PNG next to it, for readers of the
// Markdown on GitHub (the HTML edition renders the Mermaid source itself).
// Needs playwright-core and a Chromium-based browser:
//   NODE_PATH=<dir with node_modules> node docs/textbook/render_diagrams.js
const { chromium } = require('playwright-core');
const fs = require('fs');
const path = require('path');

const DIR = path.join(__dirname, 'diagrams');
const BROWSER = process.env.BROWSER_PATH || 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';

(async () => {
  const b = await chromium.launch({ executablePath: BROWSER, headless: true });
  const p = await b.newPage({ viewport: { width: 1600, height: 900 }, deviceScaleFactor: 2 });
  await p.setContent('<html><body style="margin:0;background:#fff"><div id="out" style="display:inline-block;padding:20px;background:#fff"></div></body></html>');
  await p.addScriptTag({ url: 'https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js' });
  for (const f of fs.readdirSync(DIR).filter((f) => f.endsWith('.mmd'))) {
    const src = fs.readFileSync(path.join(DIR, f), 'utf8');
    const res = await p.evaluate(async (src) => {
      mermaid.initialize({ startOnLoad: false, theme: 'default' });
      try {
        const { svg } = await mermaid.render('d' + Math.random().toString(36).slice(2), src);
        const out = document.getElementById('out');
        out.innerHTML = svg;
        const el = out.querySelector('svg');
        const vb = el.viewBox.baseVal;
        el.style.maxWidth = 'none';
        el.setAttribute('width', vb.width);
        el.setAttribute('height', vb.height);
        return 'ok';
      } catch (e) { return 'ERR ' + (e.message || e); }
    }, src);
    if (res !== 'ok') { console.error(f, res); process.exitCode = 1; continue; }
    await p.locator('#out').screenshot({ path: path.join(DIR, f.replace(/\.mmd$/, '.png')) });
    console.log('rendered', f);
  }
  await b.close();
})();
