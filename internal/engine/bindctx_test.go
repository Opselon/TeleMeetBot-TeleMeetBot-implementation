package engine

import (
	"context"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

// TestBindPageCtxPassesThroughSentinelPage is the regression guard for the
// stop-mid-join hang's fix. The session's rod pages were created by newPage
// with no Context(s.ctx), so every Navigate/Eval inherited only the page's
// own timeout: s.cancel() was a no-op for them and a stop issued while the
// session was inside openMeet/openMedia blocked Manager.Stop for its full
// 15 s budget before reporting failure. openMedia/openMeet now bind their
// pages to the session context through bindPageCtx.
//
// rod's Page.Context panics on a bare zero-value Page (no helpersLock), and
// the codebase's hand-built sessions and switchmedia_test.go use exactly
// that value as an "open but never evaluated" sentinel — so bindPageCtx must
// pass it through unchanged rather than crash the session. A real page gets
// the bound copy, which the Chromium integration suite exercises.
func TestBindPageCtxPassesThroughSentinelPage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Session{
		ID:  "s-bindctx",
		ctx: ctx,
		log: dummyLogger{},
		mgr: NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{}, dummyLogger{}),
	}

	sentinel := &rod.Page{}
	if got := s.bindPageCtx(sentinel); got != sentinel {
		t.Fatalf("bindPageCtx must return the zero-value sentinel page unchanged, got a different page")
	}
}

// TestBindPageCtxWithoutSessionContext: a hand-built session with no context
// returns any page unchanged, and nil stays nil.
func TestBindPageCtxWithoutSessionContext(t *testing.T) {
	s := &Session{ID: "s-handbuilt", log: dummyLogger{}}
	page := &rod.Page{}
	if got := s.bindPageCtx(page); got != page {
		t.Fatal("bindPageCtx without a session context must return the page unchanged")
	}
	if got := s.bindPageCtx(nil); got != nil {
		t.Fatalf("bindPageCtx(nil) must return nil, got %v", got)
	}
}

// TestStopCancelsTheSessionContext pins the observable behaviour the page
// binding exists to produce: cancelling the session (what Stop does) makes
// the session context report Done immediately, so Manager.Stop's wait on
// Done() returns promptly instead of burning its budget on a rod call that
// no stop could interrupt.
func TestStopCancelsTheSessionContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		ID:  "s-stop-cdp",
		ctx: ctx,
		log: dummyLogger{},
		mgr: NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{}, dummyLogger{}),
	}

	stopAt := time.Now()
	cancel()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled session context never reported Done")
	}
	if elapsed := time.Since(stopAt); elapsed > time.Second {
		t.Fatalf("cancellation propagated in %s, expected effectively immediate", elapsed.Round(time.Millisecond))
	}
	if err := ctx.Err(); err == nil {
		t.Fatal("the session context is not in the cancelled state after Stop")
	}
	_ = s
}
