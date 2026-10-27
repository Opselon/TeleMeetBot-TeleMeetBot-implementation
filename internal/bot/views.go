package bot

import (
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/health"
)

// Callback data prefixes.
const (
	cbHome      = "menu:home"
	cbNew       = "menu:new"
	cbSessions  = "menu:sessions"
	cbHealth    = "menu:health"
	cbHelp      = "menu:help"
	cbSettings  = "menu:settings"
	cbAudioLang = "menu:audiolang"
	cbPlaylists = "menu:playlists"
	cbManage    = "menu:manage"
	cbLaunch    = "flow:launch"
	cbCancel    = "flow:cancel"
	cbAgain     = "flow:again"
	cbRefresh   = "menu:refresh"
	cbLangFa    = "lang:fa"
	cbLangEn    = "lang:en"
	cbLangOrig  = "lang:orig"
	cbLangAuto  = "lang:auto"
	// cbMediaMode opens the Download & Stream mode picker; "mode:<choice>"
	// selects one of browser/download/auto.
	cbMediaMode = "menu:mediamode"
	// cbAccount opens the Google Account profile card.
	cbAccount        = "menu:account"
	cbAccountConnect = "account:connect"
	cbAccountRemove  = "account:remove"
	cbAccountHelp    = "account:help"
)

// stateEmoji maps engine states to dashboard emoji.
func stateEmoji(state string) string {
	switch state {
	case engine.StateQueued:
		return "⏳"
	case engine.StateStarting:
		return "🟡"
	case engine.StateJoining:
		return "🚪"
	case engine.StateWaitingForAdmission, engine.StateAdmitting:
		return "✋"
	case engine.StateAdmitted:
		return "✅"
	case engine.StateSharing:
		return "🖥️"
	case engine.StateStreaming, engine.StateActive:
		return "🟢"
	case engine.StatePaused:
		return "⏸"
	case engine.StateLeaving:
		return "🏃"
	case engine.StateEnded, engine.StateCompleted:
		return "⚪️"
	case engine.StateCancelled:
		return "🚫"
	case engine.StateTimedOut:
		return "⏱️"
	case engine.StateError, engine.StateFailed:
		return "🔴"
	default:
		return "⚪️"
	}
}

// fmtClock renders seconds as m:ss or h:mm:ss.
func fmtClock(seconds float64) string {
	if seconds < 0 || seconds != seconds {
		seconds = 0
	}
	total := int(seconds)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// mainMenuText renders the landing dashboard.
func mainMenuText(botName string, active, max int, uptime string) string {
	var sb strings.Builder
	sb.WriteString("🤖 <b>TeleMeetBot</b>")
	if botName != "" {
		sb.WriteString(" — @" + esc(botName))
	}
	sb.WriteString("\n")
	sb.WriteString("<i>Join Google Meet. Share any link. Fully headless.</i>\n\n")
	sb.WriteString(fmt.Sprintf("📡 Active sessions: <b>%d/%d</b>\n", active, max))
	sb.WriteString(fmt.Sprintf("⏱ Uptime: %s\n\n", esc(uptime)))
	sb.WriteString("Pick an action:")
	return sb.String()
}

// mainMenuKB is the landing keyboard.
func mainMenuKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("➕ New stream", cbNew),
			tgbotapi.NewInlineKeyboardButtonData("📚 Playlists", cbPlaylists),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("📋 Sessions", cbSessions),
			tgbotapi.NewInlineKeyboardButtonData("🌐 Audio Language", cbAudioLang),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬇️ Download & Stream", cbMediaMode),
			tgbotapi.NewInlineKeyboardButtonData("🩺 Health", cbHealth),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🎛 Manage", cbManage),
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", cbSettings),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("👤 Google Account", cbAccount),
			tgbotapi.NewInlineKeyboardButtonData("ℹ️ Help", cbHelp),
		},
		{
			tgbotapi.NewInlineKeyboardButtonURL("⭐ Star us on GitHub", "https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation"),
		},
	}}
}

// helpKB is a keyboard for help and about screens with a star button.
func helpKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonURL("⭐ Star on GitHub", "https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome),
		},
	}}
}

// audioLangText renders the language preference selection screen.
func audioLangText(currentLang string) string {
	norm := engine.NormalizeAudioLang(currentLang)
	var sb strings.Builder
	sb.WriteString("🌐 <b>Audio Track Language</b>\n\n")
	sb.WriteString(fmt.Sprintf("Current preference: <b>%s</b>\n\n", esc(engine.AudioLangDisplayName(norm))))
	sb.WriteString("Select your preferred audio language for YouTube playback:\n\n")
	sb.WriteString("• <b>🇮🇷 فارسی (default)</b>\nPlays Persian track if available. If unavailable, falls back to the creator's <i>Original audio</i> (never auto-dubbed English).\n\n")
	sb.WriteString("• <b>🇬🇧 English</b>\nPlays English audio (creator's original if in English, or English track).\n\n")
	sb.WriteString("• <b>🌐 Original audio</b>\nPlays the creator's original audio track, strictly avoiding auto-dubs.\n\n")
	sb.WriteString("• <b>🔄 Auto / best available</b>\nAutomatically selects Persian if present, else original audio.\n\n")
	sb.WriteString("<i>Your selection is saved persistently across bot restarts and applied to all future streams.</i>")
	return sb.String()
}

