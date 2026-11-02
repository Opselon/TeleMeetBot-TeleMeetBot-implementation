package engine

import (
	"context"
	"testing"
	"time"

	movideo "github.com/Opselon/TeleMeetBot/internal/media"
)

// TestRunRecoversPanicInAcquire is the regression test for the engine
// goroutine's panic safety. run() had no recovery: a panic inside the session
// goroutine was swallowed by the goroutine, the deferred cleanup() still
// released the resources but the session stayed in whatever non-terminal
// state it was in, its meeting's dedup slot stayed occupied, and nothing ever
// told the operator, the bot card or the dashboard what had happened.
//
// The panic is injected at a stage run() actually drives (browser
// acquisition, through its test seam) so the recovery is exercised against
// the genuine state machine rather than a mock of it.
func TestRunRecoversPanicInAcquire(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{
		GuestName: "Test Bot",
	}, dummyLogger{})
	events := make(chan Event, 16)
	mgr.events = events

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Session{
		ID:        "s-panic",
		MeetURL:   "https://meet.google.com/xyz-uvwx-rst",
		MediaURL:  "https://example.com/audio.mp3",
		MediaKind: MediaDirect,
		state:     StateStarting,
		created:   time.Now(),
		log:       dummyLogger{},
		mgr:       mgr,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		acquireRun: func() (func(), error) {
			panic("boom: simulated engine panic in browser acquisition")
		},
	}

	mgr.mu.Lock()
	mgr.sessions[s.ID] = s
	mgr.mu.Unlock()

	go s.run()

	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("run() with a panicking component never completed: Done() not closed")
	}

	// The panic must surface as a terminal error, not a silent death.
	st := s.Snapshot()
	if !IsTerminalState(st.State) {
		t.Fatalf("state after panic = %q, want terminal", st.State)
	}
	if st.Error == "" {
		t.Fatalf("panic produced no error message on the snapshot")
	}

	// The error must be emitted so the bot card and dashboard can show it.
	var sawError bool
	for {
		select {
		case ev := <-events:
			if ev.Kind == EventError && ev.Session == s.ID {
				sawError = true
			}
		default:
			if !sawError {
				t.Fatalf("no EventError emitted for the recovered panic")
			}
			return
		}
	}
}

// TestRunRecoversPanicInPrepare covers the Download & Stream stage: the same
// recovery must hold for a panic raised while the media is being fetched,
// which in production runs after browser acquisition.
func TestRunRecoversPanicInPrepare(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{
		GuestName: "Test Bot",
	}, dummyLogger{})
	events := make(chan Event, 16)
	mgr.events = events

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Session{
		ID:        "s-panic-prepare",
		MeetURL:   "https://meet.google.com/xyz-uvwx-rst",
		MediaURL:  "https://example.com/audio.mp3",
		MediaKind: MediaDirect,
		state:     StateStarting,
		created:   time.Now(),
		log:       dummyLogger{},
		mgr:       mgr,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		playMode:  movideo.ModeDownload,
		acquireRun: func() (func(), error) {
			return func() {}, nil // browser "acquired": nothing to release
		},
		prepareRun: func() error {
			panic("boom: simulated engine panic in the download stage")
		},
	}

	mgr.mu.Lock()
	mgr.sessions[s.ID] = s
	mgr.mu.Unlock()

	go s.run()

	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("run() with a panicking download never completed: Done() not closed")
	}

	st := s.Snapshot()
	if !IsTerminalState(st.State) {
		t.Fatalf("state after panic = %q, want terminal", st.State)
	}
	if st.Error == "" {
		t.Fatalf("panic produced no error message on the snapshot")
	}

	var sawError bool
	for {
		select {
		case ev := <-events:
			if ev.Kind == EventError && ev.Session == s.ID {
				sawError = true
			}
		default:
			if !sawError {
				t.Fatalf("no EventError emitted for the recovered panic")
			}
			return
		}
	}
}
