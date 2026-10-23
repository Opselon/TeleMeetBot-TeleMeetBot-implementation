#!/usr/bin/env node
/**
 * WCAG contrast audit for the design tokens in src/styles/global.css.
 * Reads the two theme blocks, computes relative luminance ratios and fails
 * below the thresholds it prints. Run: node tests/contrast-report.mjs
 */
import fs from 'node:fs';

const css = fs.readFileSync('src/styles/global.css', 'utf8');

const block = (selector) => {
  const start = css.indexOf(selector);
  if (start === -1) throw new Error(`missing ${selector} block`);
  const open = css.indexOf('{', start);
  const close = css.indexOf('\n}', open);
  const body = css.slice(open + 1, close);
  const vars = {};
  for (const match of body.matchAll(/--([a-z0-9-]+):\s*([^;]+);/g)) vars[match[1]] = match[2].trim();
  return vars;
};

const themes = {
  dark: block(':root {'),
  light: block("html[data-theme='light']"),
};

const rgb = (value) => {
  const hex = value.replace('#', '');
  if (!/^[0-9a-f]{6}$/i.test(hex)) throw new Error(`not a hex colour: ${value}`);
  return [0, 2, 4].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255);
};
const channel = (c) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
const luminance = (hex) => {
  const [r, g, b] = rgb(hex).map(channel);
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};
const ratio = (a, b) => {
  const [l1, l2] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (l1 + 0.05) / (l2 + 0.05);
};

const pairs = [
  ['text', 'bg', 4.5, 'body copy'],
  ['text', 'panel', 4.5, 'body copy on cards'],
  ['text', 'panel-2', 4.5, 'body copy on nested panels'],
  ['muted', 'bg', 4.5, 'secondary copy'],
  ['muted', 'panel', 4.5, 'secondary copy on cards'],
  ['faint', 'panel', 4.5, 'metadata on cards'],
  ['faint', 'bg', 4.5, 'metadata'],
  ['accent', 'bg', 4.5, 'links and accents'],
  ['accent', 'panel', 4.5, 'accents on cards'],
  ['accent-2', 'bg', 4.5, 'second accent'],
  ['accent-2', 'panel', 4.5, 'second accent on cards'],
  ['accent-ink', 'accent', 4.5, 'primary button label'],
  ['warn', 'panel', 4.5, 'warning text'],
  ['danger', 'panel', 4.5, 'danger text'],
  ['border', 'bg', 1.2, 'visible separators'],
];

let failures = 0;
for (const [name, theme] of Object.entries(themes)) {
  console.log(`\n${name} theme`);
  for (const [fg, bg, min, label] of pairs) {
    if (!theme[fg] || !theme[bg]) {
      console.log(`  ! missing token ${!theme[fg] ? fg : bg}`);
      failures++;
      continue;
    }
    const value = ratio(theme[fg], theme[bg]);
    const pass = value >= min;
    if (!pass) failures++;
    console.log(
      `  ${pass ? '✓' : '✗'} ${`${fg} on ${bg}`.padEnd(24)} ${value.toFixed(2).padStart(5)} : 1  (min ${min})  ${label}`,
    );
  }
}

if (failures) {
  console.error(`\n${failures} contrast failure(s)`);
  process.exit(1);
}
console.log('\nAll token pairs meet their WCAG AA threshold.');