// audioLangKB provides inline buttons for language selection.
func audioLangKB(currentLang string) *tgbotapi.InlineKeyboardMarkup {
	norm := engine.NormalizeAudioLang(currentLang)
	mark := func(code, label string) string {
		if norm == code {
			return "✅ " + label
		}
		return label
	}
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData(mark("fa", "🇮🇷 فارسی (default)"), cbLangFa),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData(mark("en", "🇬🇧 English"), cbLangEn),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData(mark("orig", "🌐 Original audio"), cbLangOrig),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData(mark("auto", "🔄 Auto / best available"), cbLangAuto),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome),
		},
	}}
}

// mediaModeText renders the Download & Stream mode selection screen.
func mediaModeText(currentMode string) string {
	mode := engine.NormalizePlaybackMode(currentMode)
	var sb strings.Builder
	sb.WriteString("⬇️ <b>Download &amp; Stream</b>\n\n")
	sb.WriteString(fmt.Sprintf("Current mode: <b>%s</b>\n\n", esc(engine.PlaybackModeDisplay(mode))))
	sb.WriteString("Choose how media reaches the share:\n\n")
	sb.WriteString("• <b>🌐 Browser</b>\nPlays the link directly in the media tab, exactly as before.\n\n")
	sb.WriteString("• <b>⬇️ Download &amp; Stream</b>\nDownloads supported media (YouTube via yt-dlp, direct mp4/webm/mkv/mov/mp3/m4a…) to the server first, then plays the local file. Downloaded files are held for the lifetime of the owning session and freed automatically when it ends or switches media (a TTL also reclaims unused files).\n\n")
	sb.WriteString("• <b>🤖 Auto</b>\nPrefers browser playback; when the browser cannot play the link, it falls back to downloading once.\n\n")
	sb.WriteString("<i>Your choice is saved across bot restarts and applies to every new stream. It does not change running sessions.</i>")
	return sb.String()
}

// mediaModeKB provides inline buttons for mode selection.
func mediaModeKB(currentMode string) *tgbotapi.InlineKeyboardMarkup {
	mode := engine.NormalizePlaybackMode(currentMode)
	mark := func(code, label string) string {
		if mode == code {
			return "✅ " + label
		}
		return label
	}
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData(mark("browser", "🌐 Browser"), "mode:browser")},
		{tgbotapi.NewInlineKeyboardButtonData(mark("download", "⬇️ Download & Stream"), "mode:download")},
		{tgbotapi.NewInlineKeyboardButtonData(mark("auto", "🤖 Auto"), "mode:auto")},
		{tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome)},
	}}
}

// backKB is a one-button "back to dashboard" keyboard.
func backKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome)},
	}}
}

