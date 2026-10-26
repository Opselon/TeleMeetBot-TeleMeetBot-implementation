package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// Callback data prefixes for the saved-meetings picker. The "meet:<id>"
// form is stable: a saved row selected from the picker must keep working
// after a restart, and the id is the SQLite primary key.
const (
	cbMeetNew    = "meet:new"
	cbMeetList   = "meet:list"
	cbMeetPick   = "meet:"
	cbMeetDone   = "meet:done"
	cbMeetDelete = "meetdel:"
	cbMeetRename = "meetren:"
)

// maxMeetButtons bounds the inline keyboard so a long saved list cannot
// overflow Telegram's button cap; the store bounds rows to
// maxSavedMeetings, this is the display cap per screen.
const maxMeetButtons = 8

// meetPickerText renders the /new entry point: saved links first, then the
// options to create a fresh meeting or cancel.
func meetPickerText(userName string, saved []store.Meeting) string {
	var sb strings.Builder
	sb.WriteString("➕ <b>New stream</b>\n\n")
	if len(saved) > 0 {
		sb.WriteString(fmt.Sprintf("📌 <b>Saved meetings (%d)</b> — pick one to jump straight to media:\n\n", len(saved)))
		for _, m := range saved {
			sb.WriteString(fmt.Sprintf("• <b>%s</b>\n   <code>%s</code>\n", esc(m.Name), esc(m.MeetURL)))
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("No saved meetings yet. Start a new Google Meet and you can save the link here for next time.\n\n")
	}
	sb.WriteString("Choose an option:")
	return sb.String()
}

// meetPickerKB builds the /new picker keyboard: one button per saved
// meeting (up to maxMeetButtons), then New Google Meet and Cancel.
func meetPickerKB(saved []store.Meeting) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{}
	for i, m := range saved {
		if i >= maxMeetButtons {
			break
		}
		label := truncate(m.Name, 28)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📌 "+label, cbMeetPick+strconv.FormatInt(m.ID, 10)),
		})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("🆕 New Google Meet", cbMeetNew),
	})
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("📋 Manage saved", cbMeetList),
		tgbotapi.NewInlineKeyboardButtonData("✖️ Cancel", cbCancel),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// meetManageText renders the saved-meetings manager screen.
func meetManageText(saved []store.Meeting) string {
	var sb strings.Builder
	sb.WriteString("📋 <b>Saved meetings</b>\n\n")
	if len(saved) == 0 {
		sb.WriteString("Nothing saved yet. Start a stream with /new and save the Meet link from the session card.\n")
		return sb.String()
	}
	for _, m := range saved {
		sb.WriteString(fmt.Sprintf("• <b>%s</b>\n   <code>%s</code>\n   Added %s\n",
			esc(m.Name), esc(m.MeetURL), m.CreatedAt.Format("2006-01-02 15:04")))
	}
	sb.WriteString("\nTap a button below to rename or delete.")
	return sb.String()
}

// meetManageKB builds the management keyboard: rename/delete rows.
func meetManageKB(saved []store.Meeting) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{}
	for i, m := range saved {
		if i >= maxMeetButtons {
			break
		}
		label := truncate(m.Name, 20)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("✏️ "+label, cbMeetRename+strconv.FormatInt(m.ID, 10)),
			tgbotapi.NewInlineKeyboardButtonData("🗑", cbMeetDelete+strconv.FormatInt(m.ID, 10)),
		})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Back", cbMeetDone),
	})
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// startMeetPicker opens the /new flow at the saved-meetings picker. It is
// the new first step of the wizard: selecting a saved link moves straight
// to media selection, "New Google Meet" asks for a fresh link.
func (b *Bot) startMeetPicker(chatID, userID int64) {
	saved := b.savedMeetings(userID)
	b.setFlow(userID, &flow{step: stepMeetPick})
	b.sendText(chatID, meetPickerText(b.api.BotUsername(), saved), meetPickerKB(saved))
}

// savedMeetings returns the user's saved meetings, or nil when the store is
// unavailable (feature disabled) or the read fails. A read failure is
// logged and degraded to an empty list: the user can still start a new
// meeting, and the error is reported in the log.
func (b *Bot) savedMeetings(userID int64) []store.Meeting {
	if b.ms == nil {
		return nil
	}
	saved, err := b.ms.List(context.Background(), userID)
	if err != nil {
		b.log.Warnf("[PIPELINE:meetings] list %d: %v", userID, err)
		return nil
	}
	return saved
}

// pickSavedMeet adopts a saved meeting into the wizard and jumps to media
// selection. Fail-closed: a foreign or missing row never reaches the flow.
func (b *Bot) pickSavedMeet(chatID, userID int64, msgID int, id int64) {
	if b.ms == nil {
		_ = b.api.AnswerCallback("", "saved meetings unavailable")
		return
	}
	m, err := b.ms.Get(context.Background(), userID, id)
	if err != nil {
		_ = b.api.AnswerCallback("", hb(err))
		b.log.Warnf("[PIPELINE:meetings] pick %d by %d: %v", id, userID, err)
		return
	}
	f := &flow{step: stepMedia, meet: m.MeetURL, meetName: m.Name}
	b.setFlow(userID, f)
	_ = b.api.AnswerCallback("", "selected: "+truncate(m.Name, 40))
	_ = b.api.Edit(chatID, msgID, flowText(stepMedia, m.MeetURL), cancelKB())
}

// hb turns a store error into a short human-readable toast.
func hb(err error) string {
	switch {
	case err == store.ErrMeetingNotFound:
		return "meeting not found"
	case err == store.ErrDuplicateURL:
		return "already saved"
	case err == store.ErrTooManyMeetings:
		return "too many saved meetings"
	default:
		return "cannot open saved meetings"
	}
}

