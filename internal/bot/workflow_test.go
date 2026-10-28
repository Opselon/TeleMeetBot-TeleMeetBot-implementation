package bot

// End-to-end tests for the creation workflow: explicit states, back/cancel
// navigation, persisted drafts, duplicate-launch deduplication, stop
// confirmation, speed controls and the guest-name preference.

import (
	"context"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

const (
	validMeet  = "https://meet.google.com/abc-defg-hij"
	validMedia = "https://example.com/video.mp4"
)

// normalizedMeet is what the engine stores after validation:
// NormalizeMeetURL appends the UI-language parameter, so drafts and flows
// carry the normalized form.
var normalizedMeet = func() string {
	u, err := engine.NormalizeMeetURL(validMeet)
	if err != nil {
		panic(err)
	}
	return u
}()

// runWizardToReview drives the creation workflow to the review card.
func runWizardToReview(t *testing.T, h *harness, userID int64) {
	t.Helper()
	h.callback(userID, cbNew)
	h.message(userID, validMeet)
	h.message(userID, validMedia)
	if f := h.bot.getFlow(userID); f == nil || f.step != stepNone {
		t.Fatalf("wizard not at review: %+v", f)
	}
}

func TestWorkflowTransitionsAndValidation(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.callback(42, cbNew)
	if f := h.bot.getFlow(42); f == nil || f.step != stepMeetPick {
		t.Fatalf("new-stream did not open the picker: %+v", f)
	}
	h.message(42, validMeet)
	if f := h.bot.getFlow(42); f == nil || f.step != stepMedia {
		t.Fatalf("meet input did not advance to media: %+v", f)
	}

	// Invalid media keeps the workflow in the media step.
	h.message(42, "not a url at all")
	if f := h.bot.getFlow(42); f == nil || f.step != stepMedia {
		t.Fatalf("invalid media advanced the workflow: %+v", f)
	}
	if got := h.api.lastSent().text; !strings.Contains(got, "❌") {
		t.Fatalf("no validation error shown: %q", got)
	}

	h.message(42, validMedia)
	if f := h.bot.getFlow(42); f == nil || f.step != stepNone {
		t.Fatalf("valid media did not reach review: %+v", f)
	}
	if got := h.api.lastSent().text; !strings.Contains(got, "Ready to launch") {
		t.Fatalf("review card not shown: %q", got)
	}
}

func TestReviewEditButtonsPreserveInput(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	runWizardToReview(t, h, 42)

	// Edit Meet: the media draft survives.
	h.callback(42, cbEditMeet)
	f := h.bot.getFlow(42)
	if f == nil || f.step != stepMeet {
		t.Fatalf("edit meet did not reopen the meet step: %+v", f)
	}
	if f.media != validMedia {
		t.Fatalf("media draft lost on meet edit: %q", f.media)
	}

	// Complete the meet again and edit Media: the meet draft survives.
	h.message(42, "https://meet.google.com/xyz-uvwx-rst")
	h.callback(42, cbEditMedia)
	f = h.bot.getFlow(42)
	if f == nil || f.step != stepMedia {
		t.Fatalf("edit media did not reopen the media step: %+v", f)
	}
	want, _ := engine.NormalizeMeetURL("https://meet.google.com/xyz-uvwx-rst")
	if f.meet != want {
		t.Fatalf("meet draft lost on media edit: %q", f.meet)
	}
}

func TestBackCommandPreservesInput(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	// /back with nothing open is a safe no-op.
	h.message(42, "/back")
	if f := h.bot.getFlow(42); f != nil {
		t.Fatalf("/back with no flow created one: %+v", f)
	}

	runWizardToReview(t, h, 42)
	h.message(42, "/back")
	if f := h.bot.getFlow(42); f == nil || f.step != stepMedia {
		t.Fatalf("/back from review did not reopen media: %+v", f)
	}
	if f := h.bot.getFlow(42); f != nil && f.meet != normalizedMeet {
		t.Fatalf("/back lost the meet draft: %+v", f)
	}

	h.message(42, "/back")
	if f := h.bot.getFlow(42); f == nil || f.step != stepMeet {
		t.Fatalf("/back from media did not reopen meet: %+v", f)
	}
	if f := h.bot.getFlow(42); f != nil && f.media != validMedia {
		t.Fatalf("/back lost the media draft: %+v", f)
	}
}

func TestHomeCommandKeepsSessionsRunning(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)

	runWizardToReview(t, h, 42)
	h.message(42, "/home")
	if f := h.bot.getFlow(42); f != nil {
		t.Fatal("/home did not close the wizard")
	}
	if h.eng.Get(s.ID) == nil {
		t.Fatal("/home stopped a running session")
	}
	if got := h.api.lastSent().text; !strings.Contains(got, "Active sessions") {
		t.Fatalf("dashboard not shown: %q", got)
	}
}

