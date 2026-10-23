/**
 * Render the social preview image (1200×630) and the touch icon with Playwright.
 * Run once when branding changes: node scripts/make-og.mjs
 * The outputs are committed in public/ so builds do not need a browser.
 */
import { chromium } from 'playwright';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const PUBLIC = path.resolve(HERE, '..', 'public');
const fontDir = path.resolve(HERE, '..', 'src', 'assets', 'fonts');

const hex = {
  bg: '#06090f',
  text: '#e9eff8',
  muted: '#a7b6c9',
  accent: '#3ddc97',
  blue: '#6cc4ff',
  border: '#1d2b3c',
  panel: '#0c141f',
};

const html = `<!doctype html>
<html><head><meta charset="utf-8" />
<style>
  @font-face { font-family: Inter; src: url('file://${fontDir}/inter-latin.woff2') format('woff2'); font-weight: 400 800; }
  @font-face { font-family: JB; src: url('file://${fontDir}/jetbrains-mono-latin.woff2') format('woff2'); font-weight: 400 700; }
  * { box-sizing: border-box; margin: 0; }
  body {
    width: 1200px; height: 630px; background: ${hex.bg}; color: ${hex.text};
    font-family: Inter, sans-serif; position: relative; overflow: hidden;
    padding: 64px 72px; display: flex; flex-direction: column; justify-content: space-between;
  }
  .glow { position: absolute; border-radius: 50%; filter: blur(20px); }
  .g1 { width: 720px; height: 520px; left: -160px; top: -240px; background: radial-gradient(circle, rgba(61,220,151,.28), transparent 65%); }
  .g2 { width: 680px; height: 520px; right: -180px; top: -160px; background: radial-gradient(circle, rgba(108,196,255,.22), transparent 65%); }
  .grid { position: absolute; inset: 0; background-image: linear-gradient(rgba(148,178,214,.07) 1px, transparent 1px), linear-gradient(90deg, rgba(148,178,214,.07) 1px, transparent 1px); background-size: 52px 52px; mask-image: linear-gradient(to bottom, rgba(0,0,0,.8), transparent 80%); }
  .row { display: flex; align-items: center; gap: 16px; position: relative; }
  .mark { width: 58px; height: 58px; border-radius: 17px; background: linear-gradient(135deg, #3ddc97, #2b9fd8); display: grid; place-items: center; }
  .mark svg { width: 34px; height: 34px; }
  .brand { font-size: 30px; font-weight: 750; letter-spacing: -0.02em; }
  .brand small { display: block; font-family: JB, monospace; font-size: 14px; letter-spacing: .14em; color: ${hex.muted}; font-weight: 500; text-transform: uppercase; margin-top: 2px; }
  h1 { position: relative; font-size: 74px; line-height: 1.02; letter-spacing: -0.045em; font-weight: 800; max-width: 900px; }
  h1 em { font-style: normal; color: ${hex.accent}; }
  p { position: relative; font-size: 27px; color: ${hex.muted}; max-width: 880px; line-height: 1.45; }
  .chips { position: relative; display: flex; gap: 12px; }
  .chip { font-family: JB, monospace; font-size: 16px; letter-spacing: .05em; text-transform: uppercase; color: ${hex.muted}; border: 1px solid ${hex.border}; background: ${hex.panel}; padding: 7px 14px; border-radius: 999px; }
  .chip.a { color: ${hex.accent}; border-color: rgba(61,220,151,.45); background: rgba(61,220,151,.1); }
</style></head>
<body>
  <div class="glow g1"></div><div class="glow g2"></div><div class="grid"></div>
  <div class="row">
    <span class="mark">
      <svg viewBox="0 0 32 32" fill="none"><path d="M11 8.5 24 16l-13 7.5v-15Z" fill="#04150d"/></svg>
    </span>
    <span class="brand">TeleMeetBot<small>Headless Meet streaming</small></span>
  </div>
  <div>
    <h1>Send a link.<br />Your meeting <em>watches it</em>.</h1>
    <p>A 100% Go bot for Google Meet with a Telegram dashboard. Joins as a guest, presents a tab, plays anything.</p>
  </div>
  <div class="chips">
    <span class="chip a">Go · one binary</span>
    <span class="chip">No desktop</span>
    <span class="chip">Tab capture</span>
    <span class="chip">MIT</span>
  </div>
</body></html>`;

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 });
await page.setContent(html, { waitUntil: 'load' });
await page.evaluate(() => document.fonts.ready);
await page.waitForTimeout(400);
await page.screenshot({ path: path.join(PUBLIC, 'og.png') });

const iconHtml = `<!doctype html><html><body style="margin:0;width:180px;height:180px;background:transparent">
<svg width="180" height="180" viewBox="0 0 32 32" xmlns="http://www.w3.org/2000/svg">
  <defs><linearGradient id="g" x1="4" y1="2" x2="28" y2="30" gradientUnits="userSpaceOnUse">
    <stop stop-color="#3ddc97"/><stop offset="1" stop-color="#2b9fd8"/></linearGradient></defs>
  <rect width="32" height="32" fill="#06090f"/>
  <rect x="2" y="2" width="28" height="28" rx="8" fill="url(#g)"/>
  <path d="M12 10 22 16l-10 6V10Z" fill="#04150d"/>
</svg></body></html>`;
const iconPage = await browser.newPage({ viewport: { width: 180, height: 180 }, deviceScaleFactor: 1 });
await iconPage.setContent(iconHtml, { waitUntil: 'load' });
await iconPage.screenshot({ path: path.join(PUBLIC, 'apple-touch-icon.png'), omitBackground: true });
await browser.close();

for (const name of ['og.png', 'apple-touch-icon.png']) {
  console.log(`wrote public/${name} (${fs.statSync(path.join(PUBLIC, name)).size} bytes)`);
}
