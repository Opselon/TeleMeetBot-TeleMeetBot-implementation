#!/usr/bin/env bash
# TeleMeetBot one-command installer.
#
#   curl -fsSL https://raw.githubusercontent.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/main/scripts/install.sh | sudo bash
#
# What it does:
#   1. detects architecture and downloads the matching release archive
#   2. verifies it against SHA256SUMS
#   3. installs /usr/local/bin/telemeetbot
#   4. installs the systemd unit (does not enable it yet)
#   5. runs the interactive setup wizard (unless --no-wizard)
#
# Flags: --version vX.Y.Z  --prefix DIR  --no-wizard  --no-service
set -euo pipefail

REPO="${REPO:-Opselon/TeleMeetBot-TeleMeetBot-implementation}"
VERSION="${VERSION:-latest}"
PREFIX="${PREFIX:-/usr/local}"
CONFIG_DIR="${CONFIG_DIR:-/etc/telemeetbot}"
WIZARD=1
SERVICE=1

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="${2:?}"; shift 2 ;;
    --prefix) PREFIX="${2:?}"; shift 2 ;;
    --config-dir) CONFIG_DIR="${2:?}"; shift 2 ;;
    --no-wizard) WIZARD=0; shift ;;
    --no-service) SERVICE=0; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[[ "$(uname -s)" == Linux ]] || die "TeleMeetBot targets Linux servers only"
command -v curl >/dev/null || die "curl is required"
command -v tar >/dev/null || die "tar is required"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l) ARCH=armv7 ;;
  armv6l) ARCH=armv6 ;;
  i386|i686) ARCH=386 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

SUDO=""
if [[ "$(id -u)" -ne 0 ]]; then
  command -v sudo >/dev/null || die "run as root or install sudo"
  SUDO="sudo"
fi

if [[ "$VERSION" == "latest" ]]; then
  log "resolving the latest release"
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
  [[ -n "$VERSION" ]] || die "could not resolve the latest release"
fi

FILE="telemeetbot-linux-${ARCH}.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "downloading TeleMeetBot ${VERSION} (${ARCH})"
curl -fL --retry 3 -o "$TMP/$FILE" "$BASE/$FILE"
curl -fL --retry 3 -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS"

log "verifying checksum"
( cd "$TMP" && grep " $FILE\$" SHA256SUMS | sha256sum -c - ) || die "checksum mismatch — aborting"

tar -C "$TMP" -xzf "$TMP/$FILE"
[[ -x "$TMP/telemeetbot" ]] || die "archive does not contain the binary"

log "installing binary to ${PREFIX}/bin/telemeetbot"
$SUDO install -d -m 0755 "$PREFIX/bin"
$SUDO install -m 0755 "$TMP/telemeetbot" "$PREFIX/bin/telemeetbot"

$SUDO install -d -m 0750 "$CONFIG_DIR"
if [[ -f "$TMP/deploy/telemeetbot.service" ]] && command -v systemctl >/dev/null; then
  if [[ "$SERVICE" -eq 1 ]]; then
    log "installing systemd unit"
    $SUDO install -m 0644 "$TMP/deploy/telemeetbot.service" /etc/systemd/system/telemeetbot.service
    $SUDO systemctl daemon-reload
  fi
fi

if ! command -v chromium >/dev/null && ! command -v chromium-browser >/dev/null \
   && ! command -v google-chrome >/dev/null && ! command -v google-chrome-stable >/dev/null; then
  log "no Chromium found — the setup wizard can download one for you"
fi

if [[ "$WIZARD" -eq 1 ]]; then
  echo
  log "starting the setup wizard (Ctrl+C to abort). Config: ${CONFIG_DIR}/config.yml"
  $SUDO "$PREFIX/bin/telemeetbot" --wizard --config "${CONFIG_DIR}/config.yml"
  echo
  log "next steps"
  echo "    sudo systemctl enable --now telemeetbot"
  echo "    sudo journalctl -u telemeetbot -f"
else
  echo
  log "installed. Run the wizard:  sudo telemeetbot --wizard --config ${CONFIG_DIR}/config.yml"
fi