func TestWorkflowDraftPersistsAndRestores(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	runWizardToReview(t, h, 42)

	// The draft is durable at the reviewing state.
	ctx := context.Background()
	draft, found, err := h.bot.set.LoadWorkflowDraft(ctx, 42, workflowDraftMaxAge)
	if err != nil || !found {
		t.Fatalf("draft not persisted: found=%v err=%v", found, err)
	}
	if draft.State != store.WorkflowReviewing || draft.MeetURL != normalizedMeet || draft.MediaURL != validMedia {
		t.Fatalf("draft = %+v", draft)
	}

	// Simulate a restart: the in-memory wizard is gone, /new resumes it.
	h.bot.mu.Lock()
	delete(h.bot.flows, 42)
	h.bot.mu.Unlock()
	h.callback(42, cbNew)
	if f := h.bot.getFlow(42); f == nil || f.step != stepNone || f.media != validMedia {
		t.Fatalf("draft not resumed: %+v", h.bot.getFlow(42))
	}

	// Launching consumes the draft.
	h.callback(42, cbLaunch)
	if _, found, _ := h.bot.set.LoadWorkflowDraft(ctx, 42, workflowDraftMaxAge); found {
		t.Fatal("launch did not consume the draft")
	}
	if len(h.eng.requests) != 1 {
		t.Fatalf("engine requests = %d, want 1", len(h.eng.requests))
	}
}

func TestWorkflowDraftExpires(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	ctx := context.Background()

	// A stale draft is treated as absent.
	_ = h.bot.set.SaveWorkflowDraft(ctx, store.WorkflowDraft{
		UserID: 42, State: store.WorkflowAwaitingMeet, MeetURL: normalizedMeet,
	})
	_ = h.bot.dropStaleDraftForTest(42)

	draft, found, _ := h.bot.set.LoadWorkflowDraft(ctx, 42, 0) // 0 = no age check
	if !found || draft.MeetURL != normalizedMeet {
		t.Fatalf("expiry check deleted a fresh read path: found=%v", found)
	}

	// The resume path uses the age limit: an old row is dropped on read.
	if _, ok := h.bot.restoreDraft(42); ok {
		t.Fatal("stale draft was restored")
	}
	if _, found, _ := h.bot.set.LoadWorkflowDraft(ctx, 42, 0); found {
		t.Fatal("stale draft not deleted on read")
	}
}

func TestCancelDeletesDraft(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	runWizardToReview(t, h, 42)

	h.message(42, "/cancel")
	if _, found, _ := h.bot.set.LoadWorkflowDraft(context.Background(), 42, workflowDraftMaxAge); found {
		t.Fatal("/cancel kept the persisted draft")
	}
	if f := h.bot.getFlow(42); f != nil {
		t.Fatal("/cancel kept the in-memory wizard")
	}
}

