package bot

// Per-user settings: the SQLite-backed preferences screen and its callbacks.
//
// Every setting follows the same loop: load the current value from the
// store, validate the requested change, persist it transactionally, refresh
// the screen with the value that was actually saved, and report failures
// honestly. When the SQLite store is unavailable (b.set == nil) the bot
// falls back to the legacy JSON accessors so the feature never dies just
// because one database could not open.

import (
	"context"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// Settings is the per-user preferences surface the bot needs
// (*store.UserStore implements it). Methods use bounded contexts internally,
// so implementations must not block indefinitely.
type Settings interface {
	Get(ctx context.Context, userID int64) (store.UserSettings, error)
	GetRaw(ctx context.Context, userID int64) (store.UserSettings, bool, error)
	SetAudioLang(ctx context.Context, userID int64, lang string) (store.UserSettings, error)
	SetMediaMode(ctx context.Context, userID int64, mode string) (store.UserSettings, error)
	SetVolume(ctx context.Context, userID int64, volume int) (store.UserSettings, error)
	SetSpeed(ctx context.Context, userID int64, speed float64) (store.UserSettings, error)
	SetAutoplay(ctx context.Context, userID int64, on bool) (store.UserSettings, error)
	SetRepeatMode(ctx context.Context, userID int64, mode string) (store.UserSettings, error)
	ResetUser(ctx context.Context, userID int64) (bool, error)
	ResetAllPreferences(ctx context.Context) (int, error)
	Preferences(ctx context.Context) (map[int64]store.UserSettings, error)
	KnownUserIDs(ctx context.Context, extra ...int64) ([]int64, error)
	SaveSessionQueue(ctx context.Context, snap store.SessionQueueSnapshot) error
	LoadSessionQueue(ctx context.Context, ownerID int64, sessionID string) (store.SessionQueueSnapshot, bool, error)
	LatestInterruptedQueue(ctx context.Context, ownerID int64) (store.SessionQueueSnapshot, bool, error)
	MarkQueuesInterrupted(ctx context.Context) (int, error)
	DeleteSessionQueue(ctx context.Context, ownerID int64, sessionID string) error
	QueueStats(ctx context.Context) (store.QueueStats, error)
}

// settingsOpTimeout bounds one settings operation. SQLite is local; a wedged
// disk must not hold a Telegram handler open.
const settingsOpTimeout = 5 * time.Second

// userSettings returns the caller's saved preferences, falling back to the
// legacy JSON store when SQLite settings are unavailable.
func (b *Bot) userSettings(userID int64) store.UserSettings {
	if b.set != nil {
		ctx, cancel := context.WithTimeout(context.Background(), settingsOpTimeout)
		defer cancel()
		u, err := b.set.Get(ctx, userID)
		if err != nil {
			b.log.Warnf("[PIPELINE:settings] get %d: %v", userID, err)
			return store.DefaultUserSettings()
		}
		return u
	}
	// Legacy fallback: the two preferences the JSON store knows about.
	return store.UserSettings{
		AudioLang:  b.st.GetUserAudioLang(userID),
		MediaMode:  b.st.GetUserMediaMode(userID),
		Autoplay:   true,
		RepeatMode: store.RepeatOff,
	}
}

// applyUserSetting persists one change and returns the refreshed screen.
// The returned toast is empty on success when the saved value speaks for
// itself, and carries the failure reason otherwise.
func (b *Bot) applyUserSetting(userID int64, apply func(ctx context.Context) (store.UserSettings, error)) (store.UserSettings, string) {
	if b.set == nil {
		return b.userSettings(userID), "settings storage unavailable on this installation"
	}
	ctx, cancel := context.WithTimeout(context.Background(), settingsOpTimeout)
	defer cancel()
	u, err := apply(ctx)
	if err != nil {
		b.log.Warnf("[PIPELINE:settings] update %d: %v", userID, err)
		return b.userSettings(userID), truncate(err.Error(), 120)
	}
	return u, ""
}

// settingsScreenText renders the per-user preferences screen. Every value
// shown is the saved value, so a failed write is visible immediately.
func settingsScreenText(u store.UserSettings) string {
	var sb strings.Builder
	sb.WriteString("⚙️ <b>Your settings</b>\n\n")
	sb.WriteString("These preferences are saved for your account and applied to every stream you launch.\n\n")
	sb.WriteString(fmt.Sprintf("🎵 Audio language: <b>%s</b>\n", esc(engine.AudioLangDisplayName(engine.NormalizeAudioLang(u.AudioLang)))))
	sb.WriteString(fmt.Sprintf("%s Media mode: <b>%s</b>\n", engine.PlaybackModeEmoji(engine.NormalizePlaybackMode(u.MediaMode)), esc(engine.PlaybackModeDisplay(engine.NormalizePlaybackMode(u.MediaMode)))))
	if u.Volume > 0 {
		sb.WriteString(fmt.Sprintf("🔊 Start volume: <b>%d%%</b>\n", u.Volume))
	} else {
		sb.WriteString("🔊 Start volume: <i>server default</i>\n")
	}
	if u.Speed > 0 {
		sb.WriteString(fmt.Sprintf("⚡️ Start speed: <b>%.2gx</b>\n", u.Speed))
	} else {
		sb.WriteString("⚡️ Start speed: <i>server default</i>\n")
	}
	if u.Autoplay {
		sb.WriteString("▶️ Auto-advance: <b>on</b> — the next queue item plays when one ends\n")
	} else {
		sb.WriteString("▶️ Auto-advance: <b>off</b> — playback stops at the end of each item\n")
	}
	sb.WriteString(fmt.Sprintf("🔁 Repeat: <b>%s</b>\n", esc(repeatDisplay(u.RepeatMode))))
	sb.WriteString("\n<i>Pick a row below to change it. Changes survive bot restarts.</i>")
	return sb.String()
}

func repeatDisplay(mode string) string {
	switch store.NormalizeRepeatMode(mode) {
	case store.RepeatOne:
		return "one item"
	case store.RepeatAll:
		return "whole queue"
	default:
		return "off"
	}
}

// userSettingsKB renders the settings keyboard: one row per editable value
// plus reset and dashboard. The audio-language and media-mode rows deep-link
// into their dedicated pickers.
func userSettingsKB(u store.UserSettings) *tgbotapi.InlineKeyboardMarkup {
	autoplayLabel := "▶️ Auto-advance: on ✅"
	if !u.Autoplay {
		autoplayLabel = "▶️ Auto-advance: off ⬜️"
	}
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🎵 Audio language", cbAudioLang),
			tgbotapi.NewInlineKeyboardButtonData("⬇️ Media mode", cbMediaMode),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🔊 Start volume", "set:volume"),
			tgbotapi.NewInlineKeyboardButtonData("⚡️ Start speed", "set:speed"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData(autoplayLabel, "set:autoplay"),
			tgbotapi.NewInlineKeyboardButtonData("🔁 Repeat: "+repeatDisplay(u.RepeatMode), "set:repeat"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("♻️ Reset to defaults", "set:reset"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome),
		},
	}}
}

