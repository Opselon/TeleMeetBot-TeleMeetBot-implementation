package bot

import (
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// TestRefreshCardRateLimitsEdits is the regression for the dashboard edit storm:
// a chatty state machine (the host-gate wait polled every ~800ms) used to edit
// the same Telegram message dozens of times a minute, burning CPU on TLS
// handshakes and hitting Telegram's rate limit. Card edits must be spaced by at
// least minCardEditInterval, and the pending text must still land afterwards.
func TestRefreshCardRateLimitsEdits(t *testing.T) {
	api := &fakeAPI{}
	b := &Bot{
		api:   api,
		log:   noopLogger{},
		cards: map[string]*cardRef{},
		flows: map[int64]*flow{},
		menus: map[int64]int{},
	}
	ref := &cardRef{chat: 1, msg: 10, text: "initial"}
	b.cards["s-1"] = ref

	// First edit goes through immediately.
	b.refreshCard(ref, engine.SessionID("s-1"), "text v2", nil)
	if got := len(api.edits); got != 1 {
		t.Fatalf("first edit: got %d edits, want 1", got)
	}

	// 20 rapid updates inside the floor window must be coalesced into none.
	for i := 0; i < 20; i++ {
		b.refreshCard(ref, engine.SessionID("s-1"), "text v3", nil)
	}
	if got := len(api.edits); got != 1 {
		t.Fatalf("rapid updates produced %d edits, want 0 extra (rate limited)", got-1)
	}

	// After the floor elapses the newest text is delivered (nothing was dropped).
	time.Sleep(minCardEditInterval + 50*time.Millisecond)
	b.refreshCard(ref, engine.SessionID("s-1"), "text v3", nil)
	if got := len(api.edits); got != 2 {
		t.Fatalf("deferred edit never arrived: %d edits, want 2", got)
	}
	if last := api.lastEdit(); last.text != "text v3" {
		t.Fatalf("deferred edit carried %q, want the latest text", last.text)
	}

	// Identical text is still free.
	b.refreshCard(ref, engine.SessionID("s-1"), "text v3", nil)
	if got := len(api.edits); got != 2 {
		t.Fatalf("unchanged text triggered an edit: %d", got)
	}
}

// TestRefreshCardRetriesAfterFailedEdit pins that a transient Telegram failure
// does not leave the card permanently stale: the text is only recorded once the
// edit succeeded.
func TestRefreshCardRetriesAfterFailedEdit(t *testing.T) {
	api := &fakeAPI{failEdits: 1}
	b := &Bot{
		api:   api,
		log:   noopLogger{},
		cards: map[string]*cardRef{},
		flows: map[int64]*flow{},
		menus: map[int64]int{},
	}
	ref := &cardRef{chat: 1, msg: 11, text: "initial"}
	b.cards["s-2"] = ref

	b.refreshCard(ref, engine.SessionID("s-2"), "after failure", nil)
	if ref.text != "initial" {
		t.Fatalf("card text recorded despite a failed edit: %q", ref.text)
	}

	// Next attempt (after the rate-limit floor) must retry the same text.
	time.Sleep(minCardEditInterval + 50*time.Millisecond)
	b.refreshCard(ref, engine.SessionID("s-2"), "after failure", nil)
	if ref.text != "after failure" {
		t.Fatalf("retry did not land, text = %q", ref.text)
	}
	if got := len(api.edits); got != 2 {
		t.Fatalf("expected a retry attempt, got %d edits", got)
	}
}
