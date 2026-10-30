package bot

import (
	"strings"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/userstate"
)

// User-state tracking. The tracker is the dashboard's answer to "what is
// every user doing": it sits above the bot's flows/pending/session maps and
// is updated at the handful of points where those change. Every call site is
// nil-guarded so tests that build a Bot without a tracker keep working.

// flowStateOf maps a wizard step to the user-level state. The review phase
// has no dedicated step: a flow with both links staged is reviewing.
func flowStateOf(f *flow) (userstate.State, string) {
	if f == nil {
		return userstate.StateIdle, ""
	}
	switch f.step {
	case stepMeet, stepMeetPick:
		return userstate.StateWizardMeet, f.meetName
	case stepMedia:
		return userstate.StateWizardMedia, ""
	case stepSwitchMedia:
		return userstate.StateSwitchingMedia, f.sessionID
	case stepQueueAdd, stepQueueAddNext:
		return userstate.StateAwaitingText, "queue link"
	case stepMeetRename:
		return userstate.StateAwaitingText, "meeting rename"
	case stepPlaylistName, stepPlaylistRename, stepPlaylistDesc:
		return userstate.StateAwaitingText, "playlist details"
	case stepPlaylistAdd, stepPlaylistImport:
		return userstate.StateAwaitingText, "playlist links"
	case stepPlaylistPlayMeet:
		return userstate.StateWizardMeet, "playlist target"
	case stepAdminAdd:
		return userstate.StateAwaitingText, "admin id"
	case stepAdminBroadcast:
		return userstate.StateAwaitingText, "broadcast text"
	case stepGuestName:
		return userstate.StateAwaitingText, "guest display name"
	case stepNone:
		if strings.TrimSpace(f.meet) != "" && strings.TrimSpace(f.media) != "" {
			return userstate.StateReviewing, ""
		}
	}
	return userstate.StateIdle, ""
}

// trackFlow mirrors a wizard mutation into the tracker.
func (b *Bot) trackFlow(userID int64, f *flow) {
	if b.track == nil || userID == 0 {
		return
	}
	st, detail := flowStateOf(f)
	if st == userstate.StateIdle {
		b.track.Clear(userID)
		return
	}
	b.track.Set(userID, st, detail, "", "")
}

// trackLive marks the owner chat of a session live (or idle when it ends).
// chatID is the Telegram chat that started the session.
func (b *Bot) trackLive(chatID int64, live bool, sessionID string) {
	if b.track == nil || chatID == 0 {
		return
	}
	if !live {
		// Only drop the live marker when the user has no other live session.
		if count := b.liveSessionCount(chatID); count > 0 {
			b.track.Set(chatID, userstate.StateLive, liveDetail(b.liveSessionIDs(chatID)), "", "")
			return
		}
		if f := b.getFlow(chatID); f != nil {
			b.trackFlow(chatID, f)
			return
		}
		b.track.Clear(chatID)
		return
	}
	// A wizard and a live session can coexist; live wins for the dashboard
	// because it is the thing that needs attention.
	b.track.Set(chatID, userstate.StateLive, liveDetail(b.liveSessionIDs(chatID)), "", "")
}

func liveDetail(ids []string) string {
	return strings.Join(ids, ", ")
}

// liveSessionCount counts the chat's sessions that are still active.
func (b *Bot) liveSessionCount(chatID int64) int {
	n := 0
	for _, s := range b.eng.List() {
		if engine.IsTerminalState(s.Snapshot().State) {
			continue
		}
		b.mu.Lock()
		chat := b.sessionChats[string(s.ID)]
		b.mu.Unlock()
		if chat == chatID {
			n++
		}
	}
	return n
}

// liveSessionIDs lists the chat's active session ids.
func (b *Bot) liveSessionIDs(chatID int64) []string {
	var ids []string
	for _, s := range b.eng.List() {
		st := s.Snapshot()
		if engine.IsTerminalState(st.State) {
			continue
		}
		b.mu.Lock()
		chat := b.sessionChats[string(st.ID)]
		b.mu.Unlock()
		if chat == chatID {
			ids = append(ids, string(st.ID))
		}
	}
	return ids
}
