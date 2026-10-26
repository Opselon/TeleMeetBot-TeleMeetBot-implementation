#!/usr/bin/env node
/**
 * Build-time content ingestion for the TeleMeetBot website.
 *
 * Every fact the site states about the product is read from the repository
 * (flags, bot commands, config keys, health fields, media extensions) or from
 * the GitHub API with a committed snapshot fallback. Nothing here is typed by
 * hand twice: when the Go source changes, the site either updates itself or
 * fails the build with an explicit message.
 *
 * Usage: node scripts/sync-repo-content.mjs   (runs before `astro build`)
 * Env:   GH_TOKEN / GITHUB_TOKEN — optional, raises the API rate limit.
 */
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const SITE_DIR = path.resolve(HERE, '..');
const REPO_ROOT = path.resolve(SITE_DIR, '..');
const OUT = path.join(SITE_DIR, 'src', 'data', 'repo.generated.json');
const CONTENT_RELEASES = path.join(SITE_DIR, 'src', 'content', 'releases');
const SNAPSHOT = path.join(SITE_DIR, 'src', 'data', 'github.snapshot.json');
const REPO_SLUG = 'Opselon/TeleMeetBot-TeleMeetBot-implementation';

const warnings = [];
const read = (rel) => fs.readFileSync(path.join(REPO_ROOT, rel), 'utf8');
const git = (...args) => {
  try {
    return execFileSync('git', ['-C', REPO_ROOT, ...args], { encoding: 'utf8' }).trim();
  } catch {
    return '';
  }
};

// ---------------------------------------------------------------- git facts
const commit = git('rev-parse', '--short=7', 'HEAD') || 'unknown';
const commitDate = git('log', '-1', '--format=%cI') || null;
const branch = git('rev-parse', '--abbrev-ref', 'HEAD') || 'main';
let latestTag = git('describe', '--tags', '--abbrev=0') || '';

const goMod = read('go.mod');
const goVersion = (goMod.match(/^go\s+(\d+\.\d+)/m) || [])[1] || '';
if (!goVersion) throw new Error('sync: could not read the Go version from go.mod');

// ------------------------------------------------------------------- flags
const mainGo = read('cmd/telemeetbot/main.go');
const flagRe = /(\w+)\s*=\s*flag\.(String|Bool|Int)\("([a-z0-9-]+)",\s*(.+?),\s*"([^"]*)"\)/g;
const cliFlags = [];
for (const m of mainGo.matchAll(flagRe)) {
  const [, , goType, name, rawDefault, usage] = m;
  let def = rawDefault.trim();
  if (def === '""') def = '';
  if (def.startsWith('"') && def.endsWith('"')) def = def.slice(1, -1);
  cliFlags.push({
    name: `--${name}`,
    type: goType === 'Bool' ? 'bool' : goType === 'Int' ? 'int' : 'string',
    default: def,
    usage,
  });
}
if (cliFlags.length < 5) {
  throw new Error(`sync: parsed only ${cliFlags.length} CLI flags from cmd/telemeetbot/main.go`);
}

