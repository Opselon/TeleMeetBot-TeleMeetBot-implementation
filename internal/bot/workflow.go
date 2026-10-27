package bot

// The session-creation workflow: explicit states, typed draft, back/cancel
// navigation and restart recovery.
//
// The wizard's int steps (flows.go) remain the runtime representation, but
// the creation path is projected onto an explicit, persisted state model:
//
//	idle → awaiting_meet_url → awaiting_media_url → reviewing_configuration → (launch: the engine owns the rest)
//
// Rules implemented here:
//
//   - invalid input never advances the workflow;
//   - back preserves the values the user already validated;
//   - every creation transition writes a draft to users.db so a bot restart
//     can resume the wizard instead of losing the user's input;
//   - drafts expire (workflowDraftMaxAge) and are deleted on cancel/launch;
//   - drafts are per-user and never authorize anything by themselves.

import (
	"context"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// workflowDraftMaxAge bounds a persisted draft: after this much inactivity
// an abandoned wizard is dropped instead of resurrected.
const workflowDraftMaxAge = 15 * time.Minute

// callback data for the review-screen edit buttons and draft resume.
const (
	cbEditMeet    = "flow:editmeet"
	cbEditMedia   = "flow:editmedia"
	cbResumeDraft = "flow:resume"
)

// workflowStateOf projects a creation flow onto the explicit state model.
// Non-creation flows (playlists, admin console, media switch, queue add)
// have their own prompts and are not part of the state machine.
func workflowStateOf(f *flow) (string, bool) {
	if f == nil {
		return "", false
	}
	switch f.step {
	case stepMeet, stepMeetPick:
		return store.WorkflowAwaitingMeet, true
	case stepMedia:
		return store.WorkflowAwaitingMedia, true
	case stepNone:
		if strings.TrimSpace(f.meet) != "" && strings.TrimSpace(f.media) != "" {
			return store.WorkflowReviewing, true
		}
		return "", false
	default:
		return "", false
	}
}

// flowFromDraft rebuilds the runtime wizard from a persisted draft.
func flowFromDraft(d store.WorkflowDraft) *flow {
	f := &flow{meet: d.MeetURL, media: d.MediaURL, mediaKind: d.MediaKind, queue: d.ExtraQueue}
	switch d.State {
	case store.WorkflowAwaitingMedia:
		f.step = stepMedia
	case store.WorkflowReviewing:
		f.step = stepNone
	default:
		f.step = stepMeet
	}
	return f
}

// draftFromFlow captures the durable part of a creation flow.
func draftFromFlow(userID int64, f *flow) (store.WorkflowDraft, bool) {
	state, ok := workflowStateOf(f)
	if !ok {
		return store.WorkflowDraft{}, false
	}
	return store.WorkflowDraft{
		UserID:     userID,
		State:      state,
		MeetURL:    f.meet,
		MediaURL:   f.media,
		MediaKind:  f.mediaKind,
		ExtraQueue: f.queue,
	}, true
}

// persistDraft stores the user's creation draft (best-effort: a storage
// failure must not break the wizard — the in-memory flow keeps working).
func (b *Bot) persistDraft(userID int64, f *flow) {
	if b.set == nil {
		return
	}
	d, ok := draftFromFlow(userID, f)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), settingsOpTimeout)
	defer cancel()
	if err := b.set.SaveWorkflowDraft(ctx, d); err != nil {
		b.log.Warnf("[PIPELINE:workflow] save draft %d: %v", userID, err)
	}
}

// dropDraft deletes the user's draft (cancel, launch, expiry).
func (b *Bot) dropDraft(userID int64) {
	if b.set == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), settingsOpTimeout)
	defer cancel()
	if err := b.set.DeleteWorkflowDraft(ctx, userID); err != nil {
		b.log.Warnf("[PIPELINE:workflow] delete draft %d: %v", userID, err)
	}
}

// restoreDraft loads a fresh draft for the user, if one survived a restart.
func (b *Bot) restoreDraft(userID int64) (*flow, bool) {
	if b.set == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), settingsOpTimeout)
	defer cancel()
	d, found, err := b.set.LoadWorkflowDraft(ctx, userID, workflowDraftMaxAge)
	if err != nil || !found {
		return nil, false
	}
	return flowFromDraft(d), true
}

// hasFreshDraft reports whether the dashboard should advertise a resumable
// draft. Storage failures degrade to "no draft".
func (b *Bot) hasFreshDraft(userID int64) bool {
	if b.set == nil {
		return false
	}
	f, ok := b.restoreDraft(userID)
	return ok && f != nil
}

