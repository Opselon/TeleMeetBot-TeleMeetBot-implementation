# TeleMeetBot v1.1.0 — presenting that actually works

**A reliability release for the two things that broke in real meetings: Meet refusing to start the
share, and YouTube showing a consent wall or green frames instead of the clip.**

This is a minor bump over v1.0.0 because it adds two diagnostic entry points (`--probe`,
`--media-probe`) and a new consent-handling module. No configuration key was removed or renamed.

## Why upgrade

v1.0.0 could join a meeting and click Present, but three failures were reproducibly fatal:

1. **“Presentation is starting…” forever.** Chromium's auto-select capture matches the
   browser-side tab title. The old title lock used a getter with a no-op setter, so the native
   title never changed and no tab matched. Headless Chromium has no picker to fall back on.
2. **Participants saw “Before you continue to YouTube”** instead of the video: the media tab was
   shared while the consent lightbox owned the page. The old matcher required an exact
   `reject all` label, and real buttons carry full sentences in the visitor's language.
3. **Green or black frames.** Fake media devices plus a software renderer without the right flags
   produced uninitialised YUV buffers, and nothing verified that the shared surface was painting.

All three are fixed and covered by tests that use a real Chromium and a real `getDisplayMedia`
grant, not mocks.

## Highlights

### Presenting

- **Leaf-button matcher** for Meet's Present control: the toolbar container is skipped, so the
  click lands on the actual button.
- **Real tab title.** The player document is served over loopback and the tab title is set through
  the native setter, so `--auto-select-tab-capture-source-by-title` matches `TeleMeet Media` —
  including the title Meet reports.
- **No fake devices.** `use-fake-device-for-media-stream` and `use-fake-ui-for-media-stream` were
  removed so tab capture shares the real tab; camera and microphone are muted on prejoin instead.
- **No present-menu reopen loop.** A failed menu open now dumps the menu DOM and fails with a
  diagnosable error instead of re-clicking in a loop.
- **Survives CDP hiccups.** A transient protocol error no longer kills a live stream.

### Consent and playback

- New `internal/engine/consent.go`: structural detection (consent hosts, DOM markers in light DOM
  *and* shadow roots, visible-only heuristics, text hints), locale phrase tables with reject
  preferred over accept, blocked decoys (`Customize`, `More options`, `Learn more`), one
  normalisation path for both sides (NFD, all combining marks dropped — required for kana),
  and a click ladder: JS → physical mouse → keyboard, then the other action family.
- The wall is also dismissed **after** the watch page reports a buffered video, because YouTube
  mounts it a few seconds late.
- Playback is now *verified*, not assumed: position must advance and sampled pixels must be
  neither black nor decoder-green. The report carries the surface, the URL and the attempt list.
- `recoverMedia` repairs a tab (consent → refit → replay) **without** re-clicking Share Screen:
  the capture track is bound to that tab.
- The watch loop self-heals a consent wall, a stall or a black surface every 10 s, bounded to five
  attempts, and only re-presents after two consecutive misses.

### Networking

- Media traffic can use a proxy (`browser.proxy`, e.g. SOCKS5) while Google Meet traffic bypasses
  it, so WebRTC latency is unaffected and datacenter bot checks apply only to media hosts.
- Headless rendering uses `--headless=new` with SwiftShader ANGLE flags to avoid green frames.

### Joining

- **Host-gated lobbies are waitable.** “No one can join unless invited or admitted by the host”
  keeps the session in `admitting` and re-requests entry every 30 s until
  `meet.admit_wait_seconds` expires. Meeting-full, ended and invalid-code screens still fail fast.
- Debug-grade telemetry: stage, elapsed time and attempt count while gated; explicit joined and
  stream-live milestones; `[STEP n/6]` pipeline logs.

### Diagnostics

- `telemeetbot --probe <meet-url> [--probe-media <url>] [--probe-seconds N]` runs one live session
  through the real engine, streams every stage to the log, serves `/livez` for the window and exits
  without touching Telegram.