// sessionCardText renders one session card.
func sessionCardText(st engine.Status) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s <b>Session %s</b>\n", stateEmoji(st.State), st.ID))
	if st.Title != "" {
		sb.WriteString(fmt.Sprintf("🎞 Media: %s\n", esc(truncate(st.Title, 60))))
	}
	sb.WriteString(fmt.Sprintf("🔗 Meet: <code>%s</code>\n", esc(truncate(st.MeetURL, 80))))
	if st.Dur > 0 {
		sb.WriteString(fmt.Sprintf("⏱ Playback: %s / %s\n", fmtClock(st.Cur), fmtClock(st.Dur)))
	}
	sb.WriteString(fmt.Sprintf("🔊 Volume: %d%%", st.Volume))
	if st.Rate > 0 && st.Rate != 1 {
		sb.WriteString(fmt.Sprintf(" · ⚡️ %.2gx", st.Rate))
	}
	if st.Muted {
		sb.WriteString(" · 🔇 muted")
	}
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("📶 State: <b>%s</b>", esc(st.State)))
	if st.Detail != "" {
		sb.WriteString(" — " + esc(truncate(st.Detail, 120)))
	}
	sb.WriteString("\n")
	if st.AudioTrack != "" {
		sb.WriteString(fmt.Sprintf("🎵 Audio: <b>%s</b>", esc(st.AudioTrack)))
		if st.AudioLang != "" {
			sb.WriteString(fmt.Sprintf(" (pref: %s)", esc(engine.NormalizeAudioLang(st.AudioLang))))
		}
		sb.WriteString("\n")
	}
	if st.PlayMode != "" {
		mode := engine.NormalizePlaybackMode(st.PlayMode)
		sb.WriteString(fmt.Sprintf("%s Mode: <b>%s</b>", engine.PlaybackModeEmoji(mode), esc(engine.PlaybackModeDisplay(mode))))
		if st.LocalFile.Bytes > 0 {
			sb.WriteString(fmt.Sprintf(" · 💾 %.1f MiB", float64(st.LocalFile.Bytes)/(1024*1024)))
		}
		sb.WriteString("\n")
	}
	if st.FallbackReason != "" {
		sb.WriteString(fmt.Sprintf("ℹ️ <i>%s</i>\n", esc(st.FallbackReason)))
	}
	if st.QueueLength > 0 {
		pos := st.QueueIndex + 1
		if pos <= 0 {
			pos = 1
		}
		sb.WriteString(fmt.Sprintf("📋 Playlist: <b>%d / %d</b>\n", pos, st.QueueLength))
	}
	sb.WriteString(fmt.Sprintf("⏳ Elapsed: %s\n", health.FormatUptime(st.Elapsed())))
	sb.WriteString(health.FormatServerResources())
	if st.Error != "" {
		sb.WriteString("\n\n🔴 <b>Error:</b> " + esc(truncate(st.Error, 300)))
	}
	return sb.String()
}