// handleWorkflowCallback routes the creation-workflow callbacks that are not
// plain step prompts: per-field review editing and draft resumption.
func (b *Bot) handleWorkflowCallback(chatID, userID int64, msgID int, q *tgbotapi.CallbackQuery, data string) {
	f := b.getFlow(userID)
	switch data {
	case cbEditMeet, cbEditMedia:
		if f == nil {
			// The wizard is gone (restart, expiry): restart it at the
			// requested field instead of answering into the void.
			f = &flow{step: stepMeet}
		}
		if _, isCreation := workflowStateOf(f); !isCreation && f.step != stepMeetPick {
			_ = b.api.AnswerCallback(q.ID, "nothing to edit — start a new stream")
			_ = b.api.Edit(chatID, msgID, "This wizard is no longer active. Use ➕ <b>New stream</b>.", mainMenuKB())
			return
		}
		if data == cbEditMeet {
			f.step = stepMeet // media draft is preserved
		} else {
			if strings.TrimSpace(f.meet) == "" {
				// No meet yet — the media edit would launch blind.
				f.step = stepMeet
			} else {
				f.step = stepMedia // meet draft is preserved
			}
		}
		b.setFlow(userID, f)
		_ = b.api.AnswerCallback(q.ID, "edit the highlighted field")
		_ = b.api.Edit(chatID, msgID, flowText(f.step, f.meet), cancelKB())
	case cbResumeDraft:
		if f != nil {
			if _, isCreation := workflowStateOf(f); isCreation {
				// Already mid-wizard in memory: just re-render its screen.
				_ = b.api.AnswerCallback(q.ID, "")
				_ = b.api.Edit(chatID, msgID, b.workflowScreenText(userID, f), b.workflowScreenKB(userID, f))
				return
			}
		}
		draft, ok := b.restoreDraft(userID)
		if !ok {
			_ = b.api.AnswerCallback(q.ID, "no draft to resume")
			_ = b.api.Edit(chatID, msgID, b.mainText(userID), mainMenuKB())
			return
		}
		b.setFlow(userID, draft)
		_ = b.api.AnswerCallback(q.ID, "draft restored")
		_ = b.api.Edit(chatID, msgID, b.workflowScreenText(userID, draft), b.workflowScreenKB(userID, draft))
	default:
		_ = b.api.AnswerCallback(q.ID, "unknown workflow action")
	}
}

// engineKindOf maps a persisted media-kind string back to the engine type.
func engineKindOf(kind string) engine.MediaKind {
	switch engine.MediaKind(kind) {
	case engine.MediaYouTube, engine.MediaDirect:
		return engine.MediaKind(kind)
	default:
		return engine.MediaPage
	}
}

// workflowScreenText renders the prompt for wherever the workflow stands.
func (b *Bot) workflowScreenText(userID int64, f *flow) string {
	if f.step == stepNone {
		userLang := b.st.GetUserAudioLang(userID)
		guestName, _ := b.effectiveGuestName(userID)
		return confirmText(f.meet, f.media, engineKindOf(f.mediaKind), userLang, b.st.GetUserMediaMode(userID), guestName, b.hasGoogleAccount(userID))
	}
	return flowText(f.step, f.meet)
}

// workflowScreenKB renders the keyboard for the workflow's current state.
func (b *Bot) workflowScreenKB(userID int64, f *flow) *tgbotapi.InlineKeyboardMarkup {
	if f.step == stepNone {
		u := b.userSettings(userID)
		return confirmKB(u.AudioLang, u.MediaMode, b.hasGoogleAccount(userID))
	}
	return cancelKB()
}

// backFromWorkflow implements /back for the creation workflow. It returns
// true when the command consumed the press.
func (b *Bot) backFromWorkflow(chatID, userID int64) {
	f := b.getFlow(userID)
	if f == nil {
		b.sendText(chatID, "Nothing to go back to.", mainMenuKB())
		return
	}
	switch f.step {
	case stepMedia:
		// Back to the Meet step, preserving the validated media draft.
		f.step = stepMeet
		b.setFlow(userID, f)
		b.sendText(chatID, "⬅️ Back — edit the Meet link (your media draft is kept).", cancelKB())
		b.sendText(chatID, flowText(stepMeet, f.meet), cancelKB())
	case stepNone:
		if _, isCreation := workflowStateOf(f); isCreation {
			f.step = stepMedia
			b.setFlow(userID, f)
			b.sendText(chatID, flowText(stepMedia, f.meet), cancelKB())
			return
		}
		b.clearFlow(userID)
		b.sendText(chatID, "⬅️ Back to the dashboard.", mainMenuKB())
	default:
		// Awaiting the Meet link or a non-creation flow: /back lands on the
		// dashboard without stopping any running session.
		b.clearFlow(userID)
		b.sendText(chatID, "⬅️ Back to the dashboard.", mainMenuKB())
	}
}

// resumeDraftNotice is shown on the dashboard when a fresh draft exists.
func resumeDraftNotice() string {
	return "\n📝 <i>An unfinished stream draft is waiting — tap ➕ <b>New stream</b> to resume it.</i>"
}
