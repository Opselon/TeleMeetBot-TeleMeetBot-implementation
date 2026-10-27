# TeleMeetBot — ربات استریم گوگل میت با داشبورد تلگرام (۱۰۰٪ Go)

[![GitHub Stars](https://img.shields.io/github/stars/Opselon/TeleMeetBot-TeleMeetBot-implementation?style=social)](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation)
[![CI Pipeline](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/actions/workflows/ci.yml/badge.svg)](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/actions/workflows/ci.yml)
[![Docker All-in-One](https://img.shields.io/badge/docker-all--in--one-blue?logo=docker)](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/pkgs/container/telemeetbot-telemeetbot-implementation)

یک باینری، بدون دسکتاپ، بدون Python/Selenium. لینک جلسه و لینک محتوا را می‌دهید؛ ربات
Chromium را headless بالا می‌آورد، به جلسه‌ی گوگل میت **به‌عنوان مهمان** می‌رود، شروع به
ارائه (Present) می‌کند، تبی که محتوا در آن پخش می‌شود را شِیر می‌کند و کنترل کامل پخش —
**Pause/Resume، Seek، Volume، Speed، Mute، Restart، Stop** — را مستقیم در تلگرام می‌دهد.

> ⭐ **حمایت از پروژه:** اگر از تله‌میت‌بات استفاده می‌کنید، با [استار دادن به مخزن گیت‌هاب](https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation) از توسعه و بهبود پروژه حمایت کنید!

```
              ┌───────────────────────────── Telegram ─────────────────────────────┐
              │  /start ⟶ داشبورد زنده · کارت‌ها هر چند ثانیه رفرش می‌شوند          │
              │  /new    ⟶ ویزارد دومرحله‌ای (لینک Meet → لینک مدیا)                │
              │  دکمه‌های Inline: ⏸ ▶️ 🔇 🔉 🔊 ⏪ ⏩ 🔁 🛑                     │
              └──────────────┬─────────────────────────────────────────────────────┘
                             │ long poll (لیست ادمین fail-closed)
                             ▼
           ┌─────────────────────────┐      ┌───────────────────────────────────┐
           │   Engine Manager        │      │ Chromium (headless, rod/CDP)      │
           │ state machine           ├─────►│ تب ۱: پلیر لینک مدیا              │
           │ API کنترل پخش           │      │ تب ۲: meet.google.com             │
           │ store + نوتیفیکشن       │      │  └─ Present now → tab capture     │
           └─────────────────────────┘      └───────────────────────────────────┘
                             │
                   /healthz · /livez (فقط محلی)
```

## چطور سمت Meet کار می‌کند؟

ربات به‌ازای هر session یک Chromium با فلگ‌های capture
(`--enable-usermedia-screen-capturing`,
`--auto-select-desktop-capture-source=…`,
`--autoplay-policy=no-user-gesture-required`) و **UA استوک Chrome** لانچ می‌کند (Meet
UA هدلس را می‌پسندد؟ نپسندید → ما آن را override می‌کنیم). دو تب باز می‌شود: تب مدیا با
عنوان قفل‌شده `TeleMeet Media` و تب Meet که از طریق UI واقعی هدایت می‌شود — پرکردن
اسم مهمان، Join now / Ask to join، بستن پاپ‌آپ‌ها — سپس «Present now → A tab» که به
طور خودکار همان تب مدیا را انتخاب می‌کند؛ همه‌چیزِ آن تبی که پخش می‌شود برای اعضای
جلسه دیدنی و شنیدنی است.

اگر به‌جای tab می‌خواهید **کل صفحه (Xvfb)** شیر شود: `present_mode: "screen"`,
`display: ":99"` (Xvfb روی آن دیسپلی) و
`capture_source: "Entire screen (monitor 1)"`.

## امکانات

* **ویزارد راه‌اندازی ۲ دقیقه‌ای** — `--wizard`
* **داشبورد تلگرام** — Home، Sessions، Settings، Health + کارت کنترل هر session
* **کنترل پخش** — pause/resume · mute/unmute · volume ۰–۱۰۰ · speed ۰.۲۵–۴ · seek ±۱۰ ·
  restart · loop
* **هر لینک** — YouTube (watch / youtu.be / shorts / live)، مستقیم
  (`mp4 mp4v webm mkv mov ogv mp3 m4a aac flac wav opus oga`)، و صفحات عمومی با پلیر
  توکار
* **تاریخچه** — `state.json` (سقف ۵۰ رکورد) که ری‌استارت را تحمل می‌کند؛ فایل خراب =
  نگه‌داری `*.corrupt-*` و استور تازه (فایل خراب را رد نمی‌کنیم)
* `/healthz` + `/livez` — بدون هیچ endpoint کنترلی HTTP
* **دکتر + capture self-test** — `--check --capture-test` واقعاً `getDisplayMedia` را
  برای ویدیو (با تغییر فریم) و صدا تست می‌کند
* **امکان عملیات** — Docker/compose (healthcheck, shm)، systemd unit، installer
  یک‌کاماندی، آرشیو ریلیز چندمعماری (`scripts/release.sh`)

## شروع سریع

### الف) استقرار تک‌دستوری و مهاجرت سریع بین سرورها (پیشنهادی)

راه‌اندازی فوری روی هر سرور اوبونتو/دبیان لینوکس با داکر:

```bash
curl -fsSL https://raw.githubusercontent.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/main/scripts/deploy.sh | sh
```

**انتقال به سرور جدید در کمتر از ۱ دقیقه:**
```bash
# ۱. در سرور قبلی، یک نسخه پشتیبان از دیتابیس‌ها و تنظیمات بگیرید:
./scripts/backup.sh /root/telemeetbot-data .

# ۲. فایل بکاپ را به سرور جدید منتقل کرده و اجرا کنید:
./scripts/deploy.sh --restore telemeetbot-backup-YYYYMMDD_HHMMSS.tar.gz
```
تمامی تنظیمات، پروفایل‌های احرازهویت گوگل، جلسات ذخیره‌شده و پلی‌لیست‌ها بدون وقفه بازگردانی می‌شوند.

### ب) Docker Compose

```bash
git clone https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation
cd TeleMeetBot-TeleMeetBot-implementation

cp .env.example .env
docker compose up -d
docker compose logs -f
curl http://127.0.0.1:18080/healthz
```

### ج) نصب یک‌کاماندی (بدون داکر)

```bash
curl -fsSL https://raw.githubusercontent.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/main/scripts/install.sh | bash
```

این اسکریپت معماری را تشخیص می‌دهد (`amd64`/`arm64`/`arm v6, v7`/`386`)، آخرین ریلیز را
دانلود و با SHA256 چک می‌کند، باینری را در `/usr/local/bin` می‌گذارد، unit های systemd
را نصب می‌کند، یک قالب کانفیگ در `/etc/telemeetbot/config.yml` می‌سازد و ویزارد را اجرا می‌کند.

### ج) بیلد از سورس

```bash
go build -o telemeetbot ./cmd/telemeetbot   # Go 1.26+
./telemeetbot --wizard                      # ساخته‌شدن ./telemeetbot.yml
./telemeetbot                               # اجرا (یا --run)
```

### توکن ربات

1. در تلگرام به [@BotFather](https://t.me/BotFather) پیام بدهید `/newbot` و توکن را کپی کنید.
2. آیدی عددی خودتان: `/id` به [@userinfobot](https://t.me/userinfobot) بدهید. یا
   `first_start_claims_admin: true` بگذارید و اولین `/start` را بزنید — اولین کاربر،
   ادمین می‌شود.

### اولین اجرا در تلگرام

```
/start            ⟶ داشبورد (Active sessions، uptime، کیبورد)
/new              ⟶ مرحله ۱/۲: لینک Meet
                  ⟶ مرحله ۲/۲: لینک مدیا
                  ⟶ "Ready to launch" ⟶ 🚀 Launch
```
سپس ربات: Chromium را اجرا می‌کند ⟶ تب مدیا (عنوان قفل‌شده) ⟶ Meet ⟶ guest join ⟶
Present ⟶ شیر تب مدیا ⟶ پخش ⟶ کارت session ⟶ بروزرسانی زنده.

---

## دستورها و دکمه‌های داخل‌خطی

| دستور | معنی |
|---|---|
| `/start` | بازکردن داشبورد |
| `/new` | شروع ویزارد استریم جدید |
| `/new <meet> <media>` | یک‌خطی: هر دو لینک با هم |
| `/sessions` | فهرست sessionهای در حال اجرا |
| `/health` | وضعیت سلامت (سبک دکتر) |
| `/settings` | خلاصه کانفیگ |
| `/help` | راهنما |
| `/stop <id>` | پایان یک session |
| `/cancel` | لغو ویزارد باز |
| `/back` | بازگشت به مرحله قبل با حفظ ورودی‌های معتبر |
| `/home` | بازگشت به داشبورد بدون توقف sessionها |
| `/id` | نمایش آیدی عددی تلگرام شما |
| `/ping` | probe بودن ربات |

ویزارد ساخت session حالت‌های مشخص دارد: لینک Meet ← لینک مدیا ← مرور و تأیید (با دکمه‌های ویرایش جدا برای هر لینک). پیش‌نویس نیمه‌کاره بعد از ری‌استارت ربات باقی می‌ماند (انقضای ۱۵ دقیقه) و با دکمه New stream ادامه می‌یابد. توقف session دکمه تأیید جدا دارد، سرعت پخش با دکمه ⚡️ روی کارت session تنظیم می‌شود و نام نمایشی مهمان از ⚙️ Settings برای هر کاربر ذخیره می‌شود.

زیرنویس و زیرنویس‌گذاری: پنل 💬 در کارت هر session و در تنظیمات — بررسی زیرنویس‌های واقعی منبع (دستی و خودکار جداگانه برچسب‌گذاری می‌شوند)، انتخاب زبان، روشن/خاموش با بازخورد پخش‌کننده. در Download & Stream زیرنویس همراه ویدیو دانلود، به WebVTT تبدیل و به‌صورت track بومی به پخش‌کننده وصل می‌شود. در یوتیوب مستقیم ماژول captions بهترین‌تلاش استفاده می‌شود و محدودیت‌ها صادقانه گزارش می‌گردد. سیاست و زبان زیرنویس در users.db برای هر کاربر ذخیره می‌شود و مستقل از زبان صوتی است.

کنترل کیفیت ویدیو: شش سیاست (خودکار، صرفه‌جویی ≤360p، متعادل ≤720p، HD 720p، فول HD ≤1080p، بهترین کیفیت) روی نردبان استخراج Download & Stream اعمال می‌شود؛ در پخش مستقیم یوتیوب، پخش‌کننده خود یوتیوب کیفیت را انتخاب می‌کند و این محدودیت صادقانه نمایش داده می‌شود. دکمه 🔍 کیفیت‌های موجود را با yt-dlp بررسی می‌کند (بدون دانلود) و 📊 کیفیت درخواستی/انتخاب‌شده/مشاهده‌شده را جدا گزارش می‌دهد. پیش‌فرض کیفیت در ⚙️ Settings ذخیره می‌شود و برای هر آیتم صف می‌توان override گذاشت.

دکمه‌های کارت session:
`⏸ ▶️ 🔇 🔊 🔉 ⏪ ⏩ 🔁 🛑 🔄`. وضعیت‌ها: 🟦 Guest · 🟪 Harness · 🟫 Streaming ·
🟨 Paused · 🟩 Onboard · ⚪️ Ended · 🔴 Error (منبع دقیق در `internal/bot/views.go`).

---

## مرجع کانفیگ

فایل کاملی با همه‌ی کلیدها و توضیح هر کدام در [README.md](README.md#configuration-reference)
انگلیسی است؛ کلیدها، پیش‌فرض‌ها و اعتبارسنجی یکسان است. خلاصه‌ی مهم‌ترین‌ها:

| کلید | پیش‌فرض | توضیح |
|---|---|---|
| `telegram.token` | — | اجباری |
| `telegram.admins` | `[]` | خالی = هیچ‌کس (fail-closed) مگر `first_start_claims_admin` |
| `telegram.first_start_claims_admin` | `true` | اولین `/start` ادمین می‌شود |
| `server.health_addr` | `127.0.0.1:18080` | در Docker: `0.0.0.0:18080` |
| `browser.headless` | `true` | `false` نیاز به Xvfb + `display` دارد |
| `browser.per_session` | `true` | ایزولیشن؛ `false` = یک Chromium مشترک |
| `meet.present_mode` | `tab` | `tab` headless-safe است؛ `screen` برای Xvfb |
| `meet.capture_source` | `TeleMeet Media` | باید با مقدار فلگ auto-select یکی باشد |
| `meet.admit_wait_seconds` | `300` | صبر برای موافقت هاست |
| `limits.max_sessions` | `2` | سقف sessionهای هم‌زمان |
| `media.default_volume` | `80` | ۰ تا ۱۰۰ |
| `media.default_speed` | `1.0` | ۰.۲۵ تا ۴ |
| `paths.data_dir` | `data` | محل `state.json` |

اعتبارسنجی سخت است؛ لیستِ ادمینِ خالی + `first_start_claims_admin: false` =
«کسی کنترل ندارد» با خطای صریح در استارت آپ (باید چنین باشد).

---

## عیب‌یابی

| نشانه | علت / راه‌حل |
|---|---|
| گیر در `Guest` («Waiting for admission») | جلسه اجازه‌ی ورودِ مهمانِ ناشناس را نمی‌دهد یا هاست آفلاین است. اجازه‌ی join مهمان را در تنظیمات Meet فعال کنید. |
| `admission timeout` | همان بالا. با کم‌کردن `admit_wait_seconds` سریع‌تر شکست بخورید. |
| `present can't start` | Chromium قدیمی (<118)، زبان UI میت غیر انگلیسی (`browser.lang=en-US` لازم)، `capture_source` ناهمسان — `--check --capture-test` را بزنید. |
| مدیا پخش نمی‌شود | صفحه اجازه‌ی embed نمی‌دهد؛ لاگ `playerProblem` نشانه دارد؛ دکمه‌ی restart را بزنید. |
| `no such element` بعد تغییر UI میت | ایشو باز کنید با لاگ `(meetctl)` در `debug`. |
| کرش Chromium | `--check` رم را نشان می‌دهد؛ در Docker `shm_size: "1gb"` و `per_session: true`. |

---

## تست

```bash
make test                # تست واحد بدون مرورگر
make test-integration    # + تست واقعی Chromium (title-lock، player doc، getDisplayMedia)
make check               # --check --capture-test واقعی
```

## امنیت

* **fail-closed**: هر درخواست با `telegram.admins` چک می‌شود.
* تنها سطح HTTP: `GET /healthz` و `GET /livez`؛ bind روی `127.0.0.1` (صرفاً مانیتورینگ).
* کانفیگ با توکن `0600` نوشته می‌شود.
* پرفایل‌های Chromium هر بار temp و در پایان پاک می‌شوند.
* به‌خاطر sandbox Chromium داخل کانتینر، `--no-sandbox` لازم است؛ ربات را با یوزر
  کم‌دسترسی اجرا کنید نه root در سیستم خودتان (داکر image از یوزر غیرریشه استفاده می‌کند).

## توسعه

ساختار پوشه‌ها و مسیر رشد در [README.md](README.md) انگلیسی؛ تست‌ها و CI هم‌سان است.
برای انتشار ریلیز:

```bash
VERSION=v1.0.0 make release     # dist/*.tar.gz برای linux/amd64,arm64,armv6,armv7,386
VERSION=v1.0.0 make publish     # + تگ‌زنی، push، gh release create
```

## لایسنس

MIT — [LICENSE](LICENSE).