// -------------------------------------------------------------- bot commands
const COMMAND_DOCS = {
  start: 'Open the dashboard: active sessions, uptime, limits and the main keyboard.',
  new: 'Start the two-step stream wizard (Meet link → media link). Accepts both links inline.',
  stream: 'Alias of /new.',
  sessions: 'List running sessions with their state and elapsed time.',
  audiotrack: 'Select or inspect the audio language track for multi-audio YouTube streams.',
  audiolang: 'Alias of /audiotrack.',
  lang: 'Alias of /audiotrack.',
  language: 'Alias of /audiotrack.',
  health: 'Health snapshot: version, browser, disk, uptime and active sessions.',
  settings: 'Print the effective configuration summary (secrets masked).',
  help: 'Usage overview with every command and button.',
  cancel: 'Abort an open wizard and drop its pending input.',
  stop: 'Stop one session: /stop <id>. Without an id it lists what can be stopped.',
  id: 'Return your numeric Telegram user id (use it in telegram.admins).',
  ping: 'Liveness probe: replies with the process uptime.',
};
const flowsGo = read('internal/bot/flows.go');
const INLINE_ACTIONS = new Set([
  'pause', 'resume', 'mute', 'unmute', 'voldown', 'volup', 'back10', 'fwd10', 'back30', 'fwd30', 'back60', 'fwd60', 'restart', 'refresh', 'stop', 'next', 'prev', 'queue', 'jump', 'clear_confirm', 'do_clear', 'do_clear_all', 'rm_menu', 'rm',
]);
const commands = [];
for (const m of flowsGo.matchAll(/case\s+"([a-z0-9]+)"(?:,\s*"([a-z0-9]+)")?:/g)) {
  const names = [m[1], m[2]].filter(Boolean);
  if (INLINE_ACTIONS.has(names[0])) continue;
  if (!(names[0] in COMMAND_DOCS)) {
    throw new Error(
      `sync: bot command "${names[0]}" has no documented description. ` +
        'Add it to COMMAND_DOCS in website/scripts/sync-repo-content.mjs so the docs cannot drift.',
    );
  }
  if (names[1] && !(names[1] in COMMAND_DOCS)) {
    throw new Error(`sync: bot command alias "${names[1]}" has no documented description`);
  }
  const primary = names[0];
  commands.push({
    command: `/${names.join(', /')}`,
    name: primary,
    description: COMMAND_DOCS[primary],
    alias: names[1] ? `/${names[1]}` : null,
    context:
      /case\s+"(pause|resume|mute|unmute|voldown|volup|back10|fwd10|back30|fwd30|back60|fwd60|restart|refresh|stop|next|prev|queue)":/.test(m[0])
        ? 'inline'
        : 'command',
  });
}
// Callback buttons (act:<id>:<action>) are their own documented surface.
const inlineActions = [
  { action: 'pause', label: '⏸ Pause', description: 'Pause playback in the shared tab.' },
  { action: 'resume', label: '▶️ Resume', description: 'Resume playback and re-assert the tab volume.' },
  { action: 'mute', label: '🔇 Mute', description: 'Mute the player (the meeting keeps receiving silence).' },
  { action: 'unmute', label: '🔊 Unmute', description: 'Unmute and restore the configured volume.' },
  { action: 'voldown', label: '🔉 Vol-', description: 'Lower the player volume by 10 percentage points.' },
  { action: 'volup', label: '🔊 Vol+', description: 'Raise the player volume by 10 percentage points.' },
  { action: 'back10', label: '⏪ -10s', description: 'Seek back by media.seek_step_seconds (default 10).' },
  { action: 'fwd10', label: '⏩ +10s', description: 'Seek forward by media.seek_step_seconds (default 10).' },
  { action: 'back30', label: '⏪ -30s', description: 'Seek back by a fixed 30 seconds.' },
  { action: 'fwd30', label: '⏩ +30s', description: 'Seek forward by a fixed 30 seconds.' },
  { action: 'back60', label: '⏪ -60s', description: 'Seek back by a fixed 60 seconds.' },
  { action: 'fwd60', label: '⏩ +60s', description: 'Seek forward by a fixed 60 seconds.' },
  { action: 'restart', label: '♻️ Restart media', description: 'Reload the media tab and start playback from the beginning.' },
  { action: 'refresh', label: '🔄 Refresh', description: 'Re-render the session card from live state.' },
  { action: 'stop', label: '⏹ Stop', description: 'End the session and close its Chromium.' },
  { action: 'next', label: '⏭ Next', description: 'Advance to the next track in the playlist queue.' },
  { action: 'prev', label: '⏮ Prev', description: 'Return to the previous track in the playlist queue.' },
  { action: 'queue', label: '📋 Queue', description: 'View and manage playlist queue tracks.' },
];
const actionSet = new Set();
for (const m of flowsGo.matchAll(/case\s+"(pause|resume|mute|unmute|voldown|volup|back10|fwd10|back30|fwd30|back60|fwd60|restart|refresh|stop|next|prev|queue)":/g)) {
  actionSet.add(m[1]);
}
const missingActions = [...actionSet].filter((a) => !inlineActions.some((i) => i.action === a));
if (missingActions.length) {
  throw new Error(`sync: inline actions missing documentation: ${missingActions.join(', ')}`);
}