// sessionCardKB renders controls; buttons reflect current state.
func sessionCardKB(st engine.Status) *tgbotapi.InlineKeyboardMarkup {
	id := string(st.ID)
	pauseLabel, pauseAct := "⏸ Pause", "pause"
	if st.State == engine.StatePaused || st.Paused {
		pauseLabel, pauseAct = "▶️ Resume", "resume"
	}
	muteLabel, muteAct := "🔇 Mute", "mute"
	if st.Muted {
		muteLabel, muteAct = "🔊 Unmute", "unmute"
	}
	qLabel := "📋 Queue"
	if st.QueueLength > 0 {
		qLabel = fmt.Sprintf("📋 Queue (%d)", st.QueueLength)
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("⏮ Prev", "act:"+id+":prev"),
			tgbotapi.NewInlineKeyboardButtonData(pauseLabel, "act:"+id+":"+pauseAct),
			tgbotapi.NewInlineKeyboardButtonData("⏭ Next", "act:"+id+":next"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData(qLabel, "act:"+id+":queue"),
			tgbotapi.NewInlineKeyboardButtonData(muteLabel, "act:"+id+":"+muteAct),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⏪ -10s", "act:"+id+":back10"),
			tgbotapi.NewInlineKeyboardButtonData("⏩ +10s", "act:"+id+":fwd10"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⏪ -30s", "act:"+id+":back30"),
			tgbotapi.NewInlineKeyboardButtonData("⏩ +30s", "act:"+id+":fwd30"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⏪ -60s", "act:"+id+":back60"),
			tgbotapi.NewInlineKeyboardButtonData("⏩ +60s", "act:"+id+":fwd60"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🔉 Vol-", "act:"+id+":voldown"),
			tgbotapi.NewInlineKeyboardButtonData("🔊 Vol+", "act:"+id+":volup"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("♻️ Restart media", "act:"+id+":restart"),
			tgbotapi.NewInlineKeyboardButtonData("⚡️ Speed", "act:"+id+":speed"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🔀 Switch media", "act:"+id+":switchurl"),
			tgbotapi.NewInlineKeyboardButtonData("🎛 Tools", "act:"+id+":tools"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("💾 Save queue", "q:save:"+id),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "act:"+id+":refresh"),
			tgbotapi.NewInlineKeyboardButtonData("⏹ Stop", "act:"+id+":stop_confirm"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome),
		},
	}
	if engine.IsTerminalState(st.State) {
		rows = [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome)},
		}
	}
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// sessionsText lists active sessions (or says there are none).
func sessionsText(list []engine.Status) string {
	if len(list) == 0 {
		return "📋 <b>Sessions</b>\n\nNo active sessions. Use ➕ <b>New stream</b> to start one."
	}
	var sb strings.Builder
	sb.WriteString("📋 <b>Active sessions</b>\n\n")
	for _, st := range list {
		title := st.Title
		if title == "" {
			title = truncate(st.MediaURL, 40)
		}
		sb.WriteString(fmt.Sprintf("%s <code>%s</code> · %s · %s\n", stateEmoji(st.State), st.ID, esc(truncate(title, 34)), health.FormatUptime(st.Elapsed())))
	}
	sb.WriteString("\nTap a session to open its controls.")
	return sb.String()
}

// sessionsKB has one row per session plus dashboard.
func sessionsKB(list []engine.Status) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, st := range list {
		label := fmt.Sprintf("%s %s", stateEmoji(st.State), st.ID)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(label, "ses:"+string(st.ID)),
		})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("➕ New stream", cbNew),
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// healthText renders the health dashboard from a snapshot.
func healthText(s health.Snapshot, browserErr string) string {
	var sb strings.Builder
	sb.WriteString("🩺 <b>Health</b>\n\n")
	sb.WriteString(fmt.Sprintf("Version: <code>%s</code>\n", esc(s.Version)))
	sb.WriteString(fmt.Sprintf("Runtime: <code>%s</code>\n", esc(s.GoVersion)))
	sb.WriteString(fmt.Sprintf("Host: <code>%s</code>\n", esc(health.Env())))
	sb.WriteString(fmt.Sprintf("Uptime: %s\n", esc(health.FormatUptime(s.Uptime))))
	sb.WriteString(fmt.Sprintf("Sessions: %d/%d\n", s.ActiveCount, s.MaxSessions))
	if s.BrowserPath != "" {
		sb.WriteString(fmt.Sprintf("Chromium: <code>%s</code>\n", esc(s.BrowserPath)))
	} else {
		sb.WriteString("Chromium: 🔴 not found\n")
	}
	if browserErr != "" {
		sb.WriteString("Browser issue: " + esc(truncate(browserErr, 160)) + "\n")
	}
	sb.WriteString(fmt.Sprintf("Disk: %.1f GB free of %.1f GB\n", s.DiskFreeGB, s.DiskTotalGB))
	sb.WriteString(fmt.Sprintf("Config: <code>%s</code>\n", esc(s.ConfigPath)))
	// Live CPU/RAM/swap telemetry. The sampler reports N/A per metric when
	// the platform cannot provide it, so the screen never invents values.
	sb.WriteString("\n" + health.FormatServerResources())
	sb.WriteString("\n🟢 Service is up. Health endpoint: <code>/healthz</code>")
	return sb.String()
}

// downloadMaxMB renders the configured size limit (default 2048).
func downloadMaxMB(c *config.Config) int {
	if c.Download.MaxSizeMB > 0 {
		return c.Download.MaxSizeMB
	}
	return 2048
}

// settingsText shows read-only configuration.
func settingsText(c *config.Config) string {
	var sb strings.Builder
	sb.WriteString("⚙️ <b>Settings</b>\n\n")
	sb.WriteString(fmt.Sprintf("Token: <code>%s</code>\n", esc(c.TokenPreview())))
	ids := make([]string, 0, len(c.Telegram.Admins))
	for _, id := range c.AdminIDs() {
		ids = append(ids, fmt.Sprintf("%d", id))
	}
	if len(ids) == 0 {
		sb.WriteString("Admins: none yet — first /start claims the bot\n")
	} else {
		sb.WriteString("Admins: <code>" + strings.Join(ids, ", ") + "</code>\n")
	}
	sb.WriteString(fmt.Sprintf("Present mode: <code>%s</code> (source: <code>%s</code>)\n", esc(c.Meet.PresentMode), esc(c.Meet.CaptureSource)))
	sb.WriteString(fmt.Sprintf("Max sessions: %d\n", c.Limits.MaxSessions))
	sb.WriteString(fmt.Sprintf("Headless: %t · per-session browser: %t\n", c.Browser.Headless, c.Browser.PerSession))
	sb.WriteString(fmt.Sprintf("Default volume: %d%% · speed %.2gx\n", c.Media.DefaultVolume, c.Media.DefaultSpeed))
	defLang := c.Media.DefaultAudioLang
	if defLang == "" {
		defLang = "fa"
	}
	sb.WriteString(fmt.Sprintf("Default audio: <code>%s</code> (%s)\n", esc(defLang), esc(engine.AudioLangDisplayName(defLang))))
	sb.WriteString(fmt.Sprintf("Data dir: <code>%s</code>\n", esc(c.Paths.DataDir)))
	if c.Download.Enabled {
		ttl := c.Download.TTLMinutes
		if ttl <= 0 {
			ttl = 60
		}
		sb.WriteString(fmt.Sprintf("Download &amp; Stream: <code>on</code> · expiry %dm · max %d MB\n", ttl, downloadMaxMB(c)))
	} else {
		sb.WriteString("Download &amp; Stream: <code>off</code>\n")
	}
	sb.WriteString("\nChange values in <code>telemeetbot.yml</code> and restart the service. Re-run <code>telemeetbot --wizard</code> for a guided edit.")
	return sb.String()
}

// helpText explains usage.
func helpText() string {
	return strings.TrimSpace(`
ℹ️ <b>How TeleMeetBot works</b>

1. Open <b>➕ New stream</b>.
2. Send a Google Meet link (<code>meet.google.com/abc-defg-hij</code>).
3. Send the media link you want on the screen (YouTube, web page, or direct file).
   <i>Tip: You can also forward Telegram audio/video files up to <b>2.0 GiB (2000 MiB)</b>!</i>
4. Confirm — the bot opens Meet, shares the tab and streams it.

<b>Controls &amp; Features</b>
• ⏸ Pause / ▶️ Resume — pause or resume the stream
• 🔇 Mute / 🔉 🔊 — control audio and volume
• ⏪ ⏩ — seek 10s (configurable)
• 🔀 Switch media — swap media without rejoining
• 🌐 Audio Language — choose Persian (fa), creator original, or dubbed audio
• 👤 Google Account — link your Google account (/account) to display your verified name
• 📚 Playlists &amp; Queue — manage multi-track streaming queues
• ⏹ Stop — leave the meeting

<b>Supported Media</b>
• YouTube (watch &amp; shorts with auto-dubbing preference &amp; ad skipping)
• Direct web video/audio files (<code>.mp4 .webm .mkv .mp3 .m4a</code>)
• Telegram forwarded files up to <b>2000 MiB (2.0 GiB)</b>

⭐ <b>Support Open Source:</b>
If you enjoy TeleMeetBot, please give us a star on <a href="https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation">GitHub</a>!
`)
}

// aboutText renders project information and links.
func aboutText(botName string) string {
	return strings.TrimSpace(`
🤖 <b>TeleMeetBot</b>
<i>Headless Google Meet media streaming bot powered by Go and Chromium.</i>

<b>Capabilities:</b>
• Stream YouTube, direct video/audio, and web media to Google Meet
• Forward Telegram files up to <b>2.0 GiB (2000 MiB)</b>
• Multi-track audio switching (Persian, Original, Dubbed)
• Google OAuth 2.0 &amp; OpenID Connect identity verification
• Full media playlist and queue management
• All-in-One Docker container deployment

⭐ <b>Star us on GitHub:</b>
https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation
`)
}

// flowText renders the new-stream wizard prompt.
func flowText(step int, meetLink string) string {
	switch step {
	case stepMeet:
		return `+ <b>New stream — step 1/2</b>

Send the <b>Google Meet link</b> (or just the code, e.g. <code>abc-defg-hij</code>).

The bot will join as a guest and wait to be admitted if the host requires it.`
	case stepMedia:
		return fmt.Sprintf(`+ <b>New stream — step 2/2</b>

Meet: <code>%s</code>

Now send the <b>media link</b> to share:
• YouTube watch/shorts links
• direct files (<code>.mp4 .webm .mp3 …</code>)
• any web page to put on screen`, esc(meetLink))
	default:
		return ""
	}
}

// cancelKB is the wizard escape hatch.
func cancelKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", cbCancel)},
		{tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome)},
	}}
}