// handleMeetCallback routes the saved-meetings callbacks.
func (b *Bot) handleMeetCallback(chatID, userID int64, msgID int, data string) {
	b.log.Infof("[PIPELINE:meetings] callback data=%q from user=%d chat=%d msg=%d", data, userID, chatID, msgID)
	switch {
	case data == cbMeetNew:
		_ = b.api.AnswerCallback("", "enter a meet link")
		b.setFlow(userID, &flow{step: stepMeet})
		_ = b.api.Edit(chatID, msgID, flowText(stepMeet, ""), cancelKB())
	case data == cbMeetList:
		_ = b.api.AnswerCallback("", "")
		_ = b.api.Edit(chatID, msgID, meetManageText(b.savedMeetings(userID)), meetManageKB(b.savedMeetings(userID)))
	case data == cbMeetDone:
		_ = b.api.AnswerCallback("", "")
		saved := b.savedMeetings(userID)
		_ = b.api.Edit(chatID, msgID, meetPickerText(b.api.BotUsername(), saved), meetPickerKB(saved))
	case strings.HasPrefix(data, cbMeetPick):
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbMeetPick), 10, 64)
		if err != nil {
			_ = b.api.AnswerCallback("", "invalid meeting")
			return
		}
		b.pickSavedMeet(chatID, userID, msgID, id)
	case strings.HasPrefix(data, cbMeetDelete):
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbMeetDelete), 10, 64)
		if err != nil {
			_ = b.api.AnswerCallback("", "invalid meeting")
			return
		}
		b.deleteSavedMeet(chatID, userID, msgID, id)
	case strings.HasPrefix(data, cbMeetRename):
		id, err := strconv.ParseInt(strings.TrimPrefix(data, cbMeetRename), 10, 64)
		if err != nil {
			_ = b.api.AnswerCallback("", "invalid meeting")
			return
		}
		b.renameSavedMeet(chatID, userID, msgID, id)
	default:
		_ = b.api.AnswerCallback("", "unknown action")
	}
}

// deleteSavedMeet removes a meeting after an ownership check. The row is
// re-fetched by owner before the delete, so a foreign id can never be
// removed even if the callback data is forged.
func (b *Bot) deleteSavedMeet(chatID, userID int64, msgID int, id int64) {
	if b.ms == nil {
		_ = b.api.AnswerCallback("", "saved meetings unavailable")
		return
	}
	if _, err := b.ms.Get(context.Background(), userID, id); err != nil {
		_ = b.api.AnswerCallback("", hb(err))
		return
	}
	if err := b.ms.Delete(context.Background(), userID, id); err != nil {
		_ = b.api.AnswerCallback("", hb(err))
		b.log.Warnf("[PIPELINE:meetings] delete %d by %d: %v", id, userID, err)
		return
	}
	_ = b.api.AnswerCallback("", "deleted")
	saved := b.savedMeetings(userID)
	_ = b.api.Edit(chatID, msgID, meetManageText(saved), meetManageKB(saved))
}

// renameSavedMeet starts a rename: the flow remembers the row id and the
// next text message becomes the new name.
func (b *Bot) renameSavedMeet(chatID, userID int64, msgID int, id int64) {
	if b.ms == nil {
		_ = b.api.AnswerCallback("", "saved meetings unavailable")
		return
	}
	if _, err := b.ms.Get(context.Background(), userID, id); err != nil {
		_ = b.api.AnswerCallback("", hb(err))
		return
	}
	b.setFlow(userID, &flow{step: stepMeetRename, meetID: id})
	_ = b.api.AnswerCallback("", "send the new name")
	_ = b.api.Edit(chatID, msgID, "✏️ <b>Rename meeting</b>\n\nSend the new name as a text message (max 100 characters):", cancelKB())
}

// saveCurrentMeet persists the flow's Meet link for the user. Called after
// a session starts so the link is one tap away next time.
func (b *Bot) saveCurrentMeet(userID int64, meetURL string) {
	if b.ms == nil {
		return
	}
	name := engine.Shortcode(meetURL)
	if name == "" {
		name = "Meeting"
	}
	if _, err := b.ms.Save(context.Background(), userID, name, meetURL); err != nil {
		b.log.Warnf("[PIPELINE:meetings] save %d: %v", userID, err)
	}
}

// tryMeetingsInput handles a text message while the flow is in one of the
// meeting steps. Returns true when the message was consumed.
func (b *Bot) tryMeetingsInput(chatID, userID int64, f *flow, text string) bool {
	switch f.step {
	case stepMeetRename:
		name := strings.TrimSpace(text)
		if name == "" {
			b.sendText(chatID, "❌ Name cannot be empty. Send a new name:", cancelKB())
			return true
		}
		if len(name) > 100 {
			b.sendText(chatID, "❌ Name too long (max 100 characters). Send a new name:", cancelKB())
			return true
		}
		if b.ms == nil {
			b.sendText(chatID, "🔴 Saved meetings are unavailable.", mainMenuKB())
			b.clearFlow(userID)
			return true
		}
		if _, err := b.ms.Rename(context.Background(), userID, f.meetID, name); err != nil {
			b.sendText(chatID, "🔴 "+esc(hb(err)), mainMenuKB())
			b.clearFlow(userID)
			return true
		}
		b.clearFlow(userID)
		b.sendText(chatID, "✅ Renamed to <b>"+esc(name)+"</b>.", mainMenuKB())
		return true
	}
	return false
}
