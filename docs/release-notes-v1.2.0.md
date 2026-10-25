# TeleMeetBot v1.2.0 — one command, every platform

**The install release: a verified one-command installer for Windows, Linux and macOS, a setup
wizard that works everywhere, and the safe lifecycle (upgrade, backup, rollback, uninstall)
around it.**

Minor bump over v1.1.0: no configuration key was removed or renamed, and every existing config
continues to load. The install surface is new; the engine, bot and website are unchanged.

## Install in one command

**Linux / macOS:**

```bash
curl -fsSL https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/latest/download/install.sh | sh
```

**Windows (PowerShell 5.1+):**

```powershell
irm https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/latest/download/install.ps1 | iex
```

**Docker:**

```bash
docker compose run --rm telemeetbot telemeetbot --wizard --config /data/telemeetbot.yml
docker compose up -d
```

Each installer:

1. detects the OS and CPU architecture (`amd64` / `arm64` / `armv6` / `armv7` / `386`),
   dependencies, permissions and whether it is running elevated;
2. resolves the latest **release tag** and refuses anything that does not look like one;
3. downloads the matching archive and **verifies it against the release's published SHA-256
   checksums** — a tampered or corrupted archive aborts the install before anything is written;
4. installs the binary (`/usr/local/bin/telemeetbot`, `$env:ProgramFiles\TeleMeetBot`, or a
   per-user fallback when not elevated);
5. stages a fail-closed template config — an existing config is **never** overwritten;
6. installs the OS service: systemd unit on Linux, a Windows service through the SCM, a launchd
   agent on macOS;
7. runs the interactive setup wizard, then verifies the health endpoint.

Pin a specific release for unattended installs:

```bash
curl -fsSL https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/download/v1.2.0/install.sh | sh -s -- --version v1.2.0
```

Verify an archive yourself before installing:

```bash
curl -fsSLO https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/download/v1.2.0/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

## Native binaries for every supported platform

Built and smoke-tested on real runners, not just cross-compiled:

| Platform | Archive |
| --- | --- |
| Linux amd64 / arm64 / armv6 / armv7 / 386 | `telemeetbot-linux-*.tar.gz` |
| macOS amd64 (Intel) / arm64 (Apple Silicon) | `telemeetbot-darwin-*.tar.gz` |
| Windows amd64 / arm64 | `telemeetbot-windows-*.zip` |

One `SHA256SUMS` manifest covers every archive in the release. The multi-arch Docker image is
published as `ghcr.io/opselon/telemeetbot-telemeetbot-implementation:v1.2.0` (and `:latest`).

## Setup wizard

- One Chromium question per platform: `apt-get` on Debian/Ubuntu, Homebrew on macOS, `winget` on
  Windows. Platforms without a supported package manager print a manual hint instead of running a
  wrong command.
- One service question per platform through the new `internal/svcinst` package: a hardened
  systemd unit (`NoNewPrivileges`, `ProtectSystem=full`, `PrivateTmp`), a Windows service with a
  restart-on-failure policy, a launchd agent with `KeepAlive`.
- **Re-running the wizard over a live install is safe**: the previous config is backed up to
  `config.yml.bak` before the new one is written.

## Lifecycle

```bash
telemeetbot --wizard                  # re-run setup (backs up the previous config)
telemeetbot --restore-backup          # roll the config back to that backup
telemeetbot --check                   # doctor: config, browser, permissions, disk, network
telemeetbot --uninstall               # stop + remove the service, then the binary
telemeetbot --uninstall --keep-binary # remove only the service registration
```

- **`--uninstall` never deletes the config, the data directory or the session history** — the
  bot token and saved meetings survive an uninstall so a re-install picks up where you left off.
- Uninstalling a **running** service requires `--yes` or a typed `yes`; without confirmation it
  fails closed and leaves the bot alone.
- The uninstall refuses to delete a binary that sits in a source checkout (a developer build),
  and converges when run twice.

## Windows service support

The binary now runs natively under the Windows Service Control Manager: it auto-detects when it
was started by the SCM, runs the bot in the service loop, reports state transitions and honours
the 30-second Stop window with a clean shutdown. Signal handling was split per platform because
SIGTERM cannot be caught on Windows. Health, disk-space and media-manager code that used
`statfs(2)` now goes through a per-platform disk layer, so the whole tree builds and runs on
Windows and macOS.

## Upgrading from v1.1.0

- Replace the binary; the config format is unchanged. The installer does this without touching
  an existing config.
- If you installed v1.0.0/v1.1.0 with the old `raw.githubusercontent.com` bootstrapping URL,
  switch to the release-pinned command above: it resolves a real tag and verifies checksums.
- The CI upgrade-path job proves a v1.1.0-era config loads, normalizes and validates under the
  new binary, and that a wizard re-run merges over it instead of dropping operator keys.

## Checksums

Every artifact in this release is covered by `SHA256SUMS`. Download it from the release and run
`sha256sum -c SHA256SUMS --ignore-missing` before installing.
