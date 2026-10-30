# Web Dashboard

A light, dependency-free web dashboard that mirrors the Telegram control
surface. Login is Telegram's official **Login Widget** — there is no password
anywhere — and everything the bot can do from Telegram, an authenticated admin
can do from the browser.

## Enabling

```yaml
dashboard:
  enabled: true
  addr: ":8080"
  public_base_url: "https://bot.example.com"   # optional, hint only
  session_hours: 24                            # login cookie lifetime
```

Restart the bot; it logs `web dashboard enabled at http://:8080`.

## Login with Telegram

1. **Register the domain with @BotFather** — `/setdomain`, pick your bot,
   add the domain the dashboard is served from. Telegram refuses to render
   the widget on unregistered domains; this is a Telegram-side requirement.
2. Open the dashboard. The login page shows the official widget ("Log in
   with Telegram").
3. Telegram authorizes you and hands the page a signed payload
   (`id`, `username`, `auth_date`, `hash`, …). The dashboard verifies it:

   - `hash` must equal `HMAC-SHA256(data_check_string, SHA256(bot_token))`
     where `data_check_string` is every received field except `hash`,
     sorted alphabetically, joined with `\n` — exactly the algorithm
     Telegram documents;
   - payloads older than 24 hours are rejected (replay protection);
   - the verified Telegram id **must also be on the bot's admin allow-list**
     (`admins` in the config / claimed via `/start`). Fail-closed: a valid
     Telegram identity that is not an admin gets `403`.

Sessions are HMAC-signed cookies (`HttpOnly`, `SameSite=Strict`) keyed by a
random secret persisted at `<data_dir>/dashboard-secret.key`. Regenerating or
deleting that file logs everyone out. Failed logins are rate-limited to
5 attempts per IP per 15 minutes.

## What you can do

Everything the Telegram dashboard offers, plus the same live-log view the
bot's log capture records:

| Area | Capability |
| --- | --- |
| Overview | version, uptime, active/max sessions, disk, log-capture stats |
| Sessions | start (Meet link + media link + mode), stop, pause/resume, mute, volume, speed, restart, next/previous, live pipeline timeline (`download 8.2s · join 12.4s*`) |
| Download | stage + percent + ETA on the session card while a Download & Stream fetch runs |
| Users | what every user is doing right now (wizard state, staging, live) |
| Logs | live SSE tail of the central ring buffer (last 2000 entries), error/warning counters |

## HTTP API

All under the dashboard origin; everything except the auth call requires the
session cookie.

```
POST /api/auth/telegram        verify Login Widget payload (JSON or query), set cookie
POST /api/auth/logout          clear the cookie
GET  /api/state                health + sessions + users + log stats (poll every 3s)
GET  /api/logs?n=&level=&component=   recent entries (level: 0 debug … 3 error)
GET  /api/logs/stream          SSE tail of new entries
POST /api/session/start        {"meet_url","media_url","mode","queue","volume","speed"}
POST /api/session/stop?id=     stop one session
POST /api/session/action?id=   {"action","arg"} — pause, resume, mute, unmute,
                               volume (0-100), speed (0.25-4), seek_back_10,
                               seek_fwd_10, seek_back_30, seek_fwd_30,
                               restart, next, previous
```

The action set is exactly the Telegram card's button set — the dashboard
never widens the control surface beyond what the bot exposes.

## Security notes

- **No control without login**: every `/api` route 401s without a valid
  cookie, and re-checks the admin allow-list on every request (an admin
  removed from the config loses access immediately).
- **No secrets in pages**: the bot token never appears in the UI; it is only
  used server-side for signature verification.
- **Same hardening headers** as the health pages: CSP, `X-Frame-Options:
  DENY`, `nosniff`, same-origin referrer policy.
- **Reverse proxy tip**: if you expose the dashboard publicly, put TLS in
  front (nginx/Caddy) and keep `addr` bound to localhost or a private
  interface.

## Troubleshooting

- **Widget doesn't render** — the domain isn't registered with @BotFather
  (`/setdomain`), or the page isn't served over the registered domain.
- **`login failed: signature mismatch`** — the payload was tampered with or
  the dashboard's `bot_token` differs from the widget's bot.
- **`403 this Telegram account is not an administrator`** — add the account
  to `admins:` in the config, or have it `/start` the bot to claim the first
  admin slot.