// switchMediaText is the prompt shown while the bot waits for the new media
// link of a switch (the Meet session keeps streaming the old URL meanwhile).
func switchMediaText(st engine.Status) string {
	var sb strings.Builder
	sb.WriteString("🔀 <b>Switch media</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session <code>%s</code> is live; the screen share stays up while the media tab reloads.\n\n", esc(string(st.ID))))
	sb.WriteString(fmt.Sprintf("Current media: <code>%s</code>\n\n", esc(truncate(st.MediaURL, 120))))
	sb.WriteString("Send the <b>new media link</b> to switch to:\n")
	sb.WriteString("• YouTube watch/shorts links\n")
	sb.WriteString("• direct files (<code>.mp4 .webm .mp3 …</code>)\n")
	sb.WriteString("• any web page to put on screen\n\n")
	sb.WriteString("<i>The meeting, the share and all controls are kept. Only the shared tab changes.</i>")
	return sb.String()
}

// confirmText summarises the pending stream. guestName is the effective
// Meet display name (empty = the server default applies).
func confirmText(meetLink, mediaLink string, kind engine.MediaKind, userAudioLang, userMediaMode, guestName string, hasGoogleAccount bool) string {
	kindLabel := "web page"
	switch kind {
	case engine.MediaYouTube:
		kindLabel = "YouTube"
	case engine.MediaDirect:
		kindLabel = "direct media file"
	}
	var sb strings.Builder
	sb.WriteString("✅ <b>Ready to launch</b>\n\n")
	sb.WriteString(fmt.Sprintf("Meet: <code>%s</code>\n", esc(meetLink)))
	sb.WriteString(fmt.Sprintf("Media (<i>%s</i>): <code>%s</code>\n", esc(kindLabel), esc(truncate(mediaLink, 120))))
	if kind == engine.MediaYouTube {
		norm := engine.NormalizeAudioLang(userAudioLang)
		sb.WriteString(fmt.Sprintf("🎵 Audio track: <b>%s</b>\n", esc(engine.AudioLangDisplayName(norm))))
	}
	mode := engine.NormalizePlaybackMode(userMediaMode)
	sb.WriteString(fmt.Sprintf("\n%s Media mode: <b>%s</b>\n", engine.PlaybackModeEmoji(mode), esc(engine.PlaybackModeDisplay(mode))))
	if (kind == engine.MediaYouTube || kind == engine.MediaDirect) && mode != engine.MediaModeBrowser {
		sb.WriteString("<i>The media is downloaded before playback; file is freed when the session ends or switches.</i>\n")
	}
	if guestName != "" {
		sb.WriteString(fmt.Sprintf("\n👤 Joining as: <b>%s</b>\n", esc(truncate(guestName, 60))))
	}
	if hasGoogleAccount {
		sb.WriteString("\n👤 Profile: <b>Google Account</b> ✅\n")
		sb.WriteString("The bot will open Chromium, join with your Google profile, share a tab and play the media.")
	} else {
		sb.WriteString("\n👤 Profile: <b>Guest</b> 🚪\n")
		sb.WriteString("The bot will open Chromium, join as a guest, share a tab and play the media.")
	}
	return sb.String()
}

