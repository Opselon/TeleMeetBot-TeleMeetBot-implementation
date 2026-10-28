package bot

// Advanced session/queue tools. These are the buttons that make a running
// playlist manageable without retyping links: reorder, shuffle once, sort,
// save the queue as a reusable playlist, and inspect the queue from /queue.
// The transport controls (prev/next/pause), the repeat and shuffle toggles
// and the per-item picker live in flows.go with the rest of the callbacks.

import (
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// handleQueueToolsCallback handles the q: verbs that are not part of the basic
// queue view. It returns true when the callback was consumed.
func (b *Bot) handleQueueToolsCallback(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, sub, sid string, parts []string) bool {
	s := b.eng.Get(engine.SessionID(sid))
	if s == nil {
		if sub == "move_menu" || sub == "save" || sub == "mvup" || sub == "mvdn" || sub == "noop" {
			_ = b.api.AnswerCallback(q.ID, "session not found")
			_ = b.api.Edit(chatID, msgID, "This session has ended.", mainMenuKB())
			return true
		}
		return false
	}
	switch sub {
	case "noop":
		_ = b.api.AnswerCallback(q.ID, "")
		return true
	case "move_menu":
		_ = b.api.AnswerCallback(q.ID, "")
		b.renderQueueMoveMenu(chatID, msgID, s)
		return true
	case "mvup", "mvdn":
		if len(parts) < 4 {
			_ = b.api.AnswerCallback(q.ID, "bad item")
			return true
		}
		delta := -1
		if sub == "mvdn" {
			delta = 1
		}
		err := s.MoveQueueItem(parts[3], delta)
		if err != nil {
			_ = b.api.AnswerCallback(q.ID, truncate(err.Error(), 60))
			return true
		}
		_ = b.api.AnswerCallback(q.ID, "moved")
		b.persistQueue(s)
		b.renderQueueMoveMenu(chatID, msgID, s)
		return true
	case "save":
		_ = b.api.AnswerCallback(q.ID, "send a playlist name")
		b.saveQueueStart(chatID, userID, sid)
		return true
	default:
		return false
	}
}

// renderQueueMoveMenu shows the upcoming items with per-item up/down buttons.
func (b *Bot) renderQueueMoveMenu(chatID int64, msgID int, s *engine.Session) {
	b.setCardView(string(s.ID), chatID, msgID, "queue")
	st := s.Snapshot()
	items, curIdx := s.QueueSnapshot()
	text, kb := queueMoveMenu(st, items, curIdx)
	_ = b.api.Edit(chatID, msgID, text, kb)
}

// queueMoveMenu renders the reorder screen for an active queue.
func queueMoveMenu(st engine.Status, items []engine.QueueItem, curIdx int) (string, *tgbotapi.InlineKeyboardMarkup) {
	var sb strings.Builder
	sb.WriteString("↕️ <b>Reorder queue</b>\n\n")
	sb.WriteString(fmt.Sprintf("Session <code>%s</code> · %d item(s)\n\n", esc(string(st.ID)), len(items)))
	start := curIdx + 1
	if start < 0 {
		start = 0
	}
	if start >= len(items) {
		sb.WriteString("Nothing is upcoming. The current item is the last one.\n\n")
		sb.WriteString("<i>Clear upcoming or add more links to reorder.</i>")
		return sb.String(), &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
			{tgbotapi.NewInlineKeyboardButtonData("📋 Back to queue", "act:"+string(st.ID)+":queue")},
		}}
	}
	sb.WriteString("Move an upcoming item with ⬆️ / ⬇️. Playback is not interrupted.\n\n")
	rows := [][]tgbotapi.InlineKeyboardButton{}
	shown := 0
	for i := start; i < len(items) && shown < 8; i++ {
		it := items[i]
		title := it.Title
		if strings.TrimSpace(title) == "" {
			title = store.DefaultPlaylistItemTitle(it.URL)
		}
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%d. %s", i+1, truncate(title, 28)), "q:noop:"+string(st.ID)),
			tgbotapi.NewInlineKeyboardButtonData("⬆️", fmt.Sprintf("q:mvup:%s:%s", st.ID, it.ID)),
			tgbotapi.NewInlineKeyboardButtonData("⬇️", fmt.Sprintf("q:mvdn:%s:%s", st.ID, it.ID)),
		})
		shown++
	}
	if len(items)-start > shown {
		sb.WriteString(fmt.Sprintf("<i>… and %d more</i>\n", len(items)-start-shown))
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("📋 Back to queue", "act:"+string(st.ID)+":queue"),
		tgbotapi.NewInlineKeyboardButtonData("🏠 Dashboard", cbHome),
	})
	return sb.String(), &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}
