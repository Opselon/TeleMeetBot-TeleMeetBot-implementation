#!/bin/sh
# TeleMeetBot — One-Command Turnkey Deployment & Migration Script
# Supports: fresh server install, container update, and zero-downtime migration.
# POSIX sh compatible (works with dash, ash, bash, zsh).
set -eu

REPO_URL="https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation"
DEFAULT_IMAGE="ghcr.io/opselon/telemeetbot-telemeetbot-implementation:latest"
DEFAULT_DATA_DIR="/root/telemeetbot-data"
CONTAINER_NAME="telemeetbot"
HEALTH_PORT="18080"
IMAGE="${TELEMEETBOT_IMAGE:-$DEFAULT_IMAGE}"
DATA_DIR="${TELEMEETBOT_DATA_DIR:-$DEFAULT_DATA_DIR}"
RESTORE_FILE=""
PULL_IMAGE=1
BUILD_LOCAL=0

print_banner() {
  cat << 'EOF'
  _____     _       __  __           _   ____        _   
 |_   _|___| | ___ |  \/  | ___  ___| |_| __ )  ___ | |_ 
   | | / _ \ |/ _ \| |\/| |/ _ \/ _ \ __|  _ \ / _ \| __|
   | ||  __/ |  __/| |  | |  __/  __/ |_| |_) | (_) | |_ 
   |_| \___|_|\___||_|  |_|\___|\___|\__|____/ \___/ \__|
                                                         
  Headless Google Meet Streaming Bot (Go + Chromium)
  All-in-One Docker · 2.0 GiB Media Support · OAuth 2.0
EOF
}

usage() {
  cat << EOF
Usage: $0 [OPTIONS]

Options:
  --data-dir <path>     Path to host data volume (default: $DEFAULT_DATA_DIR)
  --image <tag>         Docker image tag (default: $DEFAULT_IMAGE)
  --port <port>         Host health port (default: $HEALTH_PORT)
  --restore <file>      Restore configuration & data from backup tar.gz before launch
  --no-pull             Skip pulling the latest image from registry
  --build               Build docker image locally from current directory
  --help                Show this help message

Environment variables supported:
  TELEGRAM_TOKEN, TELEGRAM_ADMINS, TELEGRAM_API_ID, TELEGRAM_API_HASH,
  TELEMEETBOT_IMAGE, TELEMEETBOT_DATA_DIR, TELEMEETBOT_PROXY

EOF
  exit 0
}

while [ $# -gt 0 ]; do
  case "$1" in
    --data-dir)
      DATA_DIR="$2"
      shift 2
      ;;
    --image)
      IMAGE="$2"
      shift 2
      ;;
    --port)
      HEALTH_PORT="$2"
      shift 2
      ;;
    --restore)
      RESTORE_FILE="$2"
      shift 2
      ;;
    --no-pull)
      PULL_IMAGE=0
      shift
      ;;
    --build)
      BUILD_LOCAL=1
      PULL_IMAGE=0
      shift
      ;;
    --help|-h)
      usage
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage
      ;;
  esac
done

print_banner

# 1. Check prerequisites
if ! command -v docker >/dev/null 2>&1; then
  echo "❌ Error: Docker is not installed on this system." >&2
  echo "Install docker with: curl -fsSL https://get.docker.com | sh" >&2
  exit 1
fi

echo "==> Preparing data directory at: $DATA_DIR"
mkdir -p "$DATA_DIR"
mkdir -p "$DATA_DIR/downloads"
mkdir -p "$DATA_DIR/telegram-bot-api"
mkdir -p "$DATA_DIR/telegram-bot-api-temp"
mkdir -p "$DATA_DIR/browser-cache"
mkdir -p "$DATA_DIR/user-profiles"