// showUserSettings renders (or refreshes) the settings screen from the store.
func (b *Bot) showUserSettings(chatID int64, msgID int, userID int64) {
	u := b.userSettings(userID)
	text := settingsScreenText(u)
	kb := userSettingsKB(u)
	if msgID > 0 {
		_ = b.api.Edit(chatID, msgID, text, kb)
		return
	}
	b.sendText(chatID, text, kb)
}

// handleSettingsCallback routes every "set:" button. Each verb persists the
// change, then re-renders the screen from the saved row. Verb forms:
//
//	set:back              → back to the settings screen
//	set:volume | set:speed → open the numeric pickers
//	set:vol:<n>           → store the start volume
//	set:spd:<x>           → store the start speed
//	set:autoplay          → toggle auto-advance
//	set:repeat            → cycle off → one → all
//	set:reset             → delete the row (defaults apply again)
func (b *Bot) handleSettingsCallback(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	parts := strings.SplitN(strings.TrimPrefix(data, "set:"), ":", 3)
	verb := parts[0]
	var toast string
	var u store.UserSettings
	switch verb {
	case "back":
		_ = b.api.AnswerCallback(q.ID, "")
		b.showUserSettings(chatID, msgID, userID)
		return
	case "autoplay":
		current := b.userSettings(userID).Autoplay
		u, toast = b.applyUserSetting(userID, func(ctx context.Context) (store.UserSettings, error) {
			return b.set.SetAutoplay(ctx, userID, !current)
		})
		if toast == "" {
			toast = "auto-advance " + boolWord(!current)
		}
	case "repeat":
		current := store.NormalizeRepeatMode(b.userSettings(userID).RepeatMode)
		next := store.RepeatOff
		switch current {
		case store.RepeatOff:
			next = store.RepeatOne
		case store.RepeatOne:
			next = store.RepeatAll
		}
		u, toast = b.applyUserSetting(userID, func(ctx context.Context) (store.UserSettings, error) {
			return b.set.SetRepeatMode(ctx, userID, next)
		})
		if toast == "" {
			toast = "repeat: " + repeatDisplay(next)
		}
	case "volume", "speed":
		// Numeric pickers have their own screens; the row button just opens them.
		_ = b.api.AnswerCallback(q.ID, "")
		if verb == "volume" {
			b.showVolumePicker(chatID, msgID, userID)
		} else {
			b.showSpeedPicker(chatID, msgID, userID)
		}
		return
	case "vol":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad volume")
			return
		}
		v, ok := parseID(parts[1])
		if !ok || v > 100 {
			_ = b.api.AnswerCallback(q.ID, "volume must be 0-100")
			return
		}
		u, toast = b.applyUserSetting(userID, func(ctx context.Context) (store.UserSettings, error) {
			return b.set.SetVolume(ctx, userID, int(v))
		})
		if toast == "" {
			toast = fmt.Sprintf("start volume %d%%", v)
		}
	case "spd":
		if len(parts) < 2 {
			_ = b.api.AnswerCallback(q.ID, "bad speed")
			return
		}
		var speed float64
		if _, err := fmt.Sscanf(parts[1], "%g", &speed); err != nil || speed < 0.25 || speed > 4 {
			_ = b.api.AnswerCallback(q.ID, "speed must be 0.25-4.0")
			return
		}
		u, toast = b.applyUserSetting(userID, func(ctx context.Context) (store.UserSettings, error) {
			return b.set.SetSpeed(ctx, userID, speed)
		})
		if toast == "" {
			toast = fmt.Sprintf("start speed %.2gx", speed)
		}
	case "reset":
		if b.set == nil {
			_ = b.api.AnswerCallback(q.ID, "settings storage unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), settingsOpTimeout)
		_, err := b.set.ResetUser(ctx, userID)
		cancel()
		if err != nil {
			b.log.Warnf("[PIPELINE:settings] reset %d: %v", userID, err)
			_ = b.api.AnswerCallback(q.ID, "reset failed: "+truncate(err.Error(), 60))
			return
		}
		// Keep the legacy store in sync so a downgrade does not resurrect
		// the old values.
		_ = b.st.ResetUser(userID)
		u = b.userSettings(userID)
		toast = "settings reset to defaults"
	default:
		_ = b.api.AnswerCallback(q.ID, "unknown setting")
		return
	}
	_ = b.api.AnswerCallback(q.ID, toast)
	_ = b.api.Edit(chatID, msgID, settingsScreenText(u), userSettingsKB(u))
}

func boolWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// volumeChoices are the start-volume presets. 0 means "server default".
var volumeChoices = []int64{0, 25, 50, 75, 100}

// showVolumePicker renders the start-volume presets.
func (b *Bot) showVolumePicker(chatID int64, msgID int, userID int64) {
	u := b.userSettings(userID)
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, 3)
	for _, v := range volumeChoices {
		label := fmt.Sprintf("%d%%", v)
		if v == 0 {
			label = "default"
		}
		if u.Volume == int(v) {
			label = "✅ " + label
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("set:vol:%d", v)))
		if len(row) == 3 {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, 3)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Settings", "set:back"),
	})
	text := "🔊 <b>Start volume</b>\n\nThe volume new sessions start at. <i>default</i> keeps the server's configured volume."
	_ = b.api.Edit(chatID, msgID, text, &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// speedChoices are the start-speed presets. "default" keeps the server value.
var speedChoices = []string{"default", "0.75", "1", "1.25", "1.5", "2"}

// showSpeedPicker renders the start-speed presets.
func (b *Bot) showSpeedPicker(chatID int64, msgID int, userID int64) {
	u := b.userSettings(userID)
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, 3)
	for _, s := range speedChoices {
		label := s + "x"
		if s == "default" {
			label = "default"
		}
		if fmt.Sprintf("%g", u.Speed) == s {
			label = "✅ " + label
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, "set:spd:"+s))
		if len(row) == 3 {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, 3)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Settings", "set:back"),
	})
	text := "⚡️ <b>Start speed</b>\n\nThe playback speed new sessions start at. <i>default</i> keeps the server's configured speed."
	_ = b.api.Edit(chatID, msgID, text, &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows})
}
