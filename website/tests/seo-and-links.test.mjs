#!/usr/bin/env node
/**
 * Static audit of the built site. No browser, no test framework: it reads dist/ and asserts
 * what a crawler and a strict reviewer would check.
 *
 *   node tests/seo-and-links.test.mjs
 *
 * Exits 1 on the first failure and prints every failure it finds.
 */
import fs from 'node:fs';
import path from 'node:path';

const DIST = path.resolve('dist');
const BASE = '/TeleMeetBot-TeleMeetBot-implementation';

const failures = [];
const check = (condition, message) => {
  if (!condition) failures.push(message);
};

const walk = (dir, out = []) => {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(full, out);
    else out.push(full);
  }
  return out;
};

const files = walk(DIST);
const htmlFiles = files.filter((file) => file.endsWith('.html'));
const rel = (file) => '/' + path.relative(DIST, file).split(path.sep).join('/');
const toUrlPath = (file) => {
  const r = rel(file);
  if (r.endsWith('/index.html')) return r.slice(0, -'index.html'.length);
  if (r === '/index.html') return '/';
  return r;
};

const ids = new Map();
const html = new Map();
for (const file of htmlFiles) {
  const text = fs.readFileSync(file, 'utf8');
  html.set(toUrlPath(file), text);
  ids.set(toUrlPath(file), new Set([...text.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1])));
}

const repo = JSON.parse(fs.readFileSync(path.resolve('src/data/repo.generated.json'), 'utf8'));
const resolveInternal = (href, from) => {
  let clean = href.split('#')[0].split('?')[0];
  if (!clean) return from;
  if (!clean.startsWith('/')) return null;
  if (!clean.startsWith(BASE)) return `missing base path: ${href}`;
  clean = clean.slice(BASE.length) || '/';
  if (html.has(clean)) return clean;
  if (html.has(`${clean}/`)) return `${clean}/`;
  const disk = path.join(DIST, clean);
  if (fs.existsSync(disk) && fs.statSync(disk).isFile()) return clean;
  return `no page: ${href}`;
};

