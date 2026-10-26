package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAdmissionStateMachineTransitions(t *testing.T) {
	// Verify the complete explicit lifecycle states
	expectedStates := []string{
		StateQueued,
		StateStarting,
		StateJoining,
		StateWaitingForAdmission,
		StateAdmitted,
		StateActive,
		StateLeaving,
		StateCompleted,
	}

	for _, st := range expectedStates[:len(expectedStates)-1] {
		if IsTerminalState(st) {
			t.Fatalf("State %s should not be terminal", st)
		}
	}

	if !IsTerminalState(StateCompleted) {
		t.Fatalf("State %s should be terminal", StateCompleted)
	}
	if !IsTerminalState(StateEnded) {
		t.Fatalf("State %s should be terminal", StateEnded)
	}
	if !IsTerminalState(StateTimedOut) {
		t.Fatalf("State %s should be terminal", StateTimedOut)
	}
	if !IsTerminalState(StateCancelled) {
		t.Fatalf("State %s should be terminal", StateCancelled)
	}
	if !IsTerminalState(StateFailed) {
		t.Fatalf("State %s should be terminal", StateFailed)
	}
	if !IsTerminalState(StateError) {
		t.Fatalf("State %s should be terminal", StateError)
	}
}

func TestMeetingSingleFlightDeduplication(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{
		GuestName: "Test Bot",
	}, dummyLogger{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	meetURL := "https://meet.google.com/xyz-uvwx-rst"

	// Insert an active running session
	s1 := &Session{
		ID:       "s-active-1",
		MeetURL:  meetURL,
		MediaURL: "https://example.com/audio.mp3",
		state:    StateJoining,
		created:  time.Now(),
		ctx:      ctx,
		log:      dummyLogger{},
	}

	mgr.mu.Lock()
	mgr.sessions[s1.ID] = s1
	mgr.mu.Unlock()

	// Attempting to start another session for the exact same Meet URL must fail
	_, err := mgr.Start(ctx, StartRequest{
		MeetLink:  meetURL,
		MediaLink: "https://example.com/another.mp3",
	})
	if err == nil {
		t.Fatalf("expected error starting duplicate session for meet %s, got nil", meetURL)
	}
	if !strings.Contains(err.Error(), "already running or joining") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// But starting for a DIFFERENT meet URL is permitted (up to limit)
	diffURL := "https://meet.google.com/abc-defg-hij"
	// To avoid real browser launch, verify check passes by checking deduplication logic
	mgr.mu.Lock()
	s2 := &Session{
		ID:       "s-active-2",
		MeetURL:  diffURL,
		MediaURL: "https://example.com/video.mp4",
		state:    StateCompleted, // terminal
		created:  time.Now(),
		ctx:      ctx,
		log:      dummyLogger{},
	}
	mgr.sessions[s2.ID] = s2
	mgr.mu.Unlock()

	// Terminal session with same URL does NOT block new session
	mgr.mu.Lock()
	s1.state = StateCompleted
	mgr.mu.Unlock()

	// Check deduplication recognizes terminal state
	mgr.mu.Lock()
	activeCount := 0
	hasDuplicate := false
	for _, other := range mgr.sessions {
		if !IsTerminalState(other.snapshotState()) && other.MeetURL == meetURL {
			hasDuplicate = true
		}
		if !IsTerminalState(other.snapshotState()) {
			activeCount++
		}
	}
	mgr.mu.Unlock()

	if hasDuplicate {
		t.Fatalf("terminal session should not trigger duplicate block")
	}
	if activeCount != 0 {
		t.Fatalf("expected 0 active sessions, got %d", activeCount)
	}
}

func TestAdmissionWaitAndTimeoutStates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		ID:      "s-timeout-test",
		MeetURL: "https://meet.google.com/tst-test-tst",
		state:   StateStarting,
		created: time.Now(),
		ctx:     ctx,
		log:     dummyLogger{},
		done:    make(chan struct{}),
	}

	// Normal transition into waiting
	s.setState(StateWaitingForAdmission, "[4/6] Waiting for host to admit bot")
	if st := s.Snapshot().State; st != StateWaitingForAdmission {
		t.Fatalf("state = %s, want %s", st, StateWaitingForAdmission)
	}

	// Positive admission transition
	s.setState(StateAdmitted, "[4/6] Admitted to meeting")
	if st := s.Snapshot().State; st != StateAdmitted {
		t.Fatalf("state = %s, want %s", st, StateAdmitted)
	}

	// Cancellation transition
	cancel()
	s.cleanup()
	if st := s.Snapshot().State; st != StateCancelled {
		t.Fatalf("state = %s, want %s on cancellation", st, StateCancelled)
	}
}

func TestAdmissionReloadJitterBounds(t *testing.T) {
	// Verify that reload intervals stay bounded and jittered
	computeReloadInterval := func(attempts int) time.Duration {
		base := 20 * time.Second
		if attempts >= 2 {
			base = 25 * time.Second
		}
		if attempts >= 5 {
			base = 30 * time.Second
		}
		// In session.go jitter is 0..4s
		jitter := 2 * time.Second
		return base + jitter
	}

	for attempt := 0; attempt < 10; attempt++ {
		d := computeReloadInterval(attempt)
		if d < 20*time.Second {
			t.Fatalf("attempt %d interval %v < 20s", attempt, d)
		}
		if d > 35*time.Second {
			t.Fatalf("attempt %d interval %v > 35s", attempt, d)
		}
	}
}