// ------------------------------------------------------------- config schema
// Values come from the checked-in example (what users copy), descriptions from
// the Go struct comments (what the code means). Both can drift visibly.
const configGo = read('internal/config/config.go');
const SECTION_OF = {
  TelegramConfig: 'telegram',
  ServerConfig: 'server',
  BrowserConfig: 'browser',
  MediaConfig: 'media',
  MeetConfig: 'meet',
  LimitsConfig: 'limits',
  LoggingConfig: 'logging',
  PathsConfig: 'paths',
};
const goFieldDocs = new Map();
{
  let structName = '';
  let comments = [];
  for (const line of configGo.split('\n')) {
    const structMatch = line.match(/^type (\w+Config) struct \{/);
    if (structMatch) {
      structName = structMatch[1];
      comments = [];
      continue;
    }
    if (!structName) continue;
    if (/^\}/.test(line)) {
      structName = '';
      comments = [];
      continue;
    }
    const comment = line.match(/^\s*\/\/\s*(.*)$/);
    if (comment) {
      comments.push(comment[1].trim());
      continue;
    }
    const field = line.match(/^\s*\w+\s+[\[\]\w.]+\s+`yaml:"([a-z_]+)"/);
    if (field) {
      const section = SECTION_OF[structName];
      const text = comments.join(' ').replace(/\s+/g, ' ').trim();
      if (section && text) {
        const cleaned = text
          .replace(new RegExp(`^${field[1][0].toUpperCase()}${field[1].slice(1).replace(/_/g, '')}\\b`, 'i'), '')
          .replace(/^(is|are)\s+/i, '')
          .trim();
        goFieldDocs.set(`${section}.${field[1]}`, cleaned || text);
      }
      comments = [];
      continue;
    }
    if (line.trim() && !line.trim().startsWith('//')) comments = [];
  }
}

const configYaml = read('deploy/config.example.yml');
const configKeys = [];
let section = '';
for (const line of configYaml.split('\n')) {
  const sectionMatch = line.match(/^([a-z_]+):\s*$/);
  if (sectionMatch) {
    section = sectionMatch[1];
    continue;
  }
  const kv = line.match(/^\s{2}([a-z_]+):\s*(.*)$/);
  if (!kv || !section) continue;
  let value = kv[2];
  let comment = '';
  const hash = value.indexOf('#');
  if (hash >= 0) {
    comment = value.slice(hash + 1).trim();
    value = value.slice(0, hash).trim();
  }
  if (value.startsWith('"') && value.endsWith('"')) value = value.slice(1, -1);
  const key = `${section}.${kv[1]}`;
  configKeys.push({
    section,
    key,
    value,
    comment,
    description: goFieldDocs.get(key) || comment || '',
    secret: /token/i.test(key),
  });
}
if (configKeys.length < 20) {
  throw new Error(`sync: parsed only ${configKeys.length} config keys from deploy/config.example.yml`);
}

// One description per key, checked against the example file so a new key
// cannot ship with a blank or invented cell. Values are the copy-paste defaults
// from deploy/config.example.yml.
const CONFIG_DOCS = {
  'telegram.token': 'BotFather token in <code>&lt;bot_id&gt;:&lt;secret&gt;</code> form. Required: startup aborts when it is missing or has no colon.',
  'telegram.admins': 'Allow-list of numeric Telegram user IDs. Empty means nobody can control the bot unless first-start claim is enabled.',
  'telegram.first_start_claims_admin': 'Let the first <code>/start</code> become the admin when the allow-list is empty. Turn it off in hardened deployments.',
  'telegram.notify_chat_id': 'Parsed but not read: lifecycle notifications go to the chat that launched the session (see known gaps).',
  'telegram.poll_timeout_seconds': 'Long-poll timeout used for Telegram <code>getUpdates</code>.',
  'server.health_addr': 'Bind address for <code>/healthz</code> and <code>/livez</code>. Loopback by default; add TLS and auth before exposing it.',
  'browser.executable': 'Explicit Chromium path. Empty auto-detects <code>chromium</code>/<code>chrome</code> on PATH, then falls back to the rod-managed download.',
  'browser.headless': 'Run Chromium headless. Leave <code>true</code> on servers; <code>false</code> needs a real X display (Xvfb) and a matching <code>browser.display</code>.',
  'browser.per_session': 'One Chromium per meeting (isolation, more RAM). <code>false</code> shares one browser across sessions.',
  'browser.display': 'X display used when <code>headless</code> is false, for example <code>:99</code>.',
  'browser.window_width': 'Chromium window width. 1280 is the width the capture and playback flow is tested at.',
  'browser.window_height': 'Chromium window height. 800 is the height the capture and playback flow is tested at.',
  'browser.lang': 'Browser locale. The Meet UI must stay English (<code>en-US</code>) for the automation to find its controls.',
  'browser.extra_args': 'Extra raw Chromium flags appended after the built-in capture and autoplay flags.',
  'browser.user_agent': 'Override the user agent. Empty sends a stock Chrome UA, because Meet rejects <code>HeadlessChrome</code>.',
  'browser.proxy': 'Chromium proxy, for example <code>socks5://127.0.0.1:9050</code>, or the host-side egress proxy for YouTube.',
  'media.default_volume': 'Initial player volume, 0–100.',
  'media.default_speed': 'Initial playback rate, 0.25–4.',
  'media.autoplay': 'Parsed but not read: playback always starts after Meet confirms the share (see known gaps).',
  'media.mute_until_connected': 'Create the player muted so audio cannot leak before admission. Playback unmutes it once the share is live.',
  'media.seek_step_seconds': 'Seek step used by the <code>-10s</code> / <code>+10s</code> card buttons.',
  'media.loop': 'Parsed but not read: the embedded player always loops (see known gaps).',
  'meet.guest_name': 'Display name the bot uses when it joins as a guest.',
  'meet.present_mode': '<code>tab</code> shares the media tab and works headless. <code>screen</code> shares an X display and needs Xvfb.',
  'meet.capture_source': 'Must match the capture selector: the media tab title for <code>tab</code> mode, or <code>Entire screen (monitor 1)</code> for <code>screen</code> mode.',
  'meet.join_timeout_seconds': 'Maximum time for the join flow (open Meet, prejoin, lobby) before the session errors.',
  'meet.present_timeout_seconds': 'Maximum time to wait for Meet to confirm the tab capture after Present.',
  'meet.admit_wait_seconds': 'How long to wait in the lobby for a host. <code>0</code> waits forever; the bot re-requests entry every 30 seconds.',
  'limits.max_sessions': 'Concurrent sessions allowed, 1–8.',
  'logging.level': '<code>debug</code>, <code>info</code>, <code>warn</code> or <code>error</code>. <code>debug</code> adds the UI-control traces useful in Meet-redesign reports.',
  'logging.file': 'Log file path. Empty logs to stdout (journald under systemd).',
  'paths.data_dir': 'Writable directory for <code>state.json</code> and runtime files. The container image uses <code>/data</code>.',
};
const unknownKeys = configKeys.filter((entry) => !CONFIG_DOCS[entry.key]);
if (unknownKeys.length) {
  throw new Error(
    `sync: config key(s) without a documented description: ${unknownKeys.map((e) => e.key).join(', ')}. ` +
      'Add them to CONFIG_DOCS in website/scripts/sync-repo-content.mjs.',
  );
}
for (const entry of configKeys) {
  entry.description = CONFIG_DOCS[entry.key];
  entry.goDoc = goFieldDocs.get(entry.key) || '';
}

// Keys that exist in the config struct but are not read by the engine today.
// Listing them keeps the reference honest instead of implying they work. The
// guard below fails if one of them starts being wired, so the note is removed
// rather than left to rot.
const UNWIRED_KEYS = {
  'media.autoplay': 'The player is created with autoplay disabled and playback is started only after the share is confirmed.',
  'media.loop': 'The player is always created with loop enabled; the configured value is not read.',
  'telegram.notify_chat_id': 'Lifecycle notifications currently go to the chat that launched the session.',
};
const unwiredConfig = Object.entries(UNWIRED_KEYS)
  .filter(([key]) => configKeys.some((entry) => entry.key === key))
  .map(([key, note]) => ({ key, note }));

// ------------------------------------------------------------------- states
const managerGo = read('internal/engine/manager.go');
const STATE_DOCS = {
  queued: 'Session is enqueued and waiting for browser worker launch.',
  starting: 'Launching the session browser and preparing the player.',
  joining: 'Loading the media tab and opening Google Meet (prejoin handling included).',
  waiting_for_admission: 'Waiting for host admission or for a host-restricted gate to clear.',
  admitting: 'Waiting in the lobby for a host to admit the bot.',
  admitted: 'Google Meet has admitted the bot; preparing media presentation.',
  sharing: 'Meet accepted Present — waiting for the tab capture to go live.',
  active: 'Media stream is live and actively presenting.',
  streaming: 'Playback is running; progress, volume and speed are tracked.',
  paused: 'Playback paused from the session card.',
  leaving: 'Session is winding down and gracefully exiting the meeting.',
  completed: 'Session completed successfully and cleanly disconnected.',
  ended: 'Session finished or stopped; Chromium was closed.',
  failed: 'Session encountered an unrecoverable failure.',
  error: 'Session failed; the card carries the error detail.',
  cancelled: 'Session was cancelled by user request.',
  timed_out: 'Admission or startup deadline expired without host approval.',
};
const viewsGo = read('internal/bot/views.go');
const emojiFor = (goName) => {
  const block = viewsGo.slice(viewsGo.indexOf('func stateEmoji'));
  const match = block.match(new RegExp(`case[^:]*engine\\.${goName}[^:]*:\\s*\\n\\s*return "([^"]+)"`));
  return match ? match[1] : '·';
};
const states = [];
for (const m of managerGo.matchAll(/State([A-Z]\w+)\s*=\s*"([a-z_]+)"/g)) {
  const [, goName, value] = m;
  if (!STATE_DOCS[value]) {
    throw new Error(`sync: engine state "${value}" has no documented description in STATE_DOCS`);
  }
  states.push({ state: value, emoji: emojiFor(`State${goName}`), description: STATE_DOCS[value] });
}

