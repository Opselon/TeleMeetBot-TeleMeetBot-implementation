#!/bin/sh
# TeleMeetBot — Remote CD Deployment Runner
# Invoked by GitHub Actions CD via SSH or executed directly on server.
# Stops old container cleanly, prepares configs/secrets, launches fresh container, and verifies health.
# POSIX sh compatible.
set -eu

DATA_DIR="${TELEMEETBOT_DATA_DIR:-/root/telemeetbot-data}"
CONTAINER_NAME="telemeetbot"
HEALTH_PORT="18080"
IMAGE="${TELEMEETBOT_IMAGE:-ghcr.io/opselon/telemeetbot-telemeetbot-implementation:latest}"

echo "================================================================================"
echo "🚀 Starting TeleMeetBot Continuous Deployment..."
echo "================================================================================"

# 1. Verify Docker availability
if ! command -v docker >/dev/null 2>&1; then
  echo "❌ Error: Docker is not installed on this system." >&2
  exit 1
fi

# 2. Ensure data volumes exist with correct ownership (telemeet: 10001)
echo "==> Preparing data volumes at $DATA_DIR..."
mkdir -p "$DATA_DIR" \
         "$DATA_DIR/downloads" \
         "$DATA_DIR/telegram-bot-api" \
         "$DATA_DIR/telegram-bot-api-temp" \
         "$DATA_DIR/browser-cache" \
         "$DATA_DIR/user-profiles"

CONFIG_FILE="$DATA_DIR/telemeetbot.yml"

# 3. Create default template if telemeetbot.yml does not exist
if [ ! -f "$CONFIG_FILE" ]; then
  echo "==> Creating default template at $CONFIG_FILE..."
  cat << 'EOF' > "$CONFIG_FILE"
telegram:
  token: ""
  admins: []
  first_start_claims_admin: true
  notify_chat_id: 0
  poll_timeout_seconds: 30
  max_file_size_mb: 2000
  local_bot_api:
    enabled: true
    api_id: 17349
    api_hash: "344583e45741c457fe1862106095a5eb"
    http_port: 8081
    dir: "/data/telegram-bot-api"
    temp_dir: "/data/telegram-bot-api-temp"

server:
  health_addr: "0.0.0.0:18080"

browser:
  executable: ""
  headless: true
  per_session: true
  window_width: 1920
  window_height: 1080
  lang: "en-US"
  gpu: "compatible"
  proxy: ""

media:
  default_volume: 80
  default_speed: 1.0
  autoplay: true
  mute_until_connected: true
  seek_step_seconds: 10
  loop: false

meet:
  guest_name: "TeleMeet Bot"
  present_mode: "tab"
  capture_source: "TeleMeet Media"
  join_timeout_seconds: 120
  present_timeout_seconds: 45
  admit_wait_seconds: 300

limits:
  max_sessions: 2

logging:
  level: "info"
  file: ""

paths:
  data_dir: "/data"

google:
  enabled: false
  client_id: ""
  client_secret: ""
  redirect_url: ""
EOF
fi

# 4. Safely update configuration fields with injected environment secrets
if command -v python3 >/dev/null 2>&1; then
  python3 - << 'PYEOF'
import os, re

cfg_path = os.environ.get("CONFIG_FILE", "/root/telemeetbot-data/telemeetbot.yml")
if not os.path.exists(cfg_path):
    exit(0)

with open(cfg_path, "r", encoding="utf-8") as f:
    text = f.read()

token = os.environ.get("TELEGRAM_TOKEN", "").strip()
if token:
    text = re.sub(r'(token:\s*")[^"]*(")', rf'\g<1>{token}\g<2>', text)

admins = os.environ.get("TELEGRAM_ADMINS", "").strip()
if admins:
    text = re.sub(r'admins:\s*\[[^\]]*\]', f'admins: [{admins}]', text)
    first_admin = admins.split(",")[0].strip()
    if first_admin.isdigit():
        text = re.sub(r'notify_chat_id:\s*\d+', f'notify_chat_id: {first_admin}', text)

api_id = os.environ.get("TELEGRAM_API_ID", "").strip()
if api_id and api_id.isdigit():
    text = re.sub(r'api_id:\s*\d+', f'api_id: {api_id}', text)

api_hash = os.environ.get("TELEGRAM_API_HASH", "").strip()
if api_hash:
    text = re.sub(r'(api_hash:\s*")[^"]*(")', rf'\g<1>{api_hash}\g<2>', text)

proxy = os.environ.get("TELEMEETBOT_PROXY", "").strip()
if proxy:
    text = re.sub(r'(proxy:\s*")[^"]*(")', rf'\g<1>{proxy}\g<2>', text)

cid = os.environ.get("GOOGLE_OAUTH_CLIENT_ID", "").strip()
csec = os.environ.get("GOOGLE_OAUTH_CLIENT_SECRET", "").strip()
rurl = os.environ.get("GOOGLE_OAUTH_REDIRECT_URL", "").strip()