check(htmlFiles.length >= 15, `expected at least 15 pages, found ${htmlFiles.length}`);
for (const [route, text] of html) {
  const name = route;
  const title = /<title>([^<]+)<\/title>/.exec(text)?.[1] ?? '';
  const description = /<meta name="description" content="([^"]*)"/.exec(text)?.[1] ?? '';
  const canonical = /<link rel="canonical" href="([^"]+)"/.exec(text)?.[1] ?? '';
  const ogImage = /<meta property="og:image" content="([^"]+)"/.exec(text)?.[1] ?? '';
  const h1s = [...text.matchAll(/<h1[\s>]/g)].length;

  check(title.length >= 20 && title.length <= 75, `${name}: title length ${title.length} (${title})`);
  check(title.includes('TeleMeetBot') || name === '/404.html', `${name}: title lacks product name`);
  check(description.length >= 60 && description.length <= 200, `${name}: description length ${description.length}`);
  check(canonical.startsWith('https://opselon.github.io'), `${name}: canonical is not absolute (${canonical})`);
  check(canonical.includes(BASE), `${name}: canonical lacks base path (${canonical})`);
  check(/<html lang="en"/.test(text), `${name}: missing lang="en"`);
  check(/<meta name="viewport" content="width=device-width/.test(text), `${name}: missing responsive viewport`);
  check(/<meta charset="utf-8"/i.test(text), `${name}: missing charset`);
  check(/property="og:title"/.test(text) && /name="twitter:card"/.test(text), `${name}: missing social tags`);
  check(ogImage.startsWith('https://opselon.github.io') && ogImage.endsWith('.png'), `${name}: bad og:image (${ogImage})`);
  check(h1s === 1, `${name}: expected exactly one h1, found ${h1s}`);
  check(!/href="\/(?!TeleMeetBot-TeleMeetBot-implementation)/.test(text), `${name}: root-relative link escapes the base path`);
  check(!/src="\/(?!TeleMeetBot-TeleMeetBot-implementation)/.test(text), `${name}: root-relative asset escapes the base path`);
  const prose = text.replace(/<pre[\s\S]*?<\/pre>/g, '').replace(/<code[\s\S]*?<\/code>/g, '');
  check(!prose.includes(':::') && !prose.includes('**'), `${name}: unrendered markdown or Docusaurus syntax`);
  check(!/`[a-z-]+`/.test(prose), `${name}: unrendered inline code`);
  check(!/undefined|null<\/p>/.test(text), `${name}: leaked undefined/null value`);
  check(!/\[object Object\]/.test(text), `${name}: leaked object`);

  // Every image needs alt text and a real file.
  for (const match of text.matchAll(/<img\b([^>]*)>/g)) {
    const attrs = match[1];
    check(/\salt="[^"]*"/.test(attrs), `${name}: <img> without alt (${attrs.slice(0, 60)})`);
    const src = /src="([^"]+)"/.exec(attrs)?.[1];
    if (src?.startsWith('/')) check(fs.existsSync(path.join(DIST, src.slice(BASE.length))), `${name}: missing image ${src}`);
  }

  // Internal links and fragments must resolve.
  for (const match of text.matchAll(/(?:href|src)="([^"]+)"/g)) {
    const href = match[1];
    if (/^(https?:|mailto:|tel:|data:|#)/.test(href)) continue;
    const target = resolveInternal(href, route);
    check(typeof target === 'string' && !target.startsWith('no page') && !target.startsWith('missing base'), `${name}: ${target}`);
    const fragment = href.split('#')[1];
    if (fragment && typeof target === 'string' && ids.has(target)) {
      check(ids.get(target).has(fragment) || ids.get(target).has(decodeURIComponent(fragment)), `${name}: broken anchor ${href}`);
    }
  }
}

// Landing page must not hardcode facts that drift.
const home = html.get('/');
check(home?.includes(repo.version), `home: version ${repo.version} not rendered`);
check(repo.goVersion && home?.includes(repo.goVersion), `home: Go version ${repo.goVersion} not rendered`);
check(!/Go 1\.\d+\.\d+/.test(home ?? '') || (home ?? '').includes(repo.goVersion), 'home: hardcoded Go version');
check(!home?.includes('@SignalX_TradingBot'), 'home: real bot handle leaked');

// Sitemap, robots and search index.
const sitemap = fs.readFileSync(path.join(DIST, 'sitemap.xml'), 'utf8');
const locs = [...sitemap.matchAll(/<loc>([^<]+)<\/loc>/g)].map((m) => m[1]);
check(locs.every((loc) => loc.startsWith(`https://opselon.github.io${BASE}`)), 'sitemap: loc outside the base path');
check(new Set(locs).size === locs.length, 'sitemap: duplicate loc');
for (const route of html.keys()) {
  if (route === '/404.html') continue;
  check(locs.some((loc) => loc.endsWith(route)), `sitemap: missing ${route}`);
}

const robots = fs.readFileSync(path.join(DIST, 'robots.txt'), 'utf8');
check(robots.includes(`Sitemap: https://opselon.github.io${BASE}/sitemap.xml`), 'robots: wrong sitemap URL');

const search = JSON.parse(fs.readFileSync(path.join(DIST, 'search.json'), 'utf8'));
check(search.pages.length > 40, `search: only ${search.pages.length} entries`);
const stripBase = (value) => {
  const pathname = value.replace(/^https?:\/\/[^/]+/, '').split('#')[0];
  return pathname.startsWith(BASE) ? pathname.slice(BASE.length) || '/' : pathname;
};
const withSlash = (value) => (value.endsWith('/') ? value : `${value}/`);
const searchRoutes = new Set(search.pages.map((page) => stripBase(page.url)));
for (const route of searchRoutes) {
  check(html.has(withSlash(route)), `search: unknown route ${route}`);
}
for (const page of search.pages) {
  const fragment = page.url.split('#')[1];
  if (!fragment) continue;
  const route = stripBase(page.url);
  check(ids.get(withSlash(route))?.has(fragment), `search: broken anchor ${page.url}`);
  check(page.heading.length > 0, `search: entry without heading ${page.url}`);
}

// Docs content guards.
const docs = [...html.entries()].filter(([route]) => route.startsWith('/docs/') && route !== '/docs/');
check(docs.length >= 10, `expected at least 10 docs pages, found ${docs.length}`);
for (const [route, text] of docs) {
  const codeBlocks = [...text.matchAll(/<pre\b[^>]*data-language="([^"]*)"/g)].map((m) => m[1]);
  check(codeBlocks.length > 0, `${route}: no code samples`);
  check(codeBlocks.every((language) => language && language !== 'plaintext'), `${route}: unlabelled code block`);
  check(codeBlocks.every((language) => /^(bash|yaml|json|go|text|sh|console|dockerfile|ini|nginx|sql|http|diff|powershell|toml|md|html|javascript|typescript)$/.test(language)), `${route}: unexpected code language ${codeBlocks.join(',')}`);
}

if (failures.length) {
  console.error(`✗ ${failures.length} failure(s)\n` + failures.map((line) => `  - ${line}`).join('\n'));
  process.exit(1);
}
console.log(`✓ ${html.size} pages, ${locs.length} sitemap URLs, ${search.pages.length} search entries, ${docs.length} docs pages — all checks passed`);
