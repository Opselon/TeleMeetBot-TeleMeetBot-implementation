# TeleMeetBot v1.0.0 — first release

**The first public release: a 100% Go, headless Google Meet streaming bot with a Telegram dashboard.**

## Highlights

* Headless Chromium streaming engine driven by rod/CDP:
  `--enable-usermedia-screen-capturing`,
  `--auto-select-desktop-capture-source=…`, autoplay permitted, stock Chrome UA override.
* Real guest-flow automation in Google Meet: guest name → join → Present now → tab capture.
* Telegram dashboard: home, sessions, settings, health + live session cards
  (pause/resume, mute/unmute, volume ±, seek ±10 s, restart, stop, refresh).
* 2-minute setup wizard (`telemeetbot --wizard`), doctor (`--check`) and a real
  getDisplayMedia capture self-test (`--check --capture-test`).
* Per-session isolation, strict fail-closed admin allow-list, first-start admin claim.
* Operation ready: Dockerfile + compose (1 GiB shm, healthcheck), systemd unit,
  one-command installer, cross-arch release archives:
  linux/amd64 · linux/arm64 · linux/armv6 · linux/armv7 · linux/386.
* Surfaces of note: only `/healthz` + `/livez` are exposed (no remote control HTTP).
* Tests: unit suites for config/wizard/bot/store/web plus real-Chromium integration
  tests, also running in CI with `browser-actions/setup-chrome`.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/main/scripts/install.sh | bash
```

or Docker:

```bash
docker compose run --rm telemeetbot --wizard --config /data/telemeetbot.yml
docker compose up -d
```

SHA256 checksums for the archives are attached below (`SHA256SUMS`).