if cid and csec:
    text = re.sub(r'google:\s*\n\s*enabled:\s*false', 'google:\n  enabled: true', text)
    text = re.sub(r'(client_id:\s*")[^"]*(")', rf'\g<1>{cid}\g<2>', text)
    text = re.sub(r'(client_secret:\s*")[^"]*(")', rf'\g<1>{csec}\g<2>', text)
    if rurl:
        text = re.sub(r'(redirect_url:\s*")[^"]*(")', rf'\g<1>{rurl}\g<2>', text)

with open(cfg_path, "w", encoding="utf-8") as f:
    f.write(text)
PYEOF
fi

chmod 0600 "$CONFIG_FILE"
if command -v chown >/dev/null 2>&1; then
  chown -R 10001:10001 "$DATA_DIR" 2>/dev/null || true
fi

# 5. Pull latest container image from GHCR.
# A failed pull MUST fail the deploy: proceeding with the cached image
# silently redeployed an old build once (2026-10-27: the :latest tag raced
# the CI push and production ran a stale image for a day while CD reported
# success). Bounded retries ride out transient registry/CDN errors.
echo "==> Pulling latest Docker image: $IMAGE..."
pull_ok=0
pull_attempt=0
max_pull_attempts=3
while [ $pull_attempt -lt $max_pull_attempts ]; do
  pull_attempt=$((pull_attempt + 1))
  if docker pull "$IMAGE"; then
    pull_ok=1
    break
  fi
  echo "Warning: docker pull attempt $pull_attempt/$max_pull_attempts failed."
  [ $pull_attempt -lt $max_pull_attempts ] && sleep 10
done
if [ $pull_ok -ne 1 ]; then
  echo "Error: docker pull failed after $max_pull_attempts attempts." >&2
  echo "Error: refusing to redeploy the cached image - that would silently roll back production." >&2
  exit 1
fi
PULLED_DIGEST=$(docker inspect --format '{{index .RepoDigests 0}}' "$IMAGE" 2>/dev/null | sed 's/.*@//' || true)
echo "==> Pulled image digest: ${PULLED_DIGEST:-unknown}"

# 6. Clean shutdown and removal of previous container
echo "==> Gracefully stopping previous $CONTAINER_NAME container..."
docker stop "$CONTAINER_NAME" >/dev/null 2>&1 || true
docker rm "$CONTAINER_NAME" >/dev/null 2>&1 || true

# 7. Start fresh container
echo "==> Launching fresh $CONTAINER_NAME container..."
docker run -d   --name "$CONTAINER_NAME"   --restart unless-stopped   --shm-size 1g   -p "127.0.0.1:${HEALTH_PORT}:18080"   -v "${DATA_DIR}:/data"   "$IMAGE" >/dev/null

# 7b. Verify the running container actually uses the freshly pulled image.
# The 2026-10-27 incident: health passed while the container silently ran
# the previous image (race between CI's image push and this script's pull).
RUNNING_IMAGE=$(docker inspect --format '{{.Image}}' "$CONTAINER_NAME" 2>/dev/null | sed 's/^sha256://' | cut -c1-12)
LOCAL_IMAGE_ID=$(docker inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null | sed 's/^sha256://' | cut -c1-12)
if [ -n "$LOCAL_IMAGE_ID" ] && [ "$RUNNING_IMAGE" != "$LOCAL_IMAGE_ID" ]; then
  echo "Error: running container image ($RUNNING_IMAGE) does not match the pulled image ($LOCAL_IMAGE_ID)." >&2
  echo "Error: failing the deployment instead of leaving a stale binary in production." >&2
  exit 1
fi

# 8. Health check verification loop
echo "==> Waiting for container to become healthy..."
attempts=0
max_attempts=30
healthy=0

while [ $attempts -lt $max_attempts ]; do
  if command -v curl >/dev/null 2>&1; then
    if curl -fsS "http://127.0.0.1:${HEALTH_PORT}/livez" >/dev/null 2>&1; then
      healthy=1
      break
    fi
  elif docker exec "$CONTAINER_NAME" curl -fsS http://127.0.0.1:18080/livez >/dev/null 2>&1; then
    healthy=1
    break
  fi
  sleep 1
  attempts=$((attempts + 1))
  printf "."
done
printf "\n"

if [ $healthy -eq 1 ]; then
  echo "================================================================================"
  echo "🎉 TeleMeetBot CD Deployment Succeeded!"
  echo "================================================================================"
  echo "• Container:       $CONTAINER_NAME (Up & Healthy)"
  echo "• Image:           $IMAGE"
  echo "• Health Endpoint: http://127.0.0.1:${HEALTH_PORT}/healthz"
  echo "• Live Probe:      http://127.0.0.1:${HEALTH_PORT}/livez"
  echo "================================================================================"
  # Prune dangling layers
  docker image prune -f >/dev/null 2>&1 || true
else
  echo "❌ Error: Health probe timed out after ${max_attempts}s." >&2
  echo "Recent container logs:" >&2
  docker logs --tail 40 "$CONTAINER_NAME" >&2 || true
  exit 1
fi