# 2. Restore backup if requested
if [ -n "$RESTORE_FILE" ]; then
  if [ ! -f "$RESTORE_FILE" ]; then
    echo "❌ Error: Backup file $RESTORE_FILE does not exist." >&2
    exit 1
  fi
  echo "==> Restoring migration archive $RESTORE_FILE into $DATA_DIR..."
  tar -xzf "$RESTORE_FILE" -C "$DATA_DIR"
  echo "✅ Backup archive restored."
fi

# Ensure user 10001 (telemeet inside container) owns the data directory
if command -v chown >/dev/null 2>&1; then
  chown -R 10001:10001 "$DATA_DIR" 2>/dev/null || true
fi

# 3. Create default telemeetbot.yml if missing
CONFIG_FILE="$DATA_DIR/telemeetbot.yml"
if [ ! -f "$CONFIG_FILE" ]; then
  echo "==> Initializing default config at $CONFIG_FILE"
  token="${TELEGRAM_TOKEN:-}"
  if [ -z "$token" ] && [ -t 0 ]; then
    printf "Enter your Telegram Bot Token from @BotFather: "
    read -r token
  fi
  if [ -z "$token" ]; then
    token="PASTE_YOUR_BOTFATHER_TOKEN_HERE"
  fi

  admin_id="${TELEGRAM_ADMINS:-}"
  api_id="${TELEGRAM_API_ID:-17349}"
  api_hash="${TELEGRAM_API_HASH:-344583e45741c457fe1862106095a5eb}"

  cat << EOF > "$CONFIG_FILE"
telegram:
  token: "$token"
  admins: [${admin_id}]
  first_start_claims_admin: true
  notify_chat_id: 0
  poll_timeout_seconds: 30
  max_file_size_mb: 2000
  local_bot_api:
    enabled: true
    api_id: ${api_id}
    api_hash: "${api_hash}"
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
  proxy: "${TELEMEETBOT_PROXY:-}"

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
EOF
  chmod 0600 "$CONFIG_FILE"
  chown 10001:10001 "$CONFIG_FILE" 2>/dev/null || true
  echo "✅ Config generated at $CONFIG_FILE"
fi

# 4. Pull or build image
if [ "$BUILD_LOCAL" -eq 1 ]; then
  echo "==> Building Docker image locally..."
  docker build -t "$IMAGE" .
elif [ "$PULL_IMAGE" -eq 1 ]; then
  echo "==> Pulling Docker image $IMAGE..."
  docker pull "$IMAGE" || echo "⚠️ Warning: docker pull failed, attempting to run with local cached image."
fi

# 5. Stop and replace existing container
if docker ps -a --format '{{.Names}}' | grep -Eq "^${CONTAINER_NAME}\$"; then
  echo "==> Replacing existing $CONTAINER_NAME container..."
  docker stop "$CONTAINER_NAME" >/dev/null 2>&1 || true
  docker rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
fi

# 6. Launch container
echo "==> Launching $CONTAINER_NAME container..."
docker run -d \
  --name "$CONTAINER_NAME" \
  --restart unless-stopped \
  --shm-size 1g \
  -p "127.0.0.1:${HEALTH_PORT}:18080" \
  -v "${DATA_DIR}:/data" \
  "$IMAGE" >/dev/null

# 7. Healthcheck verification loop
echo "==> Waiting for service to become healthy..."
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
  echo "🎉 TeleMeetBot successfully deployed and healthy!"
  echo "================================================================================"
  echo "• Container:       $CONTAINER_NAME"
  echo "• Image:           $IMAGE"
  echo "• Data Directory:  $DATA_DIR"
  echo "• Health Endpoint: http://127.0.0.1:${HEALTH_PORT}/healthz"
  echo "• Logs:            docker logs -f $CONTAINER_NAME"
  echo ""
  echo "⭐ If you enjoy TeleMeetBot, please star us on GitHub:"
  echo "   $REPO_URL"
  echo "================================================================================"
else
  echo "⚠️ Warning: Healthcheck timed out after ${max_attempts}s."
  echo "Inspect container logs with: docker logs --tail 50 $CONTAINER_NAME"
  exit 1
fi