// confirmKB offers launch, per-field editing and cancel for a pending
// stream. The two edit buttons preserve the validated value of the other
// field; "Change links" stays as the full-restart escape hatch.
func confirmKB(userAudioLang, userMediaMode string, hasGoogleAccount bool) *tgbotapi.InlineKeyboardMarkup {
	norm := engine.NormalizeAudioLang(userAudioLang)
	mode := engine.NormalizePlaybackMode(userMediaMode)
	profileLabel := "👤 Profile: Guest 🚪"
	if hasGoogleAccount {
		profileLabel = "👤 Profile: Google ✅"
	}
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("🚀 Confirm &amp; Start", cbLaunch)},
		{
			tgbotapi.NewInlineKeyboardButtonData("✏️ Edit Meet", cbEditMeet),
			tgbotapi.NewInlineKeyboardButtonData("✏️ Edit Media", cbEditMedia),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🎵 Audio: "+engine.AudioLangDisplayName(norm), cbAudioLang),
			tgbotapi.NewInlineKeyboardButtonData(engine.PlaybackModeEmoji(mode)+" Mode: "+engine.PlaybackModeDisplay(mode), cbMediaMode),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData(profileLabel, cbAccount),
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", cbSettings),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("✏️ Change links", cbAgain),
			tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", cbCancel),
		},
	}}
}

// enqueuedKB is shown right after adding items to an active session's queue.
func enqueuedKB(sessionID, itemID string) *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{
				tgbotapi.NewInlineKeyboardButtonData("🔀 Switch now", "q:jump:"+sessionID+":"+itemID),
				tgbotapi.NewInlineKeyboardButtonData("📋 View Queue", "act:"+sessionID+":queue"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("📱 Session Controls", "ses:"+sessionID),
			},
		},
	}
}

// queuePageSize is how many queue items one screen lists. Eight per-item
// rows keep the keyboard inside Telegram's mobile height.
const queuePageSize = 8

// queueStateLine describes the playback state in one line.
func queueStateLine(st engine.Status) string {
	switch {
	case len(st.Error) > 0:
		return "🔴 <b>Error:</b> " + esc(truncate(st.Error, 160))
	case st.State == engine.StatePaused || st.Paused:
		return "⏸ <b>Paused</b>"
	case st.State == engine.StateEnded || st.State == engine.StateCompleted:
		return "🏁 <i>Completed</i>"
	case st.State == engine.StateError || st.State == engine.StateFailed:
		return "🔴 <b>Failed</b>"
	default:
		return ""
	}
}

