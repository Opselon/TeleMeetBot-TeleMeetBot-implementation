package bot

import (
	"context"
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/store"
)

// The /new flow must offer saved meetings, New Google Meet and Cancel, and
// selecting a saved link must jump straight to media selection.

func TestNewOffersSavedMeetingsPicker(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)
	ctx := context.Background()
	if _, err := h.ms.Save(ctx, 42, "Standup", "https://meet.google.com/abc-defg-hij?hl=en-US"); err != nil {
		t.Fatal(err)
	}

	h.message(42, "/new")
	text := h.api.lastSent().text
	for _, want := range []string{"New stream", "Standup", "abc-defg-hij"} {
		if !strings.Contains(text, want) {
			t.Fatalf("picker missing %q: %q", want, text)
		}
	}
	kb := h.api.lastSent().kb
	if kb == nil {
		t.Fatal("picker keyboard missing")
	}
	// One row per saved meeting + New Meet + (Manage, Cancel).
	if len(kb.InlineKeyboard) < 3 {
		t.Fatalf("picker has %d rows, want >= 3", len(kb.InlineKeyboard))
	}
}

func TestNewPickerWithoutSavedMeetings(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)

	h.message(42, "/new")
	text := h.api.lastSent().text
	if !strings.Contains(text, "No saved meetings yet") {
		t.Fatalf("empty picker text: %q", text)
	}
	if !strings.Contains(text, "New Google Meet") == false {
		t.Fatal("picker must still offer New Google Meet")
	}
}

// Picking a saved meeting must skip the link step and land on media.
func TestPickSavedMeetingJumpsToMedia(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)
	ctx := context.Background()
	m, err := h.ms.Save(ctx, 42, "Standup", "https://meet.google.com/abc-defg-hij?hl=en-US")
	if err != nil {
		t.Fatal(err)
	}

	h.message(42, "/new")
	h.callback(42, cbMeetPick+itoa(m.ID))
	if !strings.Contains(h.api.lastSent().text, "step 2/2") {
		t.Fatalf("media step missing: %q", h.api.lastSent().text)
	}
	f := h.bot.getFlow(42)
	if f == nil || f.step != stepMedia {
		t.Fatal("flow not at media step after picking a saved meeting")
	}
	if !strings.Contains(f.meet, "abc-defg-hij") {
		t.Fatalf("flow meet = %q", f.meet)
	}
	if f.meetName != "Standup" {
		t.Fatalf("flow meetName = %q", f.meetName)
	}

	// A media link completes the wizard and launches.
	h.message(42, "https://youtu.be/dQw4w9WgXcQ")
	if !strings.Contains(h.api.lastSent().text, "Ready to launch") {
		t.Fatalf("confirm missing: %q", h.api.lastSent().text)
	}
	h.callback(42, cbLaunch)
	if len(h.eng.requests) != 1 {
		t.Fatalf("engine not started: %+v", h.eng.requests)
	}
	if h.eng.requests[0].MeetLink != "https://meet.google.com/abc-defg-hij?hl=en-US" {
		t.Fatalf("meet link = %q", h.eng.requests[0].MeetLink)
	}
}

// New Google Meet from the picker asks for a fresh link.
func TestPickerNewGoogleMeet(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)

	h.message(42, "/new")
	h.callback(42, cbMeetNew)
	if !strings.Contains(h.api.lastSent().text, "step 1/2") {
		t.Fatalf("step 1 missing: %q", h.api.lastSent().text)
	}
	if f := h.bot.getFlow(42); f == nil || f.step != stepMeet {
		t.Fatal("flow not at meet step")
	}
}

// Cancel from the picker cancels the wizard.
func TestPickerCancel(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)

	h.message(42, "/new")
	h.callback(42, cbCancel)
	if !strings.Contains(h.api.lastSent().text, "Cancelled") {
		t.Fatalf("cancel text: %q", h.api.lastSent().text)
	}
	if f := h.bot.getFlow(42); f != nil {
		t.Fatal("flow not cleared on cancel")
	}
}

// A typed link on the picker is accepted as the meet link.
func TestPickerAcceptsTypedLink(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)

	h.message(42, "/new")
	h.message(42, "meet.google.com/abc-defg-hij")
	if !strings.Contains(h.api.lastSent().text, "step 2/2") {
		t.Fatalf("typed link not accepted: %q", h.api.lastSent().text)
	}
}