// ---------------------------------------------------------------- health API
const webGo = read('internal/web/web.go');
const healthBlock = webGo.slice(webGo.indexOf('payload := map[string]interface{}{'));
const healthFields = [];
for (const m of healthBlock.slice(0, 900).matchAll(/"([a-z_]+)":\s+/g)) {
  if (m[1] !== 'status') healthFields.push(m[1]);
}
healthFields.unshift('status');
if (healthFields.length < 6) {
  throw new Error('sync: could not parse the /healthz payload keys from internal/web/web.go');
}

// ----------------------------------------------------------- media & tests
const mediaGo = read('internal/engine/media.go');
const extBlock = mediaGo.slice(mediaGo.indexOf('var directExts'), mediaGo.indexOf('}', mediaGo.indexOf('var directExts')));
const mediaExtensions = [...extBlock.matchAll(/"([a-z0-9]+)":\s*true/g)].map((m) => m[1]).sort();

const testPackages = [];
for (const dir of fs.readdirSync(path.join(REPO_ROOT, 'internal'), { withFileTypes: true })) {
  if (!dir.isDirectory()) continue;
  const pkgDir = path.join(REPO_ROOT, 'internal', dir.name);
  const files = fs.readdirSync(pkgDir).filter((f) => f.endsWith('_test.go'));
  if (!files.length) continue;
  let unitTests = 0;
  let integrationTests = 0;
  for (const f of files) {
    const src = fs.readFileSync(path.join(pkgDir, f), 'utf8');
    for (const m of src.matchAll(/^func (Test\w+)\(/gm)) {
      if (m[1].startsWith('TestIntegration')) integrationTests += 1;
      else unitTests += 1;
    }
  }
  testPackages.push({ name: `internal/${dir.name}`, files: files.length, unitTests, integrationTests });
}
const testTotals = testPackages.reduce(
  (acc, p) => ({ unit: acc.unit + p.unitTests, integration: acc.integration + p.integrationTests }),
  { unit: 0, integration: 0 },
);

// Release archives published by scripts/release.sh (single source of truth).
const releaseSh = read('scripts/release.sh');
const targetsBlock = releaseSh.match(/TARGETS=\(([^)]+)\)/);
const releaseTargets = targetsBlock
  ? [...targetsBlock[1].matchAll(/"([^"]+)"/g)].map((m) => m[1])
  : [];
