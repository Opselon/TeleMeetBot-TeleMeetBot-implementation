package engine

import (
	"context"
	"testing"
	"time"
)

// TestManagerStopWaitsForCleanup is the regression test for the s.done
// lifecycle: for the whole life of the engine, cleanup() was the only exit
// path of run() and it never closed s.done. Manager.Stop and Manager.StopAll
// both block on Done() with a bounded deadline, so every stop reported
// "session %s did not stop in time" and shed duplicate notifications long
// after the session goroutine had finished.
//
// This test drives the production cleanup() through Manager.Stop's wait,
// without needing a real browser: a manager-registered session whose context
// is cancelled by the stop itself, with cleanup run by a stand-in for the
// run() goroutine.
func TestManagerStopWaitsForCleanup(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{
		GuestName: "Test Bot",
	}, dummyLogger{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Session{
		ID:       "s-stop-done",
		MeetURL:  "https://meet.google.com/xyz-uvwx-rst",
		MediaURL: "https://example.com/audio.mp3",
		state:    StateStreaming,
		created:  time.Now(),
		log:      dummyLogger{},
		mgr:      mgr,
	}
	// Wire ctx/cancel/done the way Manager.Start does. The stop path needs
	// a working cancel; done stays open until cleanup() closes it.
	s.ctx, s.cancel = ctx, cancel
	s.done = make(chan struct{})

	mgr.mu.Lock()
	mgr.sessions[s.ID] = s
	mgr.mu.Unlock()

	// Stand-in for the run() goroutine: it finishes when the session context
	// is cancelled and runs the production cleanup on the way out.
	go func() {
		<-ctx.Done()
		s.cleanup()
	}()

	// Manager.Stop cancels the context and then waits on Done() for up to
	// 15s. Before the fix this always hit the deadline and Stop returned
	// "session %s did not stop in time"; now cleanup() closes done and Stop
	// returns nil.
	if err := mgr.Stop(s.ID); err != nil {
		t.Fatalf("Manager.Stop: %v", err)
	}

	select {
	case <-s.Done():
	default:
		t.Fatalf("Done() is not closed after cleanup")
	}

	if st := s.Snapshot().State; !IsTerminalState(st) {
		t.Fatalf("state after stop is %q, want terminal", st)
	}
}

// TestManagerStopMissingSession keeps the not-found contract honest.
func TestManagerStopMissingSession(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{}, dummyLogger{})
	if err := mgr.Stop("s-does-not-exist"); err == nil {
		t.Fatalf("expected error for missing session, got nil")
	}
}
