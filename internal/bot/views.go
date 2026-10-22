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
	cbHome     = "menu:home"
	cbNew      = "menu:new"
	cbSessions = "menu:sessions"
	cbHealth   = "menu:health"
	cbHelp     = "menu:help"
	cbSettings = "menu:settings"
	cbLaunch   = "flow:launch"
	cbCancel   = "flow:cancel"
	cbAgain    = "flow:again"
	cbRefresh  = "menu:refresh"
)

// stateEmoji maps engine states to dashboard emoji.
func stateEmoji(state string) string {
	switch state {
	case engine.StateStarting:
		return "🟡"
	case engine.StateJoining:
		return "🟡"
	case engine.StateAdmitting:
		return "🕓"
	case engine.StateSharing:
		return "🟠"
	case engine.StateStreaming:
		return "🟢"
	case engine.StatePaused:
		return "⏸"
	case engine.StateEnded:
		return "⚪️"
	case engine.StateError:
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
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("📋 Sessions", cbSessions),
			tgbotapi.NewInlineKeyboardButtonData("🩺 Health", cbHealth),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", cbSettings),
			tgbotapi.NewInlineKeyboardButtonData("ℹ️ Help", cbHelp),
		},
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
	sb.WriteString(fmt.Sprintf("⏳ Elapsed: %s", health.FormatUptime(st.Elapsed())))
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
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData(pauseLabel, "act:"+id+":"+pauseAct),
			tgbotapi.NewInlineKeyboardButtonData(muteLabel, "act:"+id+":"+muteAct),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⏪ -10s", "act:"+id+":back10"),
			tgbotapi.NewInlineKeyboardButtonData("⏩ +10s", "act:"+id+":fwd10"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🔉 Vol-", "act:"+id+":voldown"),
			tgbotapi.NewInlineKeyboardButtonData("🔊 Vol+", "act:"+id+":volup"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("♻️ Restart media", "act:"+id+":restart"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "act:"+id+":refresh"),
			tgbotapi.NewInlineKeyboardButtonData("⏹ Stop", "act:"+id+":stop"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome),
		},
	}
	if st.State == engine.StateEnded || st.State == engine.StateError {
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
	sb.WriteString("\n🟢 Service is up. Health endpoint: <code>/healthz</code>")
	return sb.String()
}

// settingsText shows read-only configuration.
func settingsText(c *config.Config) string {
	var sb strings.Builder
	sb.WriteString("⚙️ <b>Settings</b>\n\n")
	sb.WriteString(fmt.Sprintf("Token: <code>%s</code>\n", esc(c.TokenPreview())))
	ids := make([]string, 0, len(c.Telegram.Admins))
	for _, id := range c.Telegram.Admins {
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
	sb.WriteString(fmt.Sprintf("Data dir: <code>%s</code>\n", esc(c.Paths.DataDir)))
	sb.WriteString("\nChange values in <code>telemeetbot.yml</code> and restart the service. Re-run <code>telemeetbot --wizard</code> for a guided edit.")
	return sb.String()
}

// helpText explains usage.
func helpText() string {
	return strings.TrimSpace(`
ℹ️ <b>How TeleMeetBot works</b>

1. Open <b>New stream</b>.
2. Send a Google Meet link (<code>meet.google.com/abc-defg-hij</code>).
3. Send the media link you want on the screen (YouTube, mp4/webm/mp3, or any web page).
4. Confirm — the bot opens Meet as a guest, shares a tab and plays it.

<b>Controls</b>
⏸ Pause / ▶️ Resume — pause the stream
🔇 Mute — mute the shared audio
⏪ ⏩ — seek 10s (configurable)
🔉 🔊 — volume
♻️ Restart — reload the media from the start
⏹ Stop — leave the meeting

<b>Notes</b>
• The bot joins as a guest; meetings that require a Google sign-in cannot be joined.
• If the host requires approval, the card shows <i>admitting</i> until someone lets the bot in.
• Multiple sessions are allowed up to <code>limits.max_sessions</code>.
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

// confirmText summarises the pending stream.
func confirmText(meetLink, mediaLink string, kind engine.MediaKind) string {
	kindLabel := "web page"
	switch kind {
	case engine.MediaYouTube:
		kindLabel = "YouTube"
	case engine.MediaDirect:
		kindLabel = "direct media file"
	}
	return fmt.Sprintf(`✅ <b>Ready to launch</b>

Meet: <code>%s</code>
Media (<i>%s</i>): <code>%s</code>

The bot will open Chromium, join as a guest, share a tab and play the media.`, esc(meetLink), esc(kindLabel), esc(truncate(mediaLink, 120)))
}

// confirmKB offers launch/cancel for a pending stream.
func confirmKB() *tgbotapi.InlineKeyboardMarkup {
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("🚀 Launch", cbLaunch)},
		{
			tgbotapi.NewInlineKeyboardButtonData("✏️ Change links", cbAgain),
			tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", cbCancel),
		},
	}}
}
