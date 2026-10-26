package bot

import (
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// Staged ("pending") Telegram media playlist.
//
// A user can forward multiple audio/video files to the bot at any moment.
// Each file is saved into the managed media directory and leased to the user's
// flow ("flow:<userID>"). Staged files survive until adopted by a session,
// enqueued, or discarded.
const (
	pendingMaxAge = 30 * time.Minute

	cbPendingStart   = "pm:start"
	cbPendingSwitch  = "pm:sw:"
	cbPendingEnqueue = "pm:enq:"
	cbPendingDrop    = "pm:drop"
)

// pendingMedia is one forwarded file waiting for a decision.
type pendingMedia struct {
	UserID  int64
	EntryID string
	URL     string
	Kind    engine.MediaKind
	Name    string
	MIME    string
	Bytes   int64
	Created time.Time
}

func pendingOwner(userID int64) string { return fmt.Sprintf("flow:%d", userID) }

// addPending appends a staged file to the user's pending playlist.
func (b *Bot) addPending(pm *pendingMedia) {
	if pm == nil {
		return
	}
	b.mu.Lock()
	if b.pending == nil {
		b.pending = map[int64][]*pendingMedia{}
	}
	b.pending[pm.UserID] = append(b.pending[pm.UserID], pm)
	total := len(b.pending[pm.UserID])
	b.mu.Unlock()
	b.log.Infof("[PIPELINE:ingest] user %d: staged media %s (%s, queue size: %d)",
		pm.UserID, pm.EntryID, pm.Name, total)
}

// setPending replaces the user's staged media with a single item.
func (b *Bot) setPending(pm *pendingMedia) {
	if pm == nil {
		return
	}
	b.dropPending(pm.UserID, "replaced by new file")
	b.addPending(pm)
}

// getPending returns a copy of all staged files for the user.
func (b *Bot) getPending(userID int64) []*pendingMedia {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := b.pending[userID]
	if len(items) == 0 {
		return nil
	}
	copied := make([]*pendingMedia, len(items))
	copy(copied, items)
	return copied
}

// takePending removes all staged files from the user's slot and returns them.
func (b *Bot) takePending(userID int64) []*pendingMedia {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := b.pending[userID]
	delete(b.pending, userID)
	return items
}

// dropPending releases leases and deletes all staged files for the user.
func (b *Bot) dropPending(userID int64, why string) int {
	items := b.takePending(userID)
	if len(items) == 0 {
		return 0
	}
	mgr := b.mediaMgr()
	count := 0
	for _, pm := range items {
		if mgr != nil {
			if mgr.ReleaseAndDelete(pm.EntryID, pendingOwner(userID)) {
				count++
			}
		}
	}
	b.log.Infof("[PIPELINE:ingest] user %d: dropped %d staged media items (%s)", userID, len(items), why)
	return count
}

// handoffAllPending moves all staged files' leases to the adopting session.
func (b *Bot) handoffAllPending(userID int64) {
	items := b.takePending(userID)
	mgr := b.mediaMgr()
	if mgr == nil {
		return
	}
	for _, pm := range items {
		mgr.Release(pm.EntryID, pendingOwner(userID))
	}
	b.log.Infof("[PIPELINE:ingest] user %d: handed %d staged media items to session", userID, len(items))
}

// handoffPending moves a specific staged file's lease to its session.
func (b *Bot) handoffPending(userID int64, entryID string) {
	b.mu.Lock()
	items := b.pending[userID]
	idx := -1
	for i, pm := range items {
		if pm.EntryID == entryID {
			idx = i
			break
		}
	}
	if idx >= 0 {
		b.pending[userID] = append(items[:idx], items[idx+1:]...)
		if len(b.pending[userID]) == 0 {
			delete(b.pending, userID)
		}
	}
	b.mu.Unlock()

	if mgr := b.mediaMgr(); mgr != nil && entryID != "" {
		mgr.Release(entryID, pendingOwner(userID))
	}
	b.log.Infof("[PIPELINE:ingest] user %d: handed staged media %s to its session", userID, entryID)
}

// sweepPending expires staged files the user never acted on.
func (b *Bot) sweepPending() {
	b.mu.Lock()
	var expiredUsers []int64
	for uid, items := range b.pending {
		allExpired := true
		for _, pm := range items {
			if time.Since(pm.Created) <= pendingMaxAge {
				allExpired = false
				break
			}
		}
		if allExpired && len(items) > 0 {
			expiredUsers = append(expiredUsers, uid)
		}
	}
	b.mu.Unlock()

	for _, uid := range expiredUsers {
		b.dropPending(uid, "staged files expired")
	}
}

// pendingText renders the staged-file card.
func pendingText(items []*pendingMedia, live []engine.Status) string {
	if len(items) == 0 {
		return "📥 <b>No staged media</b>\n\nForward audio or video files to stream them."
	}

	var sb strings.Builder
	if len(items) == 1 {
		pm := items[0]
		sb.WriteString("📥 <b>Ready to stream</b>\n\n")
		sb.WriteString(fmt.Sprintf("🎞 <code>%s</code>\n", esc(truncate(pm.Name, 70))))
		sb.WriteString(fmt.Sprintf("📦 %s · %s\n", strings.ToUpper(string(pm.Kind)), humanBytesLocal(pm.Bytes)))
	} else {
		sb.WriteString(fmt.Sprintf("📥 <b>Ready to stream (%d items queued)</b>\n\n", len(items)))
		for i, pm := range items {
			if i >= 10 {
				sb.WriteString(fmt.Sprintf("<i>... and %d more</i>\n", len(items)-10))
				break
			}
			sb.WriteString(fmt.Sprintf("%d. <code>%s</code> (%s)\n", i+1, esc(truncate(pm.Name, 40)), humanBytesLocal(pm.Bytes)))
		}
	}

	if len(live) == 1 {
		sb.WriteString(fmt.Sprintf("\nSession <code>%s</code> is live — add to queue, switch to it, or start a new stream.", esc(string(live[0].ID))))
	} else if len(live) > 1 {
		sb.WriteString(fmt.Sprintf("\n%d sessions are live — pick an action below.", len(live)))
	} else {
		sb.WriteString("\nSend a Google Meet link or press <b>Start a new stream</b> to launch.")
	}
	return sb.String()
}

// pendingKB builds inline actions for staged media.
func (b *Bot) pendingKB(items []*pendingMedia, live []engine.Status) *tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton

	if len(live) == 1 {
		id := string(live[0].ID)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("➕ Add to active queue", cbPendingEnqueue+id),
			tgbotapi.NewInlineKeyboardButtonData("🔀 Switch live session", cbPendingSwitch+id),
		})
	} else if len(live) > 1 {
		for _, st := range live {
			id := string(st.ID)
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData("➕ Queue "+id, cbPendingEnqueue+id),
				tgbotapi.NewInlineKeyboardButtonData("🔀 Switch "+id, cbPendingSwitch+id),
			})
		}
	}

	if b.cfg == nil || len(b.eng.Active()) < b.cfg.Limits.MaxSessions {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("▶️ Start a new stream", cbPendingStart),
		})
	}
	rows = append(rows,
		[]tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("🗑 Discard staged media", cbPendingDrop)},
		[]tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("⬅️ Dashboard", cbHome)},
	)
	return &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// handlePendingCallback dispatches actions from the staged-media card.
