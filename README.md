# TeleMeetBot

[![GitHub Stars](https://img.shields.io/github/stars/Opselon/TeleMeetBot-TeleMeetBot-implementation?style=social)](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation)
[![CI Pipeline](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/actions/workflows/ci.yml/badge.svg)](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/actions/workflows/ci.yml)
[![Docker All-in-One](https://img.shields.io/badge/docker-all--in--one-blue?logo=docker)](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/pkgs/container/telemeetbot-telemeetbot-implementation)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**A 100% Go, headless streaming bot for Google Meet with a Telegram dashboard.**

Send it a meeting link and any media link — YouTube, a direct `.mp4`/`.mp3`, or a public web page —
TeleMeetBot spins up headless Chromium (rod/CDP), joins the meeting as a guest, starts presenting,
plays your link in the shared tab and hands you **pause / resume / seek / volume / speed / mute /
restart / stop** controls straight inside Telegram.

No desktop environment. No Python/Selenium. No external automation service. One binary.

> ⭐ **Support the Project:** If you find TeleMeetBot useful, please consider [giving the repository a star](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation)! Your support motivates ongoing development and improvements.

**Documentation:** [opselon.github.io/TeleMeetBot-TeleMeetBot-implementation](https://opselon.github.io/TeleMeetBot-TeleMeetBot-implementation/) — install, configuration, Meet integration, playback troubleshooting, CLI and deployment.

```
                 ┌────────────────────────────── Telegram ──────────────────────────────┐
                 │  /start ──► live dashboard · cards refresh every few seconds         │
                 │  /new    ──► 2-step wizard (Meet link → media link)                  │
                 │  inline buttons: ⏸ ▶️ 🔇 🔉 🔊 ⏪ ⏩ 🔁 🛑                          │
                 └───────────────┬──────────────────────────────────────────────────────┘
                                 │ long poll (fail-closed admin allow-list)
                                 ▼
              ┌──────────────────────────┐        ┌────────────────────────────────┐
              │   Engine manager         │        │  Chromium (headless, rod/CDP)  │
              │  session state machine   ├───────►│  tab 1: media link (player)     │
              │  play/pause/vol/seek API │        │  tab 2: meet.google.com         │
              │  store + notifications   │        │   └─ present now → tab capture  │
              └──────────────────────────┘        └────────────────────────────────┘
                                 │
                        /healthz · /livez  (local, no remote panel)
```

---

## How the meeting side works (in one paragraph)

The bot launches one headless Chromium per session with capture flags
(`--enable-usermedia-screen-capturing`, `--auto-select-desktop-capture-source=…`,
`--autoplay-policy=no-user-gesture-required`) and a stock Chrome user agent.
Two tabs are opened: the **media tab** (titled `TeleMeet Media`, served from a tiny loopback HTTP
server so Chromium never blocks the media fetch, see title-lock below) and the
**Meet tab** which is driven through the real UI — guest name fill, "Ask to join" / "Join now",
ambient pop-ups dismissed — then *"Present now → A tab"*, auto-selected to the media tab, so
whatever you streamed is what everyone in the call sees and hears.

For a **screen** (Xvfb) mode instead of headless, set `present_mode: "screen"`, run Xvfb
(`xvfb-run` or a your own `:99` display as `display: ":99"`) and set `capture_source:
"Entire screen (monitor 1)"`.

---

## Features

* **2-minute onboarding wizard** — `telemeetbot --wizard` (token, admins, Chromium detection, save, optional systemd/capture-test)
* **Stateful creation workflow** — explicit states (`awaiting_meet_url` → `awaiting_media_url` → `reviewing_configuration`), `/back` keeps validated input, per-field ✏️ Edit buttons on the review card, drafts survive a bot restart (15-minute expiry) and launch consumes them
* **Destructive-action confirmation** — ⏹ Stop re-asks before the bot leaves the meeting; the confirm re-checks the session id so a stale button can never stop a different session
* **Telegram dashboard** — home, sessions, settings, health, per-session control cards that refresh automatically
* **Playback control** — pause/resume, mute/unmute, volume 0–100, speed 0.25–4× (⚡ Speed presets on the session card), seek ±10s/±30s/±60s (verified against the live player), loop, restart, repeat (off / one / all) and shuffle for queued playback
* **Guest display name** — optional per-user Meet display name in Settings; your explicit setting wins over the verified Google identity and the server default, and the review card shows the effective name
* **Subtitles & captions** — 💬 panel per session and in Settings: inspect the real caption tracks (yt-dlp metadata, manual and auto-generated labelled distinctly), pick a language, enable/disable with player read-back (issued vs. confirmed reported separately). Download & Stream fetches the track with the media, converts SRT→WebVTT in-process, serves it over loopback and attaches it as a native `<track>` — no video transcode. Direct YouTube uses the documented captions-module API best-effort; pages control their own captions and say so. Policy (off/auto/preferred/original) and language persist per user in `users.db`, independent of the audio language; per-queue-item overrides win
* **Video quality control** — six policies (Auto, Data Saver ≤360p, Balanced ≤720p, HD 720p, Full HD ≤1080p, Best Available) applied to the real Download & Stream extraction ladder via yt-dlp format selection (ffmpeg merges separate video/audio streams; without ffmpeg the ladder degrades to single-file formats and says so). Direct YouTube playback stays under YouTube's own adaptive player — the dashboard states this limitation and offers the download path instead of faking an enforced resolution. 🔍 Detect formats probes the source with `yt-dlp -J` (no download) and turns each detected height into a cap choice; 📊 Quality & Playback Info separates Requested (your policy), Selected (what the extractor chose), and Observed (what the live player reports) and marks anything unmeasurable as unavailable. Per-item overrides on queue items win over the session policy; the saved default lives in `users.db`
* **Any link** — YouTube (`watch`/`youtu.be`/`shorts`/`live`), direct media (`.mp4 .m4v .webm .mkv .mov .ogv .mp3 .m4a .aac .flac .wav .opus .oga`), and generic pages via a built-in player
* **Smart source detection** — a live HTTP probe (Content-Type, redirects, HEAD→GET fallback) classifies links by what the server actually serves, not by extension: an `.mp3` URL serving HTML is a page; a bare CDN link serving `video/mp4` is direct media
* **Telegram media** — forward an audio/video/document to the bot: it is downloaded through the Bot API, content-sniffed, and played as direct media (never routed through YouTube logic)
* **Saved meetings** — `/new` offers your saved Meet links first (`meetings.db`, SQLite in the data dir); pick one to jump straight to media selection, or start a fresh meeting. Save/list/rename/delete from the same screen
* **Persistent Playlist Queue** — forward multiple audio/video files or paste batches of up to 50 URLs in one go. Control playlist playback with ⏮ Previous / ⏭ Next, jump directly to any queued track, view progress (e.g. `3/50`), and manage upcoming tracks.
* **Resilient Google Meet Admission** — explicit, observable state machine (`QUEUED` → `STARTING` → `JOINING` → `WAITING_FOR_ADMISSION` → `ADMITTED` → `ACTIVE` → `LEAVING` → `COMPLETED` / `TIMED_OUT` / `CANCELLED`). Single-flight meeting room deduplication, debounced join clicks, native input synchronization, and jittered bounded backoff for host-restricted meetings.
* **Session store** — `state.json` history (max 50 records), survives restarts (corrupt file → kept aside as `*.corrupt-*`, fresh store)
* **Per-user settings (SQLite)** — audio language, media mode, start volume/speed, auto-advance and repeat are stored per Telegram user id in `users.db`; the ⚙️ Settings screen edits them live, values survive restarts, and legacy `state.json` preferences migrate automatically (backed up, idempotent)
* **Queue screen** — per-item selection with pagination, progress (`3/20`) with position and duration, repeat/shuffle toggles, add-media and play-next flows, load-playlist in place of the upcoming items, clear with confirmation; queue metadata is durable in `users.db` and restart-interrupted sessions are reported honestly instead of pretending playback resumed
* **Health surface** — `/healthz` (JSON with sessions/uptime/browser/disk/config) + `/livez`; **no** remote control endpoints
* **Diagnostics** — `--check` (doctor) and `--check --capture-test` verifies real `getDisplayMedia` capture: frame changes + audio track
* **Operations** — Dockerfile + compose (healthcheck, shm), unit file, one-command installer, cross-arch release archives (`release.sh`)

---

## Quick start

### Option A — Turnkey 1-Command Deploy & Easy Migration (Fastest)

Deploy instantly on any fresh server with Docker:

```bash
# Automated launch with sensible defaults and health checks
curl -fsSL https://raw.githubusercontent.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/main/scripts/deploy.sh | sh
```

**Moving to a new server in 1 minute?**
```bash
# 1. On your old server, take a clean backup snapshot:
./scripts/backup.sh /root/telemeetbot-data .

# 2. Transfer the backup file to your new server and run:
./scripts/deploy.sh --restore telemeetbot-backup-YYYYMMDD_HHMMSS.tar.gz
```
All bot settings, Google OAuth profiles, SQLite meeting favorites, and playlists are restored seamlessly.

### Option B — Docker Compose

```bash
git clone https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation
cd TeleMeetBot-TeleMeetBot-implementation

# Copy and customize environment variables:
cp .env.example .env

# Start with Docker Compose:
docker compose up -d
docker compose logs -f
curl http://127.0.0.1:18080/healthz
```

### Option C — one-command install (bare metal)

### Option C — One-Command Install & Native Installers (Bare Metal)

**Windows (Native Setup Wizard or PowerShell):**

1. **Native Installer Wizard (Easiest)**: Download `TeleMeetBot-Setup-windows-amd64.exe` (or `TeleMeetBot-Setup-windows-arm64.exe`) from the [latest GitHub Release](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/latest) and run it. It guides you step-by-step through profile selection, dependency setup, Telegram token configuration, and Windows service registration.
2. **PowerShell One-Liner**:
```powershell
irm https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/latest/download/install.ps1 | iex
```

**Linux / macOS (POSIX sh):**

```bash
curl -fsSL https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/latest/download/install.sh | sh
```

The installer detects the OS and CPU architecture (`amd64`/`arm64`/`arm v6, v7`/`386`), dependencies,
permissions and whether it is running elevated, resolves the latest **release tag**, downloads the
matching archive and **verifies it against the release's published SHA-256 checksums** before
touching anything. It then installs the binary (`/usr/local/bin/telemeetbot` or
`%ProgramFiles%\TeleMeetBot`), **runs it and matches its `--version` against the release tag** so a
corrupt or mismatched download can never be reported as a successful install, stages a template
config (`deploy/config.example.yml`) without overwriting an existing one, runs the interactive
setup wizard to finalize your configuration, and registers the OS service (systemd on Linux, Windows SCM, or launchd on macOS).

> The installer is written in POSIX `sh`, not bash: the one-command install pipes it straight into
> `sh`, which is `dash` on Debian/Ubuntu and `busybox` on Alpine. It is tested under both in CI.

Pin a specific release (recommended for unattended installs):

```bash
curl -fsSL https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/download/v1.5.0/install.sh | sh -s -- --version v1.5.0
```

Other flags: `--version vX.Y.Z` · `--prefix DIR` · `--config-dir DIR` · `--data-dir DIR`
· `--no-wizard` · `--no-service` · `--no-start` · `--dry-run`.

Verify a downloaded archive yourself before installing:

```bash
curl -fsSLO https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/download/v1.5.0/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

### Installation Profiles & Dependency Manager

TeleMeetBot supports two distinct installation profiles:
* **Full Installation (Recommended)**: Enables Google Meet WebRTC streaming and the high-speed Download & Stream media engine. Configures Chromium, standalone `yt-dlp`, `FFmpeg`, and optionally `Deno` for solving YouTube JS challenges.
* **Minimal Installation**: Core Google Meet presentation mode only. Uses Chromium for direct browser playback; media downloading is disabled (`download.enabled: false`).

**Dependencies & Runtime Isolation:**
* End users do **not** need to install Go, Python, Selenium, Node.js, compilers, or build toolchains.
* yt-dlp is executed as a standalone binary or managed package without requiring Python toolchains.
* Package managers (`apt-get`, `dnf`, `pacman`, `apk`, `zypper` on Linux; `winget` on Windows; `brew` on macOS) are detected and used automatically when confirming missing dependencies.

**Lifecycle and Diagnostic Commands:**

```bash
telemeetbot setup                            # run canonical 9-step interactive setup wizard
telemeetbot doctor                           # run doctor diagnostics (alias: telemeetbot --check)
telemeetbot --restore-backup                 # roll the config back to that backup (.bak)
telemeetbot uninstall                        # stop + remove the service and the binary
telemeetbot uninstall --keep-binary          # remove only the service registration
# Service checks:
# Linux:   systemctl status telemeetbot | journalctl -u telemeetbot -f
# macOS:   launchctl print gui/$(id -u)/telemeetbot
# Windows: sc.exe query telemeetbot | Get-EventLog -LogName Application -Source telemeetbot
```

> The uninstall never deletes your config, data directory, SQLite databases, or session history. Remove them manually for a full purge.

### Option D — Build from Source

```bash
go build -o telemeetbot ./cmd/telemeetbot   # Go 1.26+
./telemeetbot --wizard                      # creates ./telemeetbot.yml
./telemeetbot                               # run (also ./telemeetbot --run)
```

### Create the bot token

1. Talk to [@BotFather](https://t.me/BotFather) in Telegram → `/newbot` → copy the token.
2. Get your numeric user id: send `/id` to [@userinfobot](https://t.me/userinfobot) (or set
   `first_start_claims_admin: true` and press `/start` first — the first user to do that becomes admin).

### First run in Telegram

```
/start            → dashboard (Active sessions, uptime, keyboard)
/new              → saved meetings picker (📌 your links, 🆕 New Google Meet, ✖️ Cancel)
                  → pick a saved link → step 2/2: paste the media link
                  → or 🆕 New Google Meet → step 1/2: paste the Meet link
                  → "Ready to launch" → 🚀 Launch
```

You can also just type or paste a Meet link while the picker is up — it is
accepted directly. Meeting links used by a launched session are saved
automatically and offered by `/new` next time (rename or delete them from
**📋 Manage saved**).

The bot then: launches Chromium → opens the media tab (title-locked) → opens Meet → joins as
guest → clicks Present → shares the media tab → turns playback on → sends you a session card
and keeps it live.

---

## Commands & inline buttons

| Command | Meaning |
|---|---|
| `/start` | open the dashboard |
| `/new` | start a new stream wizard |
| `/new <meet> <media>` | one-shot: both links in one message (space separated) |
| `/sessions` | list running sessions |
| `/health` | health + doctor-style snapshot |
| `/settings` | your per-user settings (audio, mode, volume/speed, auto-advance, repeat, guest name) |
| `/help` | usage overview |
| `/stop <id>` | open the stop confirmation for a session |
| `/cancel` | abort an open wizard (draft deleted) |
| `/back` | step back in the creation wizard, keeping validated input |
| `/home` | return to the dashboard without touching running sessions |
| `/id` | show your Telegram user id |
| `/ping` | liveness probe |
| `/playlists` | open your persistent playlist library |
| `/queue [id]` | open the queue of a session |
| `/manage` | open the management console (aliases: `/admin`, `/dashboard`) |

Per-session **inline card** (`act:<id>:…`):

```
⏸ Pause      ▶️ Resume       🔁 Restart
🔇 Mute      🔊 Unmute       (toggle: first button is voluntary)
🔉 Vol-      🔊 Vol+         (±10%)
⏪ -10s      ⏩ +10s
🔄 Refresh   🛑 Stop
```

The card header includes state emoji: 🟦 Guest · 🟪 Harness · 🟫 Streaming · 🟨 Paused ·
🟩 Onboard · ⚪️ Ended · 🔴 Error (see `internal/bot/views.go` for exact scheme).

### Playlists and the management console

The dashboard also has two larger surfaces:

- **📚 Playlists** — a persistent per-user library of media links. Playlists can
  be renamed, reordered, duplicated, exported, imported, launched as a new
  session, or appended to a live session queue. They live in their own
  `playlists.db` and survive restarts.
- **🎛 Manage** — one console for users, admins, storage, maintenance,
  backups and broadcast. Admins can be added or removed at runtime; backups are
  consistent SQLite snapshots written with `VACUUM INTO`.

Every session card gained **🎛 Tools** (shuffle/sort/reorder upcoming items,
clear options) and **💾 Save queue** (copy the live queue into a playlist).

See [docs/management.md](docs/management.md) for screens, limits, import
format, backup layout and recovery notes.

---

## Sizing & resource use

One session is one Chromium (media tab + Meet tab) decoding the media and
re-encoding the shared tab for the call. Measured on a 2 vCPU / 3.8 GB VPS
(Debian bookworm Chromium 155, 1280x800 tab, 720p30 H.264 source):

| What | CPU (of one core) | Private RAM |
|---|---|---|
| idle bot, no session | ~0.1 % | ~10 MB |
| one session, tab share (current defaults) | ~14 % | ~230 MB |
| one session, tab share with `gpu: swiftshader` (pre-1.2 default) | ~84 % | ~300 MB |
| host-gated join wait, before pacing fix | ~1.2 DOM polls/s → card edit per poll | — |
| host-gated join wait, after pacing fix | 0.2 polls/s, stable card, 1 reload/30 s | — |

Practical guidance:

- **`limits.max_sessions: 1` on a 2-core VPS.** Two concurrent sessions need
  two browsers plus two Meet encodes and will saturate both cores.
- Keep **`browser.gpu: "off"`** (the default) unless the host has a real GPU;
  SwiftShader software GL costs ~6x the CPU for the same frames.
- `GOMEMLIMIT` (256 MiB in the image) bounds the Go heap; the browser, not the
  bot, is the memory consumer. Give the container a generous `/dev/shm`
  (`shm_size: "1gb"`).
- `logging.level: debug` writes a line per pipeline step; use `info` in production.

## Configuration reference

`telemeetbot.yml` (created by the wizard; omitted keys keep defaults):

```yaml
telegram:
  token: "123456789:AA..."            # required
  admins: []                          # allow-list (fail-closed when empty)
  first_start_claims_admin: true      # first /start wins if admins is empty
  notify_chat_id: 0                   # 0 = notify the launching admin
  poll_timeout_seconds: 30

server:
  health_addr: "127.0.0.1:18080"      # /healthz + /livez (bind 0.0.0.0 in docker)

browser:
  executable: ""                      # auto-detect, else rod downloads managed Chromium
  headless: true                      # false needs a display (Xvfb) — set display
  per_session: true                   # one Chromium per meeting (isolation)
  display: ":99"                      # X display when headless=false
  window_width: 1280
  window_height: 800
  lang: "en-US"                       # Meet UI language must be English
  gpu: "off"                          # off (software compositing, default in headless)
                                      # | swiftshader | auto (real GPU host)
  extra_args: []                      # raw chromium flags appended
  user_agent: ""                      # empty = stock Chrome UA override
  proxy: ""                           # e.g. socks5://127.0.0.1:9050

media:
  default_volume: 80
  default_speed: 1.0                  # 0.25 .. 4
  autoplay: true
  mute_until_connected: true
  seek_step_seconds: 10
  loop: false
  skip_youtube_ads: true              # press YouTube's own "Skip ads" button the
                                      # moment it becomes available; never blocks
                                      # or hides ads, non-YouTube media untouched

meet:
  guest_name: "TeleMeet Bot"
  present_mode: "tab"                 # "tab" (headless-safe) | "screen" (Xvfb)
  capture_source: "TeleMeet Media"    # tab title or "Entire screen (monitor 1)"
  join_timeout_seconds: 120
  present_timeout_seconds: 45
  admit_wait_seconds: 300             # how long we wait for organizer admission

limits:
  max_sessions: 2

logging:
  level: "info"                       # debug|info|warn|error
  file: ""                            # empty = stdout

paths:
  data_dir: "data"                    # state.json + downloads/ live here

download:
  enabled: true                      # Download & Stream feature (Browser mode always works)
  dir: ""                             # empty = <data_dir>/downloads (recommended)
  ttl_minutes: 60                    # orphan cleanup: reclaims files no session is streaming; an active session keeps its file for its whole lifetime
  max_size_mb: 2048                  # per-file ceiling (0 keeps the 2048 default)
  min_free_mb: 256                   # minimum free disk space required before download
  timeout_seconds: 600               # one whole download, transcode included
  max_concurrent: 1                  # bounded parallel downloads (0..8)
  ytdlp_path: ""                     # empty = auto-detect yt-dlp on PATH
  ffmpeg_path: ""                    # empty = auto-detect ffmpeg on PATH
  allow_transcode: true              # remux mkv/avi/ogv to mp4 for Chromium
```

Validation is strict (`internal/config/config.go`): invalid token shape, no reachable admins
(token set but `admins` empty and `first_start_claims_admin=false`) or `present_mode` outside
`tab|screen` all abort startup with explicit errors.

---

## Diagnostics

```bash
./telemeetbot --config telemeetbot.yml --check              # doctor
./telemeetbot --config telemeetbot.yml --check --capture-test  # + real capture self-test
```

Doctor checks: config parse/valid, Chromium presence + version, writable data dir +
1.5 GiB free disk, health port free/bound, Telegram token via `getMe` (validates against api.telegram.org),ame, outbound
HTTPS to `google.com`/`youtube.com` (skippable), `xvfb` for `screen` mode, and (with
`--capture-test`) a real `getDisplayMedia` probe proving **video frames change and an
audio track is present**.

The capture probe result is printed as JSON: `{"granted":true,"video":true,"frames_changed":4, "audio":true, …}`

### Common failures

| Symptom | Cause / fix |
|---|---|
| Session stuck at `Guest` (`Waiting for admission`) | Meeting disables anonymous joins or host left. Enable guest joining in Meet settings, or use a meeting that allows bots. |
| `admission timeout` | Same as above; reduce `admit_wait_seconds` if you want faster failures. |
| `present can't start` | Chromium too old (use ≥ 118), or Meet UI changed language (we require `browse.lang: en-US`), or `capture_source` mismatch — run `--check --capture-test`. |
| `media doesn't play` in a web page | The page may block embedding; log shows `playerProblem` hints; restart via button. |
| Share shows **"Before you continue to YouTube"** | The consent wall was not cleared. The bot dismisses it structurally (reject-all preferred, locale-independent phrase tables, light-DOM + shadow-DOM, `consent.youtube.com` redirect) and refuses to present a tab whose wall won't clear — the session error carries the surface, the buttons it saw and what it tried. Run `--media-probe <link>` for the same diagnostics without a meeting. |
| `no such element` after a Meet redesign | Open an issue with `(meetctl)` debug logs — turn `log.level: debug`. |
| Chromium keeps crashing | `--check` shows RAM; add `shm_size: "1gb"` in Docker and set `per_session: true`. |

---

## Testing

```bash
make test                # unit tests (no Chromium needed)
make test-integration    # adds real-Chromium tests (title-lock, player doc, getDisplayMedia)
```

Integration tests automatically locate `${TELEMEETBOT_BROWSER}` or a PATH `chromium/chrome`,
skip cleanly when absent, and are also run in CI (`browser-actions/setup-chrome`).

Two optional live checks (never required, so CI stays hermetic and offline):

```bash
# real YouTube: consent wall → ready video → advancing, non-black frames
TELEMEETBOT_YOUTUBE_URL=https://www.youtube.com/watch?v=jNQXAC9IVRw \
TELEMEETBOT_PROXY=http://host:port TELEMEETBOT_BROWSER=/path/to/chrome \
  go test ./internal/engine -run TestIntegrationRealYouTube -v
```

`TestIntegrationPresentationSurvivesLateConsentWall` is the hermetic version of the
production failure: it prepares a YouTube-shaped tab, shares it with a real
`getDisplayMedia`, mounts the consent wall *after* the share is live, runs
`recoverMedia`, and asserts the same capture track keeps delivering non-black
frames — i.e. the share is never restarted.

`make check` runs the real `--check --capture-test` self-test against your installed Chromium.

---

## Security model

* **Fail-closed admin allow-list**: every update authenticate against `telegram.admins`;
  an empty list with `first_start_claims_admin: false` means nobody can use the bot.
* The **only** HTTP surface is `GET /healthz` and `GET /livez` — no state, no writes —
  bind it to `127.0.0.1` unless you front it with TLS/auth.
* Config files with the bot token are written `0640` (wizard/`FirstStart` claim path).
* Chromium profiles are per-session temp dirs removed at end; `--no-sandbox` is required
  (standard for containers) — run the bot as a dedicated, low-privileged user.
* `state.json` contains meeting URLs — cap history and protect the data dir like any secret.
* `meetings.db` (SQLite, WAL mode) holds saved meeting links per owner; every read and write is
  scoped by `owner_id`, so one user can never list, rename or delete another user's rows. It lives
  in the configured `data_dir` next to `state.json` and is backed up (`*.bak-<unix>`) at startup.
* `users.db` (SQLite, WAL mode) holds per-user preferences and session-queue metadata with the
  same owner-scoped reads and writes, schema-versioned idempotent migrations and a startup backup.
  The one-shot `state.json` → SQLite preference migration rewrites the JSON file only after the
  copy succeeds and never overwrites a newer value already saved in the database.

---

## Development

```
cmd/        entrypoint, flags
internal/
  bot/      Telegram layer: updates, dashboard, wizard, session cards
  engine/   CDP/rod engine: browser, sessions, media, meet, capture test
  config/   YAML config + strict validation
  store/    session history
  setup/    interactive wizard, doctor (diagnostics)
  health/   runtime health snapshot
  web/      /healthz + /livez only
deploy/     systemd unit example
scripts/    install.sh, install.ps1, release.sh
docs/       release notes
.github/    CI (tests + real-Chromium integration, docker build)
```

Build a release:

```bash
VERSION=v1.0.0 make release     # dist/*.tar.gz for linux/amd64,arm64,armv6,armv7,386
VERSION=v1.0.0 make publish     # + tag push + `gh release create`
```

---

## Roadmap

* Stream **to** meetings as presenter with configurable layout (camera compositing)
* "Join with a Google account" mode (account password / user-data dir)
* Scheduling: cron-like "join at HH:MM"
* HTTP admin API (opt-in, token auth) alongside the Telegram dashboard

## License

MIT — see [LICENSE](LICENSE).
