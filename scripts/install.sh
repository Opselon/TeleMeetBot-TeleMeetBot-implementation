#!/usr/bin/env bash
# TeleMeetBot one-command installer (Linux/macOS).
#
#   curl -fsSL https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/latest/download/install.sh | sh
#
# What it does:
#   1. detects OS, architecture, dependencies, permissions and runtime
#   2. downloads the matching release archive from a pinned trusted release
#   3. verifies it against the release's published SHA-256 checksums
#   4. installs the telemeetbot binary
#   5. stages a fail-closed template config (never overwrites an existing one)
#   6. installs the OS service (systemd on Linux, launchd on macOS) — optional
#   7. runs the interactive setup wizard (unless --no-wizard), then verifies health
#
# Flags: --version vX.Y.Z  --prefix DIR  --config-dir DIR  --data-dir DIR
#        --no-wizard  --no-service  --no-start  --dry-run
#
# Environment overrides: REPO, VERSION, PREFIX, CONFIG_DIR, DATA_DIR, BASE_URL
#
# BASE_URL swaps the download root (default https://github.com/$REPO/releases/download)
# so the install path can be tested against a local mirror. Release asset
# names stay exactly the same.
set -euo pipefail

REPO="${REPO:-Opselon/TeleMeetBot-TeleMeetBot-implementation}"
VERSION="${VERSION:-latest}"
PREFIX="${PREFIX:-/usr/local}"
CONFIG_DIR="${CONFIG_DIR:-/etc/telemeetbot}"
DATA_DIR="${DATA_DIR:-}"
BASE_URL="${BASE_URL:-}"
WIZARD=1
SERVICE=1
START=1
DRY_RUN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="${2:?}"; shift 2 ;;
    --prefix) PREFIX="${2:?}"; shift 2 ;;
    --config-dir) CONFIG_DIR="${2:?}"; shift 2 ;;
    --data-dir) DATA_DIR="${2:?}"; shift 2 ;;
    --no-wizard) WIZARD=0; shift ;;
    --no-service) SERVICE=0; shift ;;
    --no-start) START=0; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) sed -n '2,21p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarn:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ---- 1. Platform detection ------------------------------------------------
OS="$(uname -s)"
case "$OS" in
  Linux)  PLATFORM=linux ;;
  Darwin) PLATFORM=darwin ;;
  *) die "TeleMeetBot ships a native binary for Linux and macOS only; on Windows use install.ps1 or Docker (got $OS)" ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  # Release archives are named arm6/arm7 (they carry GOARM verbatim, not the
  # uname -m spelling), so map the kernel name onto the archive name.
  armv7l)        ARCH=arm7 ;;
  armv6l)        ARCH=arm6 ;;
  i386|i686)     ARCH=386 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

command -v curl >/dev/null || command -v wget >/dev/null \
  || die "curl or wget is required"
command -v tar >/dev/null || die "tar is required"

# ---- permission detection -------------------------------------------------
SUDO=""
if [[ "$(id -u)" -ne 0 ]]; then
  if command -v sudo >/dev/null; then
    SUDO="sudo"
  elif [[ "$PREFIX" == /usr/local || "$PREFIX" == /usr || "$PREFIX" == /opt ]]; then
    die "installing to $PREFIX needs root (run with sudo or pass --prefix \$HOME/.local)"
  fi
fi

# ---- 2. Resolve the pinned trusted release --------------------------------
if [[ "$VERSION" == "latest" ]]; then
  log "resolving the latest release"
  # Read the whole body first: piping into an early-exiting grep trips SIGPIPE
  # under `set -o pipefail` and aborts the installer.
  latest_json="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest")" \
    || die "cannot reach the GitHub API (check connectivity)"
  VERSION="$(printf '%s' "$latest_json" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)"
  [[ -n "$VERSION" ]] || die "could not resolve the latest release"
fi
[[ "$VERSION" == v* ]] || die "resolved version '$VERSION' does not look like a tag (vX.Y.Z); refusing to install"

FILE="telemeetbot-${PLATFORM}-${ARCH}.tar.gz"
# BASE_URL replaces the *whole* download root, so a mirror serves the release
# assets directly (BASE_URL/SHA256SUMS, BASE_URL/<archive>). The default root
# is the GitHub release download directory for the pinned version. Either way
# the asset names are identical, which keeps the checksum verification honest.
BASE="${BASE_URL:-https://github.com/${REPO}/releases/download/${VERSION}}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ---- 3. Download + checksum verify ----------------------------------------
fetch() { # fetch <url> <dest>
  if command -v curl >/dev/null; then
    curl -fL --retry 3 --connect-timeout 20 -o "$2" "$1"
  else
    wget -q --tries=3 --timeout=20 -O "$2" "$1"
  fi
}

log "downloading TeleMeetBot ${VERSION} (${PLATFORM}/${ARCH})"
fetch "$BASE/$FILE" "$TMP/$FILE" || die "download failed: $BASE/$FILE"
fetch "$BASE/SHA256SUMS" "$TMP/SHA256SUMS" || die "download failed: $BASE/SHA256SUMS"

