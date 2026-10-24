package bot

import (
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/store"
)

// ---- Download & Stream: button, persistence, launch -----------------------

// TestMediaModeButtonOnDashboard proves the new button is a real control: the
// main menu carries it, it opens the mode picker, and each choice persists
// across a reload (restart).
func TestMediaModeButtonOnDashboard(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)

	// 1. The main menu must carry the dedicated button with a real label.
	h.message(42, "/start")
	sent := h.api.lastSent()
	if sent.kb == nil {
		t.Fatal("main menu has no keyboard")
	}
	label := buttonLabel(sent.kb, cbMediaMode)
	if label == "" {
		t.Fatalf("main menu keyboard lacks the %q callback", cbMediaMode)
	}
	if !strings.Contains(label, "Download & Stream") {
		t.Fatalf("the button label does not name the feature: %q", label)
	}

	// 2. Opening the picker edits the card with the three modes.
	h.callback(42, cbMediaMode)
	last := h.api.lastEdit()
	if !strings.Contains(last.text, "Download &amp; Stream") {
		t.Fatalf("mode picker did not render: %q", last.text)
	}
	picker := mediaModeKB(h.st.GetUserMediaMode(42))
	for _, want := range []string{"mode:browser", "mode:download", "mode:auto"} {
		if !kbHasCallback(picker, want) {
			t.Fatalf("mode picker keyboard lacks %q", want)
		}
	}

	// 3. Choosing Download persists and re-renders with a checkmark.
	h.callback(42, "mode:download")
	if got := h.st.GetUserMediaMode(42); got != "download" {
		t.Fatalf("mode not persisted: got %q", got)
	}
	if got := buttonLabel(mediaModeKB(h.st.GetUserMediaMode(42)), "mode:download"); !strings.Contains(got, "✅") {
		t.Fatalf("the selected mode does not carry a checkmark: %q", got)
	}

	// 4. A restart keeps the choice: a fresh store over the same file sees it.
	reloaded := reloadStore(t, h)
	if got := reloaded.GetUserMediaMode(42); got != "download" {
		t.Fatalf("mode lost across a reload: got %q", got)
	}
}

// TestMediaModeDefaultIsBrowser proves an upgrade changes nothing: a user who
// never touched the setting gets Browser, and Browser is checkmarked.
func TestMediaModeDefaultIsBrowser(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	if got := h.st.GetUserMediaMode(42); got != "browser" {
		t.Fatalf("default mode should be browser, got %q", got)
	}
	h.callback(42, cbMediaMode)
	if got := buttonLabel(mediaModeKB(h.st.GetUserMediaMode(42)), "mode:browser"); !strings.Contains(got, "✅") {
		t.Fatalf("Browser should carry the checkmark by default: %q", got)
	}
	if got := buttonLabel(mediaModeKB(h.st.GetUserMediaMode(42)), "mode:download"); strings.Contains(got, "✅") {
		t.Fatalf("Download should NOT be checkmarked by default: %q", got)
	}
}

// TestMediaModeIsSentToTheEngine proves the persisted mode reaches the engine
// when a session launches.
func TestMediaModeIsSentToTheEngine(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	if err := h.st.SetUserMediaMode(42, "auto"); err != nil {
		t.Fatalf("SetUserMediaMode: %v", err)
	}
	h.launchSession(42)

	if len(h.eng.requests) != 1 {
		t.Fatalf("expected one Start request, got %d", len(h.eng.requests))
	}
	if got := h.eng.requests[0].PlayMode; got != "auto" {
		t.Fatalf("StartRequest.PlayMode = %q, want %q", got, "auto")
	}
}

// TestModeRejectedForUnknownValue proves a bogus callback cannot store an
// unknown mode: it normalises to Browser rather than corrupting state.
func TestModeRejectedForUnknownValue(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	h.callback(42, "mode:teleport")
	if got := h.st.GetUserMediaMode(42); got != "browser" {
		t.Fatalf("unknown mode should normalise to browser, got %q", got)
	}
}

// TestConfirmCardShowsTheMode proves the launch confirmation displays the
// selected mode and offers the mode button.
func TestConfirmCardShowsTheMode(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	if err := h.st.SetUserMediaMode(42, "download"); err != nil {
		t.Fatalf("SetUserMediaMode: %v", err)
	}
	h.message(42, "/new")
	h.message(42, "meet.google.com/abc-defg-hij")
	h.message(42, "https://example.com/clip.mp4")
	sent := h.api.lastSent()
	if !strings.Contains(sent.text, "Download &amp; Stream") {
		t.Fatalf("confirmation card does not show the mode: %q", sent.text)
	}
	if !kbHasCallback(sent.kb, cbMediaMode) {
		t.Fatal("confirmation keyboard lacks the mode button")
	}
}

// ---- helpers ---------------------------------------------------------------

// buttonLabel returns the label of the keyboard button carrying data.
func buttonLabel(kb *tgbotapi.InlineKeyboardMarkup, data string) string {
	if kb == nil {
		return ""
	}
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			if b.CallbackData != nil && *b.CallbackData == data {
				return b.Text
			}
		}
	}
	return ""
}

// kbHasCallback reports whether a keyboard carries a callback data value.
func kbHasCallback(kb *tgbotapi.InlineKeyboardMarkup, data string) bool {
	return buttonLabel(kb, data) != ""
}

// reloadStore builds a fresh store over the same persisted file, proving the
// preference survives a restart.
func reloadStore(t *testing.T, h *harness) *store.Store {
	t.Helper()
	return store.Load(filepath.Join(h.cfg.Paths.DataDir, "state.json"))
}

// launchSession drives the wizard to a launched session as user.
func (h *harness) launchSession(user int64) {
	h.message(user, "/new")
	h.message(user, "meet.google.com/abc-defg-hij")
	h.message(user, "https://example.com/clip.mp4")
	h.callback(user, cbLaunch)
}
