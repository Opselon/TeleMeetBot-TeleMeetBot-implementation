#!/bin/sh
# TeleMeetBot — Zero-Loss Migration Restore Script
# Restores configuration, databases, and OAuth credentials from a tar.gz snapshot.
# POSIX sh compatible.
set -eu

if [ $# -lt 1 ]; then
  echo "Usage: $0 <backup-file.tar.gz> [data_dir]" >&2
  exit 1
fi

BACKUP_FILE="$1"
DATA_DIR="${2:-/root/telemeetbot-data}"

if [ ! -f "$BACKUP_FILE" ]; then
  echo "❌ Error: Backup file $BACKUP_FILE does not exist." >&2
  exit 1
fi

echo "==> Restoring $BACKUP_FILE into $DATA_DIR..."
mkdir -p "$DATA_DIR"
tar -xzf "$BACKUP_FILE" -C "$DATA_DIR"

if command -v chown >/dev/null 2>&1; then
  chown -R 10001:10001 "$DATA_DIR" 2>/dev/null || true
fi

echo "✅ Backup successfully unpacked into $DATA_DIR"

if command -v docker >/dev/null 2>&1; then
  if docker ps -a --format '{{.Names}}' | grep -Eq "^telemeetbot\$"; then
    echo "==> Restarting telemeetbot container..."
    docker restart telemeetbot >/dev/null
    echo "✅ Container restarted."
  fi
fi
