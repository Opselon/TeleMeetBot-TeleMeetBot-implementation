package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/userstate"
)

// TestLaunchDoesNotSelfDeadlockOnBotMutex is the regression test for the
// "bot freezes when I press the launch button" defect.
//
// Root cause: bot.launch() held b.mu while calling b.trackLive(), which
// itself takes b.mu (via liveSessionIDs / liveSessionCount). That recursive
// lock attempt self-deadlocked the worker goroutine handling the update —
// and because the update is dispatched on a bounded worker pool, every
// further Telegram update for that chat queued up behind the dead worker
// and the bot appeared hung. The live stack trace of the production
// container showed three goroutines parked in semacquire1 on the same
// mutex, two of them inside launch() (flows.go:559 getFlow and
// flows.go:614 trackLive).
//
// This test reproduces the failure deterministically: it takes b.mu from a
// background goroutine and then drives the launch callback. With the bug,
// the callback blocks forever inside trackLive waiting for the mutex the
// caller never released; with the fix, launch() releases the mutex before
// calling trackLive and the callback returns.
func TestLaunchDoesNotSelfDeadlockOnBotMutex(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	// The tracker is what makes trackLive take b.mu for real: without it
	// trackLive returns early and the deadlock would hide. Production wires
	// it via WithUserTracker; mirror that here so the test exercises the
	// real code path.
	h.bot.track = userstate.New(nil)

	// Drive the wizard to the confirmation card, exactly like a real user.
	h.message(42, "/new")
	h.message(42, "meet.google.com/abc-defg-hij")
	h.message(42, "https://example.com/clip.mp4")
	if !strings.Contains(h.api.lastSent().text, "Ready to launch") {
		t.Fatalf("wizard did not reach the confirmation card: %q", h.api.lastSent().text)
	}

	// Hold b.mu from another goroutine, simulating any concurrent holder
	// (refresh loop, event handler). launch() must not require the mutex
	// to be *free* for its whole duration — only for its own critical
	// sections. The bug made launch() block indefinitely here.
	released := make(chan struct{})
	go func() {
		defer close(released)
		h.bot.mu.Lock()
		// Keep the lock long enough that a self-deadlocked launch() would
		// visibly hang, then release so the test can finish cleanly.
		time.Sleep(150 * time.Millisecond)
		h.bot.mu.Unlock()
	}()

	// Give the background goroutine a moment to acquire the lock first.
	time.Sleep(50 * time.Millisecond)

	// The freeze symptom: pressing 🚀 Confirm & Start must return.
	done := make(chan struct{})
	go func() {
		h.callback(42, cbLaunch)
		close(done)
	}()
	select {
	case <-done:
		// Success: the launch callback completed even while another
		// goroutine held b.mu.
	case <-time.After(10 * time.Second):
		t.Fatal("launch callback hung: bot.launch() self-deadlocked on b.mu " +
			"(trackLive called while b.mu was held)")
	}
	<-released

	// The session must actually exist and be live-tracked: the launch path
	// must still bind the chat before returning.
	if len(h.eng.requests) != 1 {
		t.Fatalf("expected one Start request, got %d", len(h.eng.requests))
	}
	h.bot.mu.Lock()
	chats := len(h.bot.sessionChats)
	h.bot.mu.Unlock()
	if chats != 1 {
		t.Fatalf("session chat binding missing after launch: %d", chats)
	}
}