func (b *Bot) handlePendingCallback(chatID, userID int64, msgID int, data string) {
	items := b.getPending(userID)
	if len(items) == 0 {
		_ = b.api.Edit(chatID, msgID, "⌛️ No files are staged. Forward audio/video again.", mainMenuKB())
		return
	}

	switch {
	case data == cbPendingStart:
		// Advance to meet link picker with all staged files
		first := items[0]
		f := &flow{step: stepMeet, media: first.URL, mediaID: first.EntryID}
		for _, it := range items[1:] {
			f.queue = append(f.queue, it.URL)
		}
		b.setFlow(userID, f)
		_ = b.api.Edit(chatID, msgID, stagedStartQueueText(items), cancelKB())

	case strings.HasPrefix(data, cbPendingEnqueue):
		sid := strings.TrimPrefix(data, cbPendingEnqueue)
		s := b.eng.Get(engine.SessionID(sid))
		if s == nil {
			_ = b.api.Edit(chatID, msgID, "This session is gone — cannot enqueue.", mainMenuKB())
			return
		}
		var urls []string
		for _, it := range items {
			urls = append(urls, it.URL)
		}
		added, err := s.Enqueue(urls...)
		if err != nil {
			_ = b.api.Edit(chatID, msgID, "🔴 <b>Enqueue failed</b>\n\n"+esc(truncate(err.Error(), 300)), b.pendingKB(items, b.statuses()))
			return
		}
		b.handoffAllPending(userID)
		st := s.Snapshot()
		_ = b.api.Edit(chatID, msgID, fmt.Sprintf("✅ Added %d item(s) to queue of session <code>%s</code> (Total: %d)", len(added), sid, st.QueueLength), sessionCardKB(st))

	case strings.HasPrefix(data, cbPendingSwitch):
		sid := strings.TrimPrefix(data, cbPendingSwitch)
		s := b.eng.Get(engine.SessionID(sid))
		if s == nil {
			_ = b.api.Edit(chatID, msgID, "This session is gone — nothing to switch.", mainMenuKB())
			return
		}
		first := items[0]
		if err := s.SwitchMedia(first.URL); err != nil {
			b.log.Warnf("[PIPELINE:ingest] switch %s to staged media %s: %v", sid, first.EntryID, err)
			_ = b.api.Edit(chatID, msgID, "🔴 <b>Switch failed</b>\n\n"+esc(truncate(err.Error(), 300)), b.pendingKB(items, b.statuses()))
			return
		}
		if len(items) > 1 {
			var remaining []string
			for _, it := range items[1:] {
				remaining = append(remaining, it.URL)
			}
			_, _ = s.Enqueue(remaining...)
		}
		b.handoffAllPending(userID)
		st := s.Snapshot()
		_ = b.api.Edit(chatID, msgID, sessionCardText(st), sessionCardKB(st))

	case data == cbPendingDrop:
		n := b.dropPending(userID, "user discarded staged queue")
		_ = b.api.Edit(chatID, msgID, fmt.Sprintf("🗑 Discarded %d staged file(s).", n), mainMenuKB())

	default:
		_ = b.api.AnswerCallback("", "unknown action")
	}
}

// stagedStartQueueText prompts for the Meet link for single or multi-item staged media.
func stagedStartQueueText(items []*pendingMedia) string {
	if len(items) == 1 {
		return fmt.Sprintf(
			"▶️ <b>New stream</b>\n\n🎞 <code>%s</code>\n\nSend the Google Meet link to present it to — the file is already downloaded.",
			esc(truncate(items[0].Name, 70)),
		)
	}
	return fmt.Sprintf(
		"▶️ <b>New stream (%d items queued)</b>\n\nSend the Google Meet link to present this playlist — all files are already downloaded.",
		len(items),
	)
}
