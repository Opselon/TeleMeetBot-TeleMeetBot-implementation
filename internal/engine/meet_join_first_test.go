package engine

import (
	"context"
	"testing"
	"time"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// TestJoinFirstDoesNotAbortMediaLoad verifies that when the meet leg reports
// success (admitted to the meeting) before the media leg finishes loading, the
// session does NOT cancel itself as a fatal leg error. A nil meet verdict is
// success: the bot joined. run() must fall through to [5/6] Present and
// [6/6] Play rather than recording "session stopped during teardown" or a
// nil-pointer panic in handleMeetLegError.
//
// Regression for: a perfectly successful join was treated as a fatal leg error
// because meetErrCh carries nil on success, killing the session while the
// media tab was still buffering.
func TestJoinFirstDoesNotAbortMediaLoad(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{
		GuestName: "Test Bot",
	}, dummyLogger{})
	events := make(chan Event, 16)
	mgr.events = events

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The media leg is deliberately slower than the meet leg: prepareRun is
	// fast but openRun blocks until the test says so, proving that a
	// successful join does not tear the media leg down mid-load.
	s := &Session{
		ID:        "s-join-first",
		MeetURL:   "https://meet.google.com/xyz-uvwx-rst",
		MediaURL:  "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		MediaKind: MediaYouTube,
		state:     StateStarting,
		created:   time.Now(),
		log:       dummyLogger{},
		mgr:       mgr,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		playMode:  mediateam.ModeBrowser,
		acquireRun: func() (func(), error) {
			return func() {}, nil
		},
		prepareRun: func() error {
			return nil // download unnecessary in browser mode
		},
		openRun: func() error {
			return nil
		},
		joinRun: func() error {
			return nil // admitted immediately
		},
		presentRun: func() error { return nil },
		playRun:    func() error { return nil },
	}

	mgr.mu.Lock()
	mgr.sessions[s.ID] = s
	mgr.mu.Unlock()

	go s.run()

	// Meet success must not abort the session: the meet leg reports nil
	// while the media leg finishes loading. run() must fall through to
	// [5/6]/[6/6] and reach streaming, not record a terminal error.
	deadline := time.Now().Add(5 * time.Second)
	var streaming bool
	var last Status
	for time.Now().Before(deadline) {
		st := s.Snapshot()
		last = st
		if st.State == StateStreaming && st.Detail == "[6/6] Streaming live" {
			streaming = true
			break
		}
		if st.State == StateError {
			t.Fatalf("session died with terminal error: %q (detail %q)", st.Error, st.Detail)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !streaming {
		t.Fatalf("session never reached streaming; last state %q detail %q error %q",
			last.State, last.Detail, last.Error)
	}

	// A clean stop must not report the error-state fallout of the join leg.
	s.Stop()
	st := s.Snapshot()
	if st.State == StateError {
		t.Fatalf("stopped session reports terminal error: %q (detail %q)", st.Error, st.Detail)
	}
}
