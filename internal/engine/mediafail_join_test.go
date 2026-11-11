package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// The media leg failing while the join is still in flight is the production
// failure class that used to end meetings before Google's admission gate
// ever lifted: YouTube bot-checks routinely fail the browser media leg at
// [2/6] — 35-40 s into a join that needs the host to click Admit — and the
// old fatal-first path cancelled the whole session, freezing the card on
// "waiting_for_admission" before the terminal verdict degraded into
// "cancelled". These tests pin the new contract:
//
//   - admitted wins: a media failure during the join never ends an admitted
//     session (the meeting stays live, the media failure is recorded as a
//     phase, the standard recovery owns it after admission)
//   - the join verdict wins: when the join also fails, its classified
//     terminal state (timed_out for join timeouts, error otherwise) is what
//     the session reports — never a straggler-overwritten "cancelled".

// mediaFailFixture builds a session with the standard leg seams and the given
// open/join/present/play behaviours.
func mediaFailFixture(t *testing.T, openErr, joinErr error, joinDelay time.Duration) *Session {
	t.Helper()
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{GuestName: "Test Bot"}, dummyLogger{})
	events := make(chan Event, 16)
	mgr.events = events

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s := &Session{
		ID:        SessionID("s-mediafail-" + t.Name()),
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
		prepareRun: func() error { return nil },
		openRun: func() error {
			return openErr
		},
		joinRun: func() error {
			time.Sleep(joinDelay)
			return joinErr
		},
		presentRun: func() error { return nil },
		playRun:    func() error { return nil },
	}

	mgr.mu.Lock()
	mgr.sessions[s.ID] = s
	mgr.mu.Unlock()
	return s
}

// waitForState polls until the session reaches one of the wanted states (or
// the done channel closes) and returns the final snapshot.
func waitForState(t *testing.T, s *Session, want map[string]bool, timeout time.Duration) Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last Status
	for time.Now().Before(deadline) {
		last = s.Snapshot()
		if want[last.State] {
			return last
		}
		select {
		case <-s.done:
			return s.Snapshot()
		default:
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("session never reached a wanted state %v; last state %q detail %q error %q", want, last.State, last.Detail, last.Error)
	return last
}

// TestMediaFailureDuringJoinKeepsMeeting proves a media failure at [2/6]
// cannot end a session whose join is still in flight: after the (delayed)
// join succeeds, the session must reach streaming — presenting and playing
// with the failed media recorded as a phase — never a terminal error.
func TestMediaFailureDuringJoinKeepsMeeting(t *testing.T) {
	s := mediaFailFixture(t, errors.New("video element never buffered within 35s: LOGIN_REQUIRED"), nil, 300*time.Millisecond)
	go s.run()

	st := waitForState(t, s, map[string]bool{StateStreaming: true}, 6*time.Second)
	if st.State != StateStreaming {
		t.Fatalf("state = %q, want streaming", st.State)
	}
	// The media failure must be recorded honestly in the phase history: the
	// card can show what happened to the media even though the meeting
	// survived. (The live phase reads "browser" here because the play seam
	// verifies playback — the same "playback recovered" reset the after-share
	// path applies.)
	failedSeen := false
	for _, tr := range st.PhaseHistory {
		if tr.Phase == PhaseFailed {
			failedSeen = true
			break
		}
	}
	if !failedSeen {
		t.Fatalf("phase history = %+v, want a failed entry recording the media failure", st.PhaseHistory)
	}
	s.Stop()
}

// TestMediaFailureWithFailedJoinRecordsMediaVerdict proves the both-failed
// ordering keeps the media story as the terminal verdict, symmetric with the
// meet-first branch, and — critically — that the terminal state is never the
// straggler-degraded "cancelled" the old quietMeet ordering produced.
func TestMediaFailureWithFailedJoinRecordsMediaVerdict(t *testing.T) {
	s := mediaFailFixture(t, errors.New("video element never buffered within 35s: LOGIN_REQUIRED"), errors.New("meet join timed out after 2m0s (stage=blocked)"), 150*time.Millisecond)
	go s.run()

	st := waitForState(t, s, map[string]bool{StateError: true, StateTimedOut: true, StateCancelled: true}, 6*time.Second)
	if st.State != StateError {
		t.Fatalf("state = %q, want error with the media verdict; detail %q", st.State, st.Detail)
	}
	if !strings.Contains(st.Error, "video element never buffered") {
		t.Fatalf("error = %q, want the media story", st.Error)
	}
	<-s.done // cleanup must complete
}

// TestMediaFailureWithFailedJoinKeepsHonestError proves a non-timeout join
// failure does not corrupt the terminal state either.
func TestMediaFailureWithFailedJoinKeepsHonestError(t *testing.T) {
	s := mediaFailFixture(t, errors.New("video element never buffered"), errors.New("this meeting requires a signed-in Google account"), 150*time.Millisecond)
	go s.run()

	st := waitForState(t, s, map[string]bool{StateError: true, StateCancelled: true}, 6*time.Second)
	if st.State != StateError {
		t.Fatalf("state = %q, want error, not a straggler-degraded cancelled; detail %q", st.State, st.Detail)
	}
	<-s.done
}

// TestJoinTimeoutWithoutMediaFailureRecordsTimedOut pins the failWithState
// fix directly: a join timeout with a healthy media leg reports timed_out,
// not the generic error that setError forced.
func TestJoinTimeoutWithoutMediaFailureRecordsTimedOut(t *testing.T) {
	s := mediaFailFixture(t, nil, errors.New("timed out waiting 5m0s for the meeting to accept guests"), 0)
	go s.run()

	st := waitForState(t, s, map[string]bool{StateTimedOut: true, StateError: true, StateCancelled: true}, 6*time.Second)
	if st.State != StateTimedOut {
		t.Fatalf("state = %q, want timed_out", st.State)
	}
	<-s.done
}