// queueViewText formats the queue overview: now playing, position and the
// upcoming tail for one page. page must already be resolved (>= 0); bounds
// are clamped here.
func queueViewText(st engine.Status, items []engine.QueueItem, curIdx, page int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 <b>Media Queue</b> · %d item(s)\n", len(items)))
	if len(items) > 0 && curIdx >= 0 {
		sb.WriteString(fmt.Sprintf("▶️ Position: <b>%d/%d</b>\n", curIdx+1, len(items)))
	}
	if line := queueStateLine(st); line != "" {
		sb.WriteString(line + "\n")
	}
	sb.WriteString("\n")

	if curIdx >= 0 && curIdx < len(items) {
		cur := items[curIdx]
		title := cur.Title
		if title == "" {
			title = cur.URL
		}
		sb.WriteString(fmt.Sprintf("▶️ <b>Now Playing:</b>\n<code>%s</code>\n", esc(truncate(title, 60))))
		if st.Dur > 0 {
			sb.WriteString(fmt.Sprintf("⏱ %s / %s (%d%%)\n", fmtClock(st.Cur), fmtClock(st.Dur), queuePercent(st.Cur, st.Dur)))
		}
		sb.WriteString("\n")
	}

	if len(items) == 0 {
		sb.WriteString("The queue is empty.\nSend any media link in chat to add it, or load a saved playlist below.\n")
		return sb.String()
	}

	start, end := queuePageBounds(len(items), page)
	if start < end {
		sb.WriteString("<b>Queue:</b>\n")
		for i := start; i < end; i++ {
			it := items[i]
			title := it.Title
			if title == "" {
				title = it.URL
			}
			marker := "·"
			if i == curIdx {
				marker = "▶️"
			} else if i < curIdx {
				marker = "✓"
			}
			duration := ""
			if it.Duration > 0 {
				duration = " (" + fmtClock(it.Duration) + ")"
			}
			sb.WriteString(fmt.Sprintf("%s %d. <code>%s</code>%s\n", marker, i+1, esc(truncate(title, 44)), duration))
		}
		total := queuePageCount(len(items))
		if total > 1 {
			sb.WriteString(fmt.Sprintf("<i>Page %d/%d</i>\n", page+1, total))
		}
	}
	if curIdx >= len(items)-1 && curIdx >= 0 {
		sb.WriteString("\n🏁 <i>At end of queue</i>\n")
	}
	return sb.String()
}

// queuePercent renders the playback progress as 0-100.
func queuePercent(cur, dur float64) int {
	if dur <= 0 {
		return 0
	}
	p := int(cur / dur * 100)
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// queuePageBounds clamps a page into the item range [start, end).
func queuePageBounds(n, page int) (int, int) {
	if page < 0 {
		page = 0
	}
	start := page * queuePageSize
	if start >= n {
		start = 0
	}
	end := start + queuePageSize
	if end > n {
		end = n
	}
	return start, end
}

// queuePageCount is the number of pages needed for n items.
func queuePageCount(n int) int {
	if n <= 0 {
		return 1
	}
	return (n + queuePageSize - 1) / queuePageSize
}

// queueViewKB renders the queue controls: transport, repeat/shuffle toggles,
// per-item selection with pagination, and the management rows.
func queueViewKB(st engine.Status, items []engine.QueueItem, curIdx, page int) *tgbotapi.InlineKeyboardMarkup {
	id := string(st.ID)
	var rows [][]tgbotapi.InlineKeyboardButton

	pauseLabel, pauseAct := "⏸ Pause", "pause"
	if st.State == engine.StatePaused || st.Paused {
		pauseLabel, pauseAct = "▶️ Resume", "resume"
	}
	// Transport: prev/next only when the cursor can actually move; the card
	// refresh loop keeps this honest as the queue changes.
	transport := []tgbotapi.InlineKeyboardButton{}
	if st.HasPrevious {
		transport = append(transport, tgbotapi.NewInlineKeyboardButtonData("⏮ Prev", "act:"+id+":prev"))
	}
	transport = append(transport, tgbotapi.NewInlineKeyboardButtonData(pauseLabel, "act:"+id+":"+pauseAct))
	if st.HasNext || st.RepeatMode == engine.RepeatAll {
		transport = append(transport, tgbotapi.NewInlineKeyboardButtonData("⏭ Next", "act:"+id+":next"))
	}
	rows = append(rows, transport)

	// Toggles with explicit on/off indicators.
	repeatLabel := "🔁 Repeat: off"
	switch st.RepeatMode {
	case engine.RepeatOne:
		repeatLabel = "🔁 Repeat: one 🔂"
	case engine.RepeatAll:
		repeatLabel = "🔁 Repeat: all 🔁"
	}
	shuffleLabel := "🔀 Shuffle: off"
	if st.Shuffle {
		shuffleLabel = "🔀 Shuffle: on ✅"
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData(repeatLabel, "q:repeat:"+id),
		tgbotapi.NewInlineKeyboardButtonData(shuffleLabel, "q:shuftoggle:"+id),
	})

	// Per-item selection: every visible item is a button that plays it.
	if len(items) > 0 {
		start, end := queuePageBounds(len(items), page)
		for i := start; i < end; i++ {
			it := items[i]
			title := it.Title
			if title == "" {
				title = it.URL
			}
			label := fmt.Sprintf("%d. %s", i+1, truncate(title, 30))
			if i == curIdx {
				label = "▶️ " + label
			}
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("q:jump:%s:%s", id, it.ID)),
			})
		}
		// Pagination row.
		total := queuePageCount(len(items))
		if total > 1 {
			if page < 0 {
				page = 0
			}
			if page >= total {
				page = total - 1
			}
			var pager []tgbotapi.InlineKeyboardButton
			if page > 0 {
				pager = append(pager, tgbotapi.NewInlineKeyboardButtonData("◀️", fmt.Sprintf("q:page:%s:%d", id, page-1)))
			}
			pager = append(pager, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d/%d", page+1, total), fmt.Sprintf("q:page:%s:%d", id, page)))
			if page < total-1 {
				pager = append(pager, tgbotapi.NewInlineKeyboardButtonData("▶️", fmt.Sprintf("q:page:%s:%d", id, page+1)))
			}
			rows = append(rows, pager)
		}
	}

	// Management rows.
	manage := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("➕ Add media", "q:add:"+id),
	}
	if len(items) > 0 {
		manage = append(manage, tgbotapi.NewInlineKeyboardButtonData("💾 Save as playlist", "q:save:"+id))
	}
	rows = append(rows, manage)

	if len(items) > 1 {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("↕️ Reorder", "q:move_menu:"+id),
			tgbotapi.NewInlineKeyboardButtonData("❌ Remove item", "q:rm_menu:"+id),
		})
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📚 Load playlist", "q:load:"+id),
			tgbotapi.NewInlineKeyboardButtonData("🗑 Clear…", "q:clear_confirm:"+id),
		})
	} else if len(items) == 1 {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("🗑 Clear…", "q:clear_confirm:"+id),
		})
	}

	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("📱 Session controls", "ses:"+id),
		tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "act:"+id+":queue"),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome),
	})

	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// queueRemoveMenuKB displays upcoming items to be removed.
