# Embedded Local Telegram Bot API Server (2.0 GiB Support)

TeleMeetBot supports handling Telegram video and audio files up to **2000 MiB (2.0 GiB)** (the maximum Telegram limit) directly within an **All-in-One Docker container** by embedding a supervised local `telegram-bot-api` server.

---

## Background & Why Local Bot API?

The official public Telegram Bot API server (`api.telegram.org`) restricts bot file downloads to **20.0 MiB**. For streaming media (presentations, recorded webinars, large video files) in Google Meet, media must be downloaded to the bot host so Chromium can play it.

When running with an embedded local Bot API server in `--local` mode:
1. File download limit increases from **20 MiB to 2000 MiB (2.0 GiB)**.
2. The local server downloads files directly to the host filesystem.
3. TeleMeetBot uses **Local Fast-Path Direct Copy** (`io.Copy`), eliminating network roundtrips and streaming high-bitrate media with zero latency.

---

## Configuration

### 1. Environment Variables (Recommended)

Set the following environment variables in your Docker run command or `.env`:

```bash
# Telegram application credentials from https://my.telegram.org
TELEGRAM_API_ID=17349
TELEGRAM_API_HASH=your_api_hash_here

# Enable embedded local Bot API supervisor (default: true if API ID/Hash present)
TELEGRAM_LOCAL_BOT_API_ENABLED=true
```

### 2. YAML Configuration (`telemeetbot.yml`)

You can also configure the embedded local Bot API server in `telemeetbot.yml`:

```yaml
telegram:
  token: "123456789:ABCdefGHIjklMNOpqrSTUvwxYZ"
  # Max file size automatically raises to 2000 MiB when local_bot_api is enabled
  max_file_size_mb: 2000
  local_bot_api:
    enabled: true
    api_id: 17349
    api_hash: "your_api_hash_here"
    http_port: 8081
    dir: "/data/telegram-bot-api"
    temp_dir: "/data/telegram-bot-api-temp"
```

---

## Docker Deployment (All-in-One)

The official Docker image packages both TeleMeetBot and `telegram-bot-api` in a single image. No external supervisor or extra container is required.

```bash
docker run -d \
  --name telemeetbot \
  --restart unless-stopped \
  -p 18080:18080 \
  -v /root/telemeetbot-data:/data \
  -e TELEGRAM_API_ID="17349" \
  -e TELEGRAM_API_HASH="your_hash" \
  telemeetbot:latest
```

---

## Verification

When enabled, TeleMeetBot will log:
```text
level=INFO msg="[PIPELINE:localbotapi] starting local telegram-bot-api on port 8081 (dir=/data/telegram-bot-api)"
level=INFO msg="[PIPELINE:localbotapi] local bot API server healthy on 127.0.0.1:8081 (max file size up to 2000 MiB)"
```
And files sent to Telegram up to 2.0 GiB will be ingested and streamed into Google Meet.
