#!/bin/sh
# Egress benchmark: 10x10MiB Cloudflare (direct), 3x20MiB Tor, YouTube sanity checks.
# Runs inside the production container via docker exec. Cleans temp files after each run.
# POSIX sh — compatible with dash / shellcheck -s sh.

set -e

CONTAINER="telemeetbot"
CTMP="/tmp/bench-$$"  # container temp path

run() {
    echo "$@"
    docker exec "$CONTAINER" sh -c "$@"
}

cleanup() {
    run "rm -rf '$CTMP' 2>/dev/null || true"
}
trap cleanup EXIT INT TERM

# ensure container temp exists
run "mkdir -p '$CTMP'"

echo "=== Egress Benchmark $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
echo "Container: $CONTAINER"
echo "Temp dir:  $CTMP (inside container)"
echo ""

# 1. Direct raw: 10x10MiB Cloudflare
echo "--- Direct (10 x 10 MiB) ---"
i=1
while [ $i -le 10 ]; do
    out=$(run "curl -s -m 30 -o /dev/null -w 'speed=%{speed_download} ttfb=%{time_starttransfer} code=%{http_code}' 'https://speed.cloudflare.com/__down?bytes=10485760'")
    speed=$(echo "$out" | sed 's/.*speed=\([0-9]*\).*/\1/')
    ttfb=$(echo "$out" | sed 's/.*ttfb=\([0-9.]*\).*/\1/')
    code=$(echo "$out" | sed 's/.*code=\([0-9]*\).*/\1/')
    mibs=$(echo "$speed" | awk '{printf "%.2f", $1/1048576}')
    echo "  run$i: ${mibs} MiB/s  TTFB=${ttfb}s  code=$code"
    i=$((i+1))
done

# 2. Tor raw: 3x20MiB Cloudflare
echo "--- Tor (3 x 20 MiB) ---"
i=1
while [ $i -le 3 ]; do
    out=$(run "curl -s -m 60 -o /dev/null -w 'speed=%{speed_download} ttfb=%{time_starttransfer} code=%{http_code}' 'https://speed.cloudflare.com/__down?bytes=20971520' --socks5-hostname 172.17.0.1:9050")
    speed=$(echo "$out" | sed 's/.*speed=\([0-9]*\).*/\1/')
    ttfb=$(echo "$out" | sed 's/.*ttfb=\([0-9.]*\).*/\1/')
    code=$(echo "$out" | sed 's/.*code=\([0-9]*\).*/\1/')
    mibs=$(echo "$speed" | awk '{printf "%.2f", $1/1048576}')
    echo "  run$i: ${mibs} MiB/s  TTFB=${ttfb}s  code=$code"
    i=$((i+1))
done

# 3. Direct YouTube sanity (dQw - known good)
echo "--- YouTube (direct, dQw4w9WgXcQ, 720p cap) ---"
run "cd '$CTMP' && rm -f yt_direct.mp4* && yt-dlp --no-config --extractor-args 'youtube:player_client=tv_samsung,visionos' -f 'b[ext=mp4][height<=720]/b' --max-filesize 100M -o 'yt_direct.mp4' 'https://www.youtube.com/watch?v=dQw4w9WgXcQ' 2>&1 | tail -1"
run "ls -la '$CTMP'/yt_direct.mp4 2>/dev/null && rm -f '$CTMP'/yt_direct.mp4*"

# 4. YouTube Tor fallback (ZKmTRKD4SiY - known bot-checked)
echo "--- YouTube (Tor, ZKmTRKD4SiY, 480p cap) ---"
run "cd '$CTMP' && rm -f yt_tor.mp4* && timeout 240 yt-dlp --no-config --proxy 'socks5://172.17.0.1:9050' --extractor-args 'youtube:player_client=tv_samsung,visionos' -f 'b[ext=mp4][height<=480]/b' -o 'yt_tor.mp4' 'https://www.youtube.com/watch?v=ZKmTRKD4SiY' 2>&1 | tail -1"
run "ls -la '$CTMP'/yt_tor.mp4 2>/dev/null && rm -f '$CTMP'/yt_tor.mp4*"

# 5. Direct upload: 3x10MiB
echo "--- Upload (direct, 3 x 10 MiB) ---"
run "head -c 10485760 /dev/urandom > '$CTMP'/up.bin"
i=1
while [ $i -le 3 ]; do
    out=$(run "curl -s -m 60 -X POST -F 'file=@$CTMP/up.bin' -o /dev/null -w 'speed=%{speed_upload} code=%{http_code}' 'https://speed.cloudflare.com/__up'")
    speed=$(echo "$out" | sed 's/.*speed=\([0-9]*\).*/\1/')
    code=$(echo "$out" | sed 's/.*code=\([0-9]*\).*/\1/')
    mibs=$(echo "$speed" | awk '{printf "%.2f", $1/1048576}')
    echo "  run$i: ${mibs} MiB/s  code=$code"
    i=$((i+1))
done
run "rm -f '$CTMP'/up.bin"

echo ""
echo "=== Benchmark complete ==="