func queueRemoveMenuKB(id string, items []engine.QueueItem, curIdx int) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	count := 0
	for i, it := range items {
		if i == curIdx {
			continue
		}
		if count >= 8 {
			break
		}
		title := it.Title
		if title == "" {
			title = it.URL
		}
		label := fmt.Sprintf("❌ %d. %s", i+1, truncate(title, 25))
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("q:rm:%s:%s", id, it.ID)),
		})
		count++
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Back to Queue", "act:"+id+":queue"),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// speedMenuText renders the playback-speed picker for a live session.
func speedMenuText(st engine.Status) string {
	var sb strings.Builder
	sb.WriteString("⚡️ <b>Playback speed</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session <code>%s</code>\n", esc(string(st.ID))))
	if st.Rate > 0 {
		sb.WriteString(fmt.Sprintf("Current: <b>%.2gx</b>\n\n", st.Rate))
	}
	sb.WriteString("The speed applies immediately to the shared player.")
	return sb.String()
}

// speedMenuKB renders the speed presets. The active preset is marked.
func speedMenuKB(st engine.Status) *tgbotapi.InlineKeyboardMarkup {
	id := string(st.ID)
	presets := []float64{0.5, 0.75, 1, 1.25, 1.5, 2}
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, 3)
	for _, p := range presets {
		label := fmt.Sprintf("%.2gx", p)
		if p == 1 {
			label = "1x"
		}
		if st.Rate >= p-0.01 && st.Rate <= p+0.01 {
			label = "✅ " + label
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("act:%s:rate:%d", id, int(p*100))))
		if len(row) == 3 {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, 3)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Session controls", "ses:"+id),
		tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// stopConfirmText asks before tearing a session down.
func stopConfirmText(st engine.Status) string {
	var sb strings.Builder
	sb.WriteString("⚠️ <b>Stop this session?</b>\n\n")
	title := st.Title
	if title == "" {
		title = st.MediaURL
	}
	sb.WriteString(fmt.Sprintf("%s <code>%s</code>", stateEmoji(st.State), esc(string(st.ID))))
	if title != "" {
		sb.WriteString(" · " + esc(truncate(title, 48)))
	}
	sb.WriteString("\n\nThe bot leaves the meeting, stops the stream and releases the browser. This cannot be undone.")
	return sb.String()
}

// stopConfirmKB is the destructive-action confirmation. The confirm button
// carries the same session id, and stop_ok re-checks the session is still
// alive, so a stale confirm can never stop a different session.
func stopConfirmKB(id string) *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("⏹ Stop session", "act:"+id+":stop_ok"),
			tgbotapi.NewInlineKeyboardButtonData("❌ Keep running", "act:"+id+":refresh"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
		},
	}}
}

// queueClearConfirmKB confirms clearing upcoming items or all other items.
func queueClearConfirmKB(id string) *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{
				tgbotapi.NewInlineKeyboardButtonData("🗑 Clear Upcoming Only", "q:do_clear:"+id),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("🧹 Clear All Except Current", "q:do_clear_all:"+id),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "act:"+id+":queue"),
			},
		},
	}
}