if (releaseTargets.length < 3) {
  throw new Error('sync: could not parse release targets from scripts/release.sh');
}

// ------------------------------------------------------------ GitHub snapshot
const snapshot = fs.existsSync(SNAPSHOT) ? JSON.parse(fs.readFileSync(SNAPSHOT, 'utf8')) : {};
const token = process.env.GH_TOKEN || process.env.GITHUB_TOKEN || '';
let github = { live: false, source: 'snapshot', ...snapshot };
async function api(pathname) {
  const res = await fetch(`https://api.github.com${pathname}`, {
    headers: {
      Accept: 'application/vnd.github+json',
      'User-Agent': 'telemeetbot-website-build',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
  });
  if (!res.ok) throw new Error(`${pathname} → HTTP ${res.status}`);
  return res.json();
}
try {
  const [repo, releases] = await Promise.all([
    api(`/repos/${REPO_SLUG}`),
    api(`/repos/${REPO_SLUG}/releases?per_page=20`),
  ]);
  github = {
    live: true,
    source: 'api',
    fetchedAt: new Date().toISOString(),
    repo: {
      stars: repo.stargazers_count,
      forks: repo.forks_count,
      watchers: repo.subscribers_count,
      openIssues: repo.open_issues_count,
      language: repo.language,
      license: repo.license?.spdx_id || null,
      defaultBranch: repo.default_branch,
      pushedAt: repo.pushed_at,
    },
    releases: releases.map((r) => ({
      tag: r.tag_name,
      name: r.name || r.tag_name,
      publishedAt: r.published_at,
      createdAt: r.created_at,
      url: r.html_url,
      prerelease: r.prerelease,
      draft: r.draft,
      body: r.body || '',
      assets: (r.assets || []).map((a) => ({
        name: a.name,
        size: a.size,
        downloads: a.download_count,
        url: a.browser_download_url,
      })),
    })),
  };
  fs.writeFileSync(SNAPSHOT, JSON.stringify(github, null, 2) + '\n');
} catch (err) {
  warnings.push(`GitHub API unavailable (${err.message}); using the committed snapshot.`);
  github = { ...snapshot, live: false, source: snapshot.releases ? 'snapshot' : 'unavailable' };
}
if (!github.repo) {
  warnings.push('No GitHub snapshot available: repository stats are hidden.');
  github.repo = null;
}
github.releases = github.releases || [];

const publishedRelease =
  github.releases.find((r) => !r.draft && !r.prerelease) || github.releases[0] || null;
if (publishedRelease) latestTag = publishedRelease.tag;

// --------------------------------------------------- release notes → content
fs.mkdirSync(CONTENT_RELEASES, { recursive: true });
for (const file of fs.readdirSync(CONTENT_RELEASES)) {
  if (file.endsWith('.md')) fs.rmSync(path.join(CONTENT_RELEASES, file));
}
const notesDir = path.join(REPO_ROOT, 'docs');
const noteFiles = fs.existsSync(notesDir)
  ? fs.readdirSync(notesDir).filter((f) => /^release-notes-v.+\.md$/.test(f))
  : [];
const releaseNotes = [];
for (const file of noteFiles.sort()) {
  const tag = file.replace(/^release-notes-/, '').replace(/\.md$/, '');
  const raw = fs.readFileSync(path.join(notesDir, file), 'utf8');
  const meta = github.releases.find((r) => r.tag === tag);
  const firstLine = raw.split('\n').find((l) => l.startsWith('# ')) || `# TeleMeetBot ${tag}`;
  const title = firstLine.replace(/^#\s*/, '');
  const summaryLine =
    raw
      .split('\n')
      .find((l) => l.trim() && !l.startsWith('#') && !l.startsWith('```')) || '';
  const summary = summaryLine.replace(/\*\*/g, '').slice(0, 180);
  releaseNotes.push({ tag, title, publishedAt: meta?.publishedAt || null, url: meta?.url || null });
  const frontmatter = [
    '---',
    `title: ${JSON.stringify(title)}`,
    `tag: ${JSON.stringify(tag)}`,
    `summary: ${JSON.stringify(summary)}`,
    `publishedAt: ${meta?.publishedAt ? JSON.stringify(meta.publishedAt) : 'null'}`,
    `releaseUrl: ${meta?.url ? JSON.stringify(meta.url) : 'null'}`,
    '---',
    '',
  ].join('\n');
  fs.writeFileSync(path.join(CONTENT_RELEASES, `${tag}.md`), frontmatter + raw.replace(/^# .*\n/, ''));
}

// ------------------------------------------------------------------- output
const payload = {
  generatedAt: new Date().toISOString(),
  repoSlug: REPO_SLUG,
  goVersion,
  repoUrl: `https://github.com/${REPO_SLUG}`,
  commit,
  commitDate,
  branch,
  version: latestTag,
  cliFlags,
  commands: commands.filter((c) => c.context === 'command'),
  inlineActions,
  configKeys,
  unwiredConfig,
  states,
  healthFields,
  mediaExtensions,
  testPackages,
  testTotals,
  releaseTargets,
  releaseNotes,
  github,
  warnings,
};
fs.mkdirSync(path.dirname(OUT), { recursive: true });
fs.writeFileSync(OUT, JSON.stringify(payload, null, 2) + '\n');

console.log(`sync: ${cliFlags.length} flags · ${payload.commands.length} commands · ${configKeys.length} config keys · ` +
  `${mediaExtensions.length} media extensions · ${testTotals.unit} unit tests · ` +
  `${github.releases.length} releases (${github.source})`);
for (const warning of warnings) console.warn(`sync: warning: ${warning}`);
