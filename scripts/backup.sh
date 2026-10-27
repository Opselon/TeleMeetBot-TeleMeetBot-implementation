#!/bin/sh
# TeleMeetBot — Zero-Loss Migration & Backup Script
# Creates a clean, portable tar.gz snapshot of configuration, databases, and OAuth profiles.
# POSIX sh compatible.
set -eu

DATA_DIR="${1:-/root/telemeetbot-data}"
DEST_DIR="${2:-.}"

if [ ! -d "$DATA_DIR" ]; then
  echo "❌ Error: Data directory $DATA_DIR does not exist." >&2
  exit 1
fi

TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
OUT_FILE="${DEST_DIR}/telemeetbot-backup-${TIMESTAMP}.tar.gz"

echo "==> Creating backup of TeleMeetBot data from $DATA_DIR..."
TMP_STAGE="$(mktemp -d)"
trap 'rm -rf "$TMP_STAGE"' EXIT

# Copy critical configuration and databases
[ -f "$DATA_DIR/telemeetbot.yml" ] && cp "$DATA_DIR/telemeetbot.yml" "$TMP_STAGE/"
[ -f "$DATA_DIR/playlists.db" ] && cp "$DATA_DIR/playlists.db" "$TMP_STAGE/"
[ -f "$DATA_DIR/meetings.db" ] && cp "$DATA_DIR/meetings.db" "$TMP_STAGE/"
if [ -d "$DATA_DIR/user-profiles" ]; then
  cp -r "$DATA_DIR/user-profiles" "$TMP_STAGE/"
fi

# Package into compressed tarball
tar -C "$TMP_STAGE" -czf "$OUT_FILE" .

SIZE="$(du -h "$OUT_FILE" | cut -f1)"
echo "✅ Backup created successfully: $OUT_FILE ($SIZE)"
if command -v sha256sum >/dev/null 2>&1; then
  echo "• SHA256: $(sha256sum "$OUT_FILE" | cut -d' ' -f1)"
fi
echo ""
echo "To restore on a new server:"
echo "  ./scripts/deploy.sh --restore $OUT_FILE"