log "verifying SHA-256 checksum"
# SHA256SUMS lines carry bare names; tolerate an optional ./ or .\ prefix
# (the Windows build of sha256sum writes the latter).
sum_re=" *\\.?[/\\\\]?${FILE//./\\.}\$"
( cd "$TMP" && grep -E "$sum_re" SHA256SUMS | sha256sum -c - ) \
  || die "checksum mismatch — the archive is corrupt or the download was tampered with; aborting"

if [[ "$DRY_RUN" -eq 1 ]]; then
  log "dry run: verified $FILE, would install to ${PREFIX}/bin/telemeetbot"
  exit 0
fi

tar -C "$TMP" -xzf "$TMP/$FILE"
[[ -x "$TMP/telemeetbot" ]] || die "archive does not contain the binary"

# ---- 4. Install the binary ------------------------------------------------
log "installing binary to ${PREFIX}/bin/telemeetbot"
$SUDO install -d -m 0755 "$PREFIX/bin"
$SUDO install -m 0755 "$TMP/telemeetbot" "$PREFIX/bin/telemeetbot"

# ---- 5. Config + data dirs ------------------------------------------------
[[ -n "$DATA_DIR" ]] || DATA_DIR="$CONFIG_DIR/data"
$SUDO install -d -m 0750 "$CONFIG_DIR"
if [[ ! -f "$CONFIG_DIR/config.yml" && -f "$TMP/deploy/config.example.yml" ]]; then
  $SUDO install -m 0640 "$TMP/deploy/config.example.yml" "$CONFIG_DIR/config.yml"
  log "staged a template config at ${CONFIG_DIR}/config.yml — the wizard will edit it"
fi

# ---- 6. OS service --------------------------------------------------------
install_systemd() {
  [[ -f "$TMP/deploy/telemeetbot.service" ]] || return 0
  log "installing systemd unit"
  $SUDO install -m 0644 "$TMP/deploy/telemeetbot.service" /etc/systemd/system/telemeetbot.service
  $SUDO systemctl daemon-reload
}

install_launchd() {
  local plist="$HOME/Library/LaunchAgents/telemeetbot.plist"
  log "installing launchd agent"
  mkdir -p "$(dirname "$plist")"
  printf '%s\n' \
'<?xml version="1.0" encoding="UTF-8"?>' \
'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
'<plist version="1.0">' \
'<dict>' \
'    <key>Label</key>' \
'    <string>telemeetbot</string>' \
'    <key>ProgramArguments</key>' \
'    <array>' \
"        <string>${PREFIX}/bin/telemeetbot</string>" \
'        <string>--config</string>' \
"        <string>${CONFIG_DIR}/config.yml</string>" \
'    </array>' \
'    <key>RunAtLoad</key>' \
'    <true/>' \
'    <key>KeepAlive</key>' \
'    <dict><key>SuccessfulExit</key><false/></dict>' \
'</dict>' \
'</plist>' > "$plist"
  chmod 0644 "$plist"
}

if [[ "$SERVICE" -eq 1 ]]; then
  case "$PLATFORM" in
    linux)
      if command -v systemctl >/dev/null 2>&1 && $SUDO systemctl is-system-running >/dev/null 2>&1; then
        install_systemd
      else
        warn "systemd is not running on this host; skipped the unit install. Start the bot manually."
      fi
      ;;
    darwin)
      if command -v launchctl >/dev/null 2>&1; then
        install_launchd
      else
        warn "launchctl not found; skipped the agent install. Start the bot manually."
      fi
      ;;
  esac
fi

# ---- Chromium availability check ------------------------------------------
if ! command -v chromium >/dev/null 2>&1 && ! command -v chromium-browser >/dev/null 2>&1 \
   && ! command -v google-chrome >/dev/null 2>&1 && ! command -v google-chrome-stable >/dev/null 2>&1; then
  warn "no Chromium found — the setup wizard can install one for you (or use Docker)"
fi

# ---- 7. Wizard + health verification --------------------------------------
BIN="$PREFIX/bin/telemeetbot"
if [[ "$WIZARD" -eq 1 ]]; then
  echo
  log "starting the setup wizard (Ctrl+C to abort). Config: ${CONFIG_DIR}/config.yml"
  $SUDO "$BIN" --wizard --config "${CONFIG_DIR}/config.yml"
  echo
  log "next steps"
  case "$PLATFORM" in
    linux)  echo "    sudo systemctl enable --now telemeetbot"; echo "    sudo journalctl -u telemeetbot -f" ;;
    darwin) echo "    launchctl bootstrap gui/\$(id -u) \$HOME/Library/LaunchAgents/telemeetbot.plist"; echo "    tail -f ${CONFIG_DIR}/data/*.log" ;;
  esac
else
  echo
  log "installed. Run the wizard:  ${SUDO:+$SUDO }$BIN --wizard --config ${CONFIG_DIR}/config.yml"
fi

if [[ "$START" -eq 1 && "$SERVICE" -eq 1 ]]; then
  log "verifying the health endpoint"
  # The wizard binds 127.0.0.1:18080 by default; give the service a moment.
  sleep 2
  if curl -fsS --max-time 5 "http://127.0.0.1:18080/livez" >/dev/null 2>&1; then
    log "✅ health check passed (http://127.0.0.1:18080/livez)"
  else
    warn "health endpoint not answering yet — check the logs (the service may still be starting)"
  fi
fi
