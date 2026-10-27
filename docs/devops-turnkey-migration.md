# TeleMeetBot — DevOps, Turnkey Deployment & Server Migration Guide

This guide details how to operate, deploy, and migrate **TeleMeetBot** with zero downtime or data loss across servers.

---

## 1. Quick Turnkey Deployment (New Server)

### Prerequisites
- Linux OS (Ubuntu 22.04 / 24.04, Debian 12, Alpine, Rocky Linux)
- Docker installed (`curl -fsSL https://get.docker.com | sh`)

### Run the One-Command Installer
```bash
curl -fsSL https://raw.githubusercontent.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/main/scripts/deploy.sh | sh
```

The deploy script automatically:
1. Validates Docker and host prerequisites.
2. Creates the `/root/telemeetbot-data` persistent volume with correct `10001:10001` permissions.
3. Generates a default `telemeetbot.yml` configuration (prompting for token if run interactively, or taking it from `TELEGRAM_TOKEN`).
4. Pulls the latest all-in-one container from GHCR.
5. Launches the container with 1 GiB shared memory (`--shm-size=1g`) for Chromium stability.
6. Verifies `/livez` health check and prints container status.

---

## 2. Zero-Loss Server Migration (Old Server ➔ New Server)

When replacing or upgrading your host machine, TeleMeetBot supports a seamless 2-step backup and restore process.

### Step 1: Create Backup on Old Server
Run the backup script:
```bash
./scripts/backup.sh /root/telemeetbot-data .
```
This generates a portable compressed archive: `telemeetbot-backup-YYYYMMDD_HHMMSS.tar.gz` containing:
- `telemeetbot.yml` (Bot credentials, admin IDs, settings)
- `playlists.db` (Saved user queues and tracks)
- `meetings.db` (Saved Google Meet links and nicknames)
- `user-profiles/` (Google OAuth 2.0 / OpenID Connect tokens)

### Step 2: Transfer and Restore on New Server
Copy the tarball to your new server (via `scp`, `rsync`, or secure download):
```bash
scp telemeetbot-backup-*.tar.gz root@new-server-ip:/root/
```

On the new server, run:
```bash
./scripts/deploy.sh --restore /root/telemeetbot-backup-YYYYMMDD_HHMMSS.tar.gz
```
The deploy script unpacks the snapshot, sets container permissions, launches the container, and verifies health. Your bot resumes immediately with all user sessions and tokens intact!

---

## 3. Configuration via 12-Factor Environment Variables

TeleMeetBot supports configuration via `.env` without needing to modify `telemeetbot.yml` directly:

| Variable | Description | Example |
|---|---|---|
| `TELEGRAM_TOKEN` | BotFather token | `123456789:ABCdef...` |
| `TELEGRAM_ADMINS` | Comma-separated admin Telegram IDs | `5094837833,987654321` |
| `TELEGRAM_NOTIFY_CHAT_ID` | Telegram chat ID for admin alerts | `5094837833` |
| `TELEGRAM_API_ID` | Telegram API ID for 2GB local server | `17349` |
| `TELEGRAM_API_HASH` | Telegram API Hash for 2GB local server | `344583e45741c457...` |
| `TELEMEETBOT_PROXY` | HTTP/SOCKS5 proxy for Bot & Meet | `http://10.0.0.1:8080` |
| `BROWSER_PROXY` | Proxy dedicated to Chromium browser | `socks5://127.0.0.1:1080` |
| `GOOGLE_OAUTH_CLIENT_ID` | Google Web Client ID for OAuth 2.0 | `xxx.apps.googleusercontent.com` |
| `GOOGLE_OAUTH_CLIENT_SECRET` | Google Client Secret | `GOCSPX-...` |
| `GOOGLE_OAUTH_REDIRECT_URL` | OAuth 2.0 callback URL | `https://googlemeet.example.com/auth/google/callback` |

---

## 4. Docker Compose Deployment

If you prefer `docker compose`:

```bash
git clone https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation
cd TeleMeetBot-TeleMeetBot-implementation

# Configure environment variables
cp .env.example .env
nano .env

# Start container in background
docker compose up -d

# Check logs and health
docker compose logs -f
curl http://127.0.0.1:18080/healthz
```

---

## 5. Nginx Reverse Proxy with HTTPS

To expose Google OAuth 2.0 callbacks and health probes securely over the internet:

1. Copy the template from `deploy/nginx-googlemeet.conf`:
   ```bash
   cp deploy/nginx-googlemeet.conf /etc/nginx/sites-available/googlemeet.conf
   ln -s /etc/nginx/sites-available/googlemeet.conf /etc/nginx/sites-enabled/
   ```
2. Replace `googlemeet.example.com` with your real domain.
3. Obtain a free SSL certificate with Certbot:
   ```bash
   certbot --nginx -d googlemeet.example.com
   ```
4. Reload Nginx: `nginx -t && systemctl reload nginx`.

---

## 6. CI/CD Architecture (GitHub Actions)

- **PR Verification (`ci.yml`)**: Every pull request undergoes formatting, linting, cross-OS compilation (Linux, macOS, Windows; AMD64 & ARM64), unit tests, docs synchronization, and Docker container build tests.
- **Continuous Deployment (GHCR)**: When code merges into `main`, GitHub Actions automatically builds the multi-arch all-in-one Docker image and publishes it to `ghcr.io/opselon/telemeetbot-telemeetbot-implementation:latest`.

---

⭐ **Star on GitHub**: If this project is helpful, please support it by starring [Opselon/TeleMeetBot-TeleMeetBot-implementation](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation)!