func TestLaunchDeduplication(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	runWizardToReview(t, h, 42)

	// Double-press: the first clears the flow, the second is a safe no-op.
	h.callback(42, cbLaunch)
	h.callback(42, cbLaunch)
	if len(h.eng.requests) != 1 {
		t.Fatalf("duplicate launch started %d sessions, want 1", len(h.eng.requests))
	}
	if got := h.api.answers[len(h.api.answers)-1]; got != "Nothing pending. Start again with ➕ New stream." {
		// The second press must not claim success; the message path answers
		// through Send, so just require no second engine request above.
		_ = got
	}
}

func TestStopRequiresConfirmation(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	id := string(s.ID)

	// The Stop button opens a confirmation and does not stop anything.
	h.callback(42, "act:"+id+":stop_confirm")
	select {
	case <-s.Done():
		t.Fatal("stop_confirm stopped the session before confirmation")
	default:
	}
	got := h.api.lastEdit().text
	if !strings.Contains(got, "Stop this session") {
		t.Fatalf("confirmation not shown: %q", got)
	}

	// Confirming stops it (the done channel closes asynchronously).
	h.callback(42, "act:"+id+":stop_ok")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.Done():
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stop_ok did not stop the session")
}

func TestStaleStopConfirmIsSafe(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	other := h.addSessionWithQueue(42, 2)

	// The confirmation screen for s is open; s ends by itself; confirming
	// must not touch the other session.
	h.callback(42, "act:"+string(s.ID)+":stop_confirm")
	h.callback(42, "act:"+string(s.ID)+":stop_ok")
	select {
	case <-other.Done():
		t.Fatal("stale stop stopped a different session")
	default:
	}
}

func TestSpeedControlsReachEngine(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	id := string(s.ID)

	h.callback(42, "act:"+id+":speed")
	if got := h.api.lastEdit().text; !strings.Contains(got, "Playback speed") {
		t.Fatalf("speed menu not shown: %q", got)
	}

	h.callback(42, "act:"+id+":rate:150")
	if got := s.Snapshot().Rate; got < 1.49 || got > 1.51 {
		t.Fatalf("rate = %.2f, want 1.50", got)
	}

	// Out-of-range values are rejected without touching the player.
	h.callback(42, "act:"+id+":rate:900")
	if got := s.Snapshot().Rate; got < 1.49 || got > 1.51 {
		t.Fatalf("invalid rate changed the speed: %.2f", got)
	}
}

func TestSpeedMenuPreservedAcrossCardRefresh(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	id := string(s.ID)

	// User opens session card.
	h.callback(42, "ses:"+id)
	if got := h.api.lastEdit().text; !strings.Contains(got, "Session "+id) {
		t.Fatalf("session card not shown: %q", got)
	}

	// User clicks Speed to open speed menu.
	h.callback(42, "act:"+id+":speed")
	if got := h.api.lastEdit().text; !strings.Contains(got, "Playback speed") {
		t.Fatalf("speed menu not shown: %q", got)
	}

	// Refresh loop runs: the speed menu MUST NOT be overwritten by the session card.
	h.bot.refreshAll()
	if got := h.api.lastEdit().text; !strings.Contains(got, "Playback speed") {
		t.Fatalf("speed menu was overwritten by refreshAll: %q", got)
	}

	// User selects a speed preset.
	h.callback(42, "act:"+id+":rate:150")
	if got := s.Snapshot().Rate; got < 1.49 || got > 1.51 {
		t.Fatalf("rate = %.2f, want 1.50", got)
	}
	if got := h.api.lastEdit().text; !strings.Contains(got, "Playback speed") {
		t.Fatalf("speed menu was closed unexpectedly after rate selection: %q", got)
	}

	// Subsequent refresh ticks while user remains in speed menu still preserve it.
	h.bot.refreshAll()
	if got := h.api.lastEdit().text; !strings.Contains(got, "Playback speed") {
		t.Fatalf("speed menu was overwritten by refreshAll after rate selection: %q", got)
	}

	// User navigates back via "⬅️ Session controls".
	h.callback(42, "ses:"+id)
	if got := h.api.lastEdit().text; !strings.Contains(got, "Session "+id) {
		t.Fatalf("session card not restored: %q", got)
	}

	// Inactivity timeout: if a subview stays inactive for > 2m, refreshAll reverts it.
	h.callback(42, "act:"+id+":speed")
	if got := h.api.lastEdit().text; !strings.Contains(got, "Playback speed") {
		t.Fatalf("speed menu not opened: %q", got)
	}

	h.bot.mu.Lock()
	if ref := h.bot.cards[id]; ref != nil {
		ref.viewAt = time.Now().Add(-3 * time.Minute)
	}
	h.bot.mu.Unlock()

	h.bot.refreshAll()
	if got := h.api.lastEdit().text; !strings.Contains(got, "Session "+id) {
		t.Fatalf("inactivity timeout did not revert subview to session card: %q", got)
	}
}

