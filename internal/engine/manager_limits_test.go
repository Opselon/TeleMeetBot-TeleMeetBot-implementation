package engine

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestManagerEnforcesMaxSessions verifies the engine-level concurrent session
// limit. Even if the caller bypasses the dashboard check, Start must reject
// any request past limits.max_sessions.
func TestManagerEnforcesMaxSessions(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{}, noopLogger{})
	mgr.SetMaxSessions(2)

	// Inject two "running" sessions without starting a browser.
	for i := 1; i <= 2; i++ {
		id := SessionID(fmt.Sprintf("s-%d", i))
		s := &Session{
			ID:      id,
			state:   StateStreaming,
			created: time.Now(),
			mgr:     mgr,
		}
		s.ctx, s.cancel = context.WithCancel(context.Background())
		s.done = make(chan struct{})
		mgr.sessions[id] = s
	}

	_, err := mgr.Start(context.Background(), StartRequest{
		MeetLink:  "https://meet.google.com/abc-defg-hij",
		MediaLink: "https://example.com/test.mp4",
	})
	if err == nil {
		t.Fatal("Start past max_sessions should be refused, got nil")
	}
	want := "session limit reached: 2 of 2 already running (limits.max_sessions)"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestManagerPrunesFinishedSessions bounds memory growth: ended sessions must
// not accumulate unbounded in the manager map for the lifetime of the process.
func TestManagerPrunesFinishedSessions(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{}, noopLogger{})
	mgr.SetMaxSessions(100)

	// Seed more finished sessions than maxFinishedSessions (10).
	base := time.Now().Add(-1 * time.Hour)
	for i := 1; i <= 25; i++ {
		id := SessionID(fmt.Sprintf("s-old-%02d", i))
		mgr.sessions[id] = &Session{
			ID:      id,
			state:   StateEnded,
			created: base.Add(time.Duration(i) * time.Minute),
			mgr:     mgr,
		}
	}

	mgr.mu.Lock()
	mgr.pruneFinishedLocked()
	mgr.mu.Unlock()

	mgr.mu.RLock()
	n := len(mgr.sessions)
	mgr.mu.RUnlock()

	if n != maxFinishedSessions {
		t.Fatalf("finished sessions retained: %d, want exactly %d", n, maxFinishedSessions)
	}

	// Verify the retained ones are the newest ones (s-old-16 .. s-old-25).
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	for i := 16; i <= 25; i++ {
		id := SessionID(fmt.Sprintf("s-old-%02d", i))
		if _, ok := mgr.sessions[id]; !ok {
			t.Fatalf("expected newer session %s to be kept", id)
		}
	}
	for i := 1; i <= 15; i++ {
		id := SessionID(fmt.Sprintf("s-old-%02d", i))
		if _, ok := mgr.sessions[id]; ok {
			t.Fatalf("expected older session %s to be pruned", id)
		}
	}
}