- `telemeetbot --media-probe <url>` plays a link in the same player and reports consent state,
  viewport, `overlayFree`, `readyState`, position advance, mean RGB, a verification verdict and a
  human-readable failure. No meeting required.
- The doctor now accepts a health port already served by a live TeleMeetBot instead of reporting a
  conflict.

### Fixes

- Probe event-draining goroutine deadlock on session end.
- `scripts/install.sh`: SIGPIPE on the latest-release lookup, and checksum lines prefixed with
  `./` are handled.
- `scripts/release.sh` writes bare archive names into `SHA256SUMS`.
- Corrected the Docker first-run command in `README.md`, `README.fa.md` and `docker-compose.yml`:
  the compose entrypoint is `tini`, so the binary name is required —
  `docker compose run --rm telemeetbot telemeetbot --wizard --config /data/telemeetbot.yml`.

## New documentation site

`website/` builds a static Astro site (search, dark and light themes, mobile-first layout) that
generates its facts from the Go source at build time: CLI flags, bot commands, config keys, states,
health fields, media extensions, test counts and release targets. Enabled by default:
`https://opselon.github.io/TeleMeetBot-TeleMeetBot-implementation/`.

## Upgrade

Archives are attached below with `SHA256SUMS`. No config migration is needed.

```bash
VERSION=v1.1.0
curl -fsSLO "https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/download/$VERSION/telemeetbot-linux-amd64.tar.gz"
curl -fsSLO "https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/download/$VERSION/SHA256SUMS"
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf telemeetbot-linux-amd64.tar.gz
sudo install -m 0755 telemeetbot /usr/local/bin/telemeetbot
sudo systemctl restart telemeetbot
```

Docker:

```bash
git pull
docker compose build
docker compose up -d
```

Then verify the real capture path on the host, not just the version string:

```bash
telemeetbot --config /etc/telemeetbot/config.yml --check --capture-test
telemeetbot --config /etc/telemeetbot/config.yml --media-probe "<a link you use>" --probe-seconds 15
```

## Behaviour changes to know about

- A host-gated lobby now waits instead of failing on the first restrictive screen. If nobody admits
  the bot, the error arrives after `meet.admit_wait_seconds` (default 5 minutes; `0` waits forever) with both likely
  causes named.
- The bot refuses to present a tab whose consent wall will not clear, and says which controls it
  tried. That is a hard failure by design: presenting the wall is worse than not presenting.
- Media hosts may now resolve through `browser.proxy`; Meet hosts always connect directly.

## Tests

- `go test ./...` — 53 unit tests across config, bot, engine, store, setup, web, health.
- `go test ./internal/engine -run TestIntegration` — 13 real-Chromium tests, including 16 consent
  variants across 13 locales (text-only, shadow DOM, no accept button, no wall, inert wall) and an
  end-to-end case where the wall mounts *after* the share is live and the same capture track keeps
  delivering non-black frames.
- Opt-in live checks (`TELEMEETBOT_YOUTUBE_URL`, `TELEMEETBOT_PROXY`) are never required by CI.
- CI runs gofmt, `go vet`, the unit suite, the Chromium integration suite, a Docker build, and the
  website build with its SEO, contrast and Playwright audits.

## Known limitations

- Guest join only; meetings that require a Google sign-in cannot be joined.
- Meet's UI must be English (`browser.lang: en-US`).
- One presenter per session; two sessions in one meeting compete for the role.
- `media.autoplay`, `media.loop` and `telegram.notify_chat_id` are parsed but not read. Playback
  starts after the share is confirmed, the player always loops, notifications go to the launching
  chat.
- No control API: `/healthz` and `/livez` are read-only, and control is Telegram-only.
- Playback still depends on the source: pages that require a login, block framing or refuse
  automated browsers will not play, and there is no credential injection.

## Changes since v1.0.0

- #1 host gate waitable, `--probe`, richer logging
- #2 YouTube green screen, proxy routing, step logs, media probe
- #3 real capture tab title
- #4 consent wall dismissal and verified playback

Full commit list: `git log v1.0.0..v1.1.0 --oneline`.