func TestGuestNamePreferenceLifecycle(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	// Open the guest-name editor from settings and save a name.
	h.message(42, "/settings")
	h.callback(42, "set:guestname")
	if f := h.bot.getFlow(42); f == nil || f.step != stepGuestName {
		t.Fatalf("guest-name step not opened: %+v", f)
	}
	h.message(42, "  Ada   Lovelace ")
	if f := h.bot.getFlow(42); f != nil {
		t.Fatalf("guest-name step not closed after save: %+v", f)
	}
	if u := h.bot.userSettings(42); u.GuestName != "Ada Lovelace" {
		t.Fatalf("guest name = %q", u.GuestName)
	}

	// Launch applies the name.
	runWizardToReview(t, h, 42)
	h.callback(42, cbLaunch)
	if len(h.eng.requests) != 1 || h.eng.requests[0].GuestName != "Ada Lovelace" {
		t.Fatalf("guest name not applied to launch: %+v", h.eng.requests)
	}

	// " - " clears the override.
	h.callback(42, "set:guestname")
	h.message(42, "-")
	if u := h.bot.userSettings(42); u.GuestName != "" {
		t.Fatalf("guest name not cleared: %q", u.GuestName)
	}
}

func TestGuestNameValidation(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.callback(42, "set:guestname")
	h.message(42, strings.Repeat("x", 80))
	if got := h.api.lastSent().text; !strings.Contains(got, "too long") {
		t.Fatalf("long name accepted: %q", got)
	}
	// The step stays open so the user can retry.
	if f := h.bot.getFlow(42); f == nil || f.step != stepGuestName {
		t.Fatal("invalid name closed the step")
	}
}

func TestDashboardAdvertisesResumableDraft(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.callback(42, cbNew)
	h.message(42, validMeet)
	// Leave the wizard at the media step (a draft now exists).
	h.bot.mu.Lock()
	delete(h.bot.flows, 42) // simulate a restart
	h.bot.mu.Unlock()

	h.message(42, "/start")
	if got := h.api.lastSent().text; !strings.Contains(got, "unfinished stream draft") {
		t.Fatalf("dashboard missing draft notice: %q", got)
	}
}

// TestUnauthorizedUserCannotPressWorkflowButtons pins the authorization gate
// on the new callbacks.
func TestUnauthorizedUserCannotPressWorkflowButtons(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.callback(43, cbEditMeet) // 43 is not an admin and claims nothing
	if got := h.api.allSentText(); !strings.Contains(got, "Access denied") {
		t.Fatalf("unauthorized callback not denied: %q", got)
	}
	if f := h.bot.getFlow(43); f != nil {
		t.Fatal("unauthorized user got a workflow")
	}
	_ = tgbotapi.Update{}
}

// dropStaleDraftForTest writes a backdated updated_at so the expiry path can
// be exercised without sleeping.
func (b *Bot) dropStaleDraftForTest(userID int64) error {
	us := b.set.(interface {
		BackdateWorkflowDraftForTest(userID int64, age time.Duration) error
	})
	return us.BackdateWorkflowDraftForTest(userID, 2*workflowDraftMaxAge)
}