// Rename: the picker's rename button waits for a text message.
func TestRenameSavedMeeting(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)
	ctx := context.Background()
	m, err := h.ms.Save(ctx, 42, "Standup", "https://meet.google.com/abc-defg-hij?hl=en-US")
	if err != nil {
		t.Fatal(err)
	}

	h.message(42, "/new")
	h.callback(42, cbMeetList)
	h.callback(42, cbMeetRename+itoa(m.ID))
	if !strings.Contains(h.api.lastSent().text, "Rename meeting") {
		t.Fatalf("rename prompt missing: %q", h.api.lastSent().text)
	}
	h.message(42, "Daily Standup")
	if !strings.Contains(h.api.lastSent().text, "Renamed") {
		t.Fatalf("rename confirmation missing: %q", h.api.lastSent().text)
	}
	got, err := h.ms.Get(ctx, 42, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Daily Standup" {
		t.Fatalf("name = %q", got.Name)
	}
	if f := h.bot.getFlow(42); f != nil {
		t.Fatal("flow not cleared after rename")
	}
}

func TestRenameValidatesLength(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)
	ctx := context.Background()
	m, err := h.ms.Save(ctx, 42, "Standup", "https://meet.google.com/abc-defg-hij?hl=en-US")
	if err != nil {
		t.Fatal(err)
	}
	h.message(42, "/new")
	h.callback(42, cbMeetList)
	h.callback(42, cbMeetRename+itoa(m.ID))
	h.message(42, strings.Repeat("x", 101))
	if !strings.Contains(h.api.lastSent().text, "❌") {
		t.Fatalf("over-long rename accepted: %q", h.api.lastSent().text)
	}
	got, _ := h.ms.Get(ctx, 42, m.ID)
	if got.Name != "Standup" {
		t.Fatalf("name changed despite rejection: %q", got.Name)
	}
}

func TestDeleteSavedMeeting(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)
	ctx := context.Background()
	m, err := h.ms.Save(ctx, 42, "Standup", "https://meet.google.com/abc-defg-hij?hl=en-US")
	if err != nil {
		t.Fatal(err)
	}

	h.message(42, "/new")
	h.callback(42, cbMeetList)
	h.callback(42, cbMeetDelete+itoa(m.ID))
	if !strings.Contains(h.api.lastSent().text, "Saved meetings") {
		t.Fatalf("manage screen after delete: %q", h.api.lastSent().text)
	}
	if list, _ := h.ms.List(ctx, 42); len(list) != 0 {
		t.Fatalf("meeting still present after delete: %+v", list)
	}
}

// Owner isolation at the Telegram layer: a foreign user cannot pick, rename
// or delete another user's meeting through the callbacks.
func TestSavedMeetingOwnerIsolationCallbacks(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)
	ctx := context.Background()
	m, err := h.ms.Save(ctx, 42, "Standup", "https://meet.google.com/abc-defg-hij?hl=en-US")
	if err != nil {
		t.Fatal(err)
	}

	// The stranger is not an admin: every callback is refused first by
	// authorization, so the meeting is unreachable.
	h.callback(7, cbMeetPick+itoa(m.ID))
	if list, _ := h.ms.List(ctx, 42); len(list) != 1 {
		t.Fatalf("owner's meeting changed by a stranger: %+v", list)
	}
	h.callback(7, cbMeetDelete+itoa(m.ID))
	if list, _ := h.ms.List(ctx, 42); len(list) != 1 {
		t.Fatalf("owner's meeting deleted by a stranger: %+v", list)
	}
}

// /new with inline arguments still works and does not show the picker.
func TestNewWithInlineArgumentsSkipsPicker(t *testing.T) {
	h := newHarnessWithMeetings(t, []int64{42}, 2)

	h.message(42, "/new meet.google.com/abc-defg-hij https://youtu.be/dQw4w9WgXcQ")
	if !strings.Contains(h.api.lastSent().text, "Ready to launch") {
		t.Fatalf("inline /new broken: %q", h.api.lastSent().text)
	}
}

// The meeting store is optional: a nil store must not crash /new.
func TestNewWithoutMeetingStore(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	if h.bot.ms != nil {
		t.Fatal("expected nil meeting store in the plain harness")
	}
	h.message(42, "/new")
	if !strings.Contains(h.api.lastSent().text, "New stream") {
		t.Fatalf("picker missing without store: %q", h.api.lastSent().text)
	}
	h.callback(42, cbMeetNew)
	if !strings.Contains(h.api.lastSent().text, "step 1/2") {
		t.Fatalf("step 1 missing without store: %q", h.api.lastSent().text)
	}
}

// Meet flow steps are internal; the picker step id must not collide with the
// wizard steps.
func TestFlowStepsDisjoint(t *testing.T) {
	seen := map[int]bool{}
	for _, step := range []int{stepNone, stepMeet, stepMedia, stepSwitchMedia, stepMeetPick, stepMeetRename} {
		if seen[step] {
			t.Fatal("duplicate flow step constant")
		}
		seen[step] = true
	}
}

// itoa keeps the test file free of an strconv import for one call.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	if neg {
		out = append([]byte{'-'}, out...)
	}
	return string(out)
}

var _ = store.ErrMeetingNotFound
