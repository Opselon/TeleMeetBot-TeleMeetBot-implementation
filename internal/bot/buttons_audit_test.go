package bot

import (
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// Regression tests for the dead and stranded controls found by the
// button audit (309 inline buttons traced). Each one pins a fix so the
// control cannot go dead again.
//
// These assert the contract that actually broke: that a button EXISTS
// on a keyboard for every implemented action, and that the back-button
// returns to the screen the user came from. A handler with no emitter
// is a dead button no matter how correct its body is.

// TestAddNextHasAButton pins the q:addnext fix: the queue-add-next
// flow (stepQueueAddNext → EnqueueNext, "insert after the playing
// item") was fully implemented but NO keyboard emitted the callback,
// making the whole feature unreachable.
func TestAddNextHasAButton(t *testing.T) {
	st := engine.Status{ID: "s-tools", QueueLength: 2, QueueIndex: 0}
	kb := playlistQueueToolsKB(st)
	if !hasCallback(kb, "q:addnext:s-tools") {
		t.Fatalf("queue tools keyboard has no q:addnext button: %v", labels(kb))
	}
	// The buttons that were already there must not be displaced.
	for _, want := range []string{"q:repeat:s-tools", "q:shuftoggle:s-tools", "q:save:s-tools", "q:load:s-tools", "q:clear_confirm:s-tools"} {
		if !hasCallback(kb, want) {
			t.Fatalf("queue tools keyboard lost an existing button %q: %v", want, labels(kb))
		}
	}
}

// TestPlaylistClearHasAButton pins the pl:clr fix: the whole
// clear-items flow (pl:clr → playlistClearKB → pl:clrok → ClearItems)
// existed but no keyboard emitted pl:clr, so an operator could never
// reach it.
func TestPlaylistClearHasAButton(t *testing.T) {
	p := store.Playlist{ID: 7, Name: "Mix", OwnerID: 42}
	kb := playlistDetailKB(p, nil, 0, 1)
	if !hasCallback(kb, "pl:clr:7") {
		t.Fatalf("open-playlist keyboard has no pl:clr button: %v", labels(kb))
	}
	// The confirm screen the handler renders must carry the real action.
	clearKB := playlistClearKB(7)
	if !hasCallback(clearKB, "pl:clrok:7") {
		t.Fatalf("clear-confirm keyboard has no pl:clrok button: %v", labels(clearKB))
	}
}

// TestSettingsBackReturnsToTheWizard pins the stranded-user fix.
// confirmKB (the pre-launch review card) renders set:quality and
// set:captions; the pickers' only back button was "set:back", which
// always returned to the settings screen — a dead end with no path
// back to the review card. When a creation flow is open, set:back must
// return to the wizard screen instead.
func TestSettingsBackReturnsToTheWizard(t *testing.T) {
	h := newHarness(t, []int64{42}, 1)

	// Open a creation flow and get it to the review stage: both links
	// staged and the flow at stepNone (WorkflowReviewing).
	h.bot.setFlow(42, &flow{step: stepNone, meet: "https://meet.google.com/abc", media: "https://example.com/v.mp4"})

	h.bot.handleSettingsCallback(1, 42, 7, &tgbotapi.CallbackQuery{ID: "q1", From: &tgbotapi.User{ID: 42}, Data: "set:back"}, "set:back")
	texts := h.api.editTexts()
	if !containsAny(texts, "Confirm", "Review", "Meet") {
		t.Fatalf("set:back with an open wizard must return to the wizard screen, got edits %v", texts)
	}
}

// TestSettingsBackGoesToSettingsWithoutAWizard is the other half of
// the same fix: with no creation flow open the back button must keep
// its historical destination (the settings screen).
func TestSettingsBackGoesToSettingsWithoutAWizard(t *testing.T) {
	h := newHarness(t, []int64{42}, 1)

	h.bot.handleSettingsCallback(1, 42, 7, &tgbotapi.CallbackQuery{ID: "q1", From: &tgbotapi.User{ID: 42}, Data: "set:back"}, "set:back")
	texts := h.api.editTexts()
	if !containsAny(texts, "Your settings") {
		t.Fatalf("set:back with no wizard must return to settings, got edits %v", texts)
	}
}

// TestDraftResumeButtonRenders pins the cbResumeDraft fix: the
// draft-restore handler (workflow.go case cbResumeDraft) existed but
// no keyboard emitted "flow:resume", so the only way back into a saved
// draft was to retype everything.
func TestDraftResumeButtonRenders(t *testing.T) {
	kb := mainMenuKBWithDraft()
	if !hasCallback(kb, "flow:resume") {
		t.Fatalf("draft keyboard has no flow:resume button: %v", labels(kb))
	}
	// The base menu must stay intact.
	base := labels(mainMenuKB())
	withdraft := labels(mainMenuKBWithDraft())
	if len(withdraft) != len(base)+1 {
		t.Fatalf("draft keyboard must add exactly one button: %d vs %d", len(withdraft), len(base))
	}
}

// TestPendingUnknownCallbackAnswersTheSpinner pins the pending.go
// fix: the unknown-action fallback answered with an EMPTY callback id,
// which cannot dismiss the button's spinner, and logged nothing.
func TestPendingUnknownCallbackAnswersTheSpinner(t *testing.T) {
	h := newHarness(t, []int64{42}, 1)
	h.bot.setPending(&pendingMedia{UserID: 42, EntryID: "e1", URL: "http://127.0.0.1:1/d/e1", Kind: engine.MediaDirect, Name: "t.mp3", MIME: "audio/mpeg", Bytes: 10})

	h.bot.handlePendingCallback(1, 42, 7, &tgbotapi.CallbackQuery{ID: "spin", From: &tgbotapi.User{ID: 42}, Data: "pm:bogus"}, "pm:bogus")
	answers := h.api.answersOf()
	if len(answers) == 0 || answers[len(answers)-1] == "" {
		t.Fatalf("unknown pending callback must answer with the real callback id, got %v", answers)
	}
}

// ---- helpers -------------------------------------------------------------

func hasCallback(kb *tgbotapi.InlineKeyboardMarkup, data string) bool {
	if kb == nil {
		return false
	}
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			if btn.CallbackData != nil && *btn.CallbackData == data {
				return true
			}
		}
	}
	return false
}

// editTexts returns the text of every Edit the fake API received, in order.
func (f *fakeAPI) editTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.edits))
	for i, e := range f.edits {
		out[i] = e.text
	}
	return out
}

// answersOf returns every callback answer the fake API received.
func (f *fakeAPI) answersOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.answers...)
}

func containsAny(texts []string, subs ...string) bool {
	for _, t := range texts {
		for _, s := range subs {
			if strings.Contains(t, s) {
				return true
			}
		}
	}
	return false
}
