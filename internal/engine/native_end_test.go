package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWatchLoopEndsNativeMediaGate is the regression test for the watch loop's
// end-of-media gate. The gate was `st.Ended && !st.Paused`, and
// HTMLMediaElement sets paused=true the moment playback ends (the element is
// parked at its last frame), so the advance was suppressed for every native
// or downloaded file — auto-advance and repeat-one silently never fired on
// the direct path, leaving the meeting on a frozen last frame. Only YouTube
// worked, because its probe forces ended=true from getPlayerState()===0 while
// the element still reports paused=false.
//
// The gate lives inline in watch(), so the regression is pinned against the
// source: the poll that drives the queue decision must fire on st.Ended with
// no !st.Paused qualifier. The companion test proves the decision still moves
// the cursor for both media semantics.
func TestWatchLoopEndsNativeMediaGate(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("session.go"))
	if err != nil {
		t.Fatalf("read session.go: %v", err)
	}
	// Locate the end-of-media poll the watch loop evaluates each tick.
	needle := "if st.Ended {"
	i := strings.Index(string(src), needle)
	if i < 0 {
		t.Fatalf("the end-of-media gate `%s` is missing from session.go", needle)
	}
	// The historical, suppressed-native-media form must not come back.
	reintroduced := strings.Contains(string(src), "if st.Ended && !st.Paused {")
	if reintroduced {
		t.Errorf("session.go re-introduced `st.Ended && !st.Paused`: native/downloaded media ends with paused=true, so auto-advance and repeat-one would silently stop firing on the direct path again")
	}
}

// TestNativeEndOfStreamAdvancesTheQueue proves the queue decision still fires
// for a native end-of-stream sample — the case the historical gate suppressed.
func TestNativeEndOfStreamAdvancesTheQueue(t *testing.T) {
	s := newQueueSessionWithPage(t, 3)
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error {
		return nil
	})

	_, before := s.QueueSnapshot()
	if got := s.handleQueueEnded(); got != queueEndNext {
		t.Fatalf("native end-of-stream action = %v, want queueEndNext", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, cur := s.QueueSnapshot()
		if cur != before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("native end-of-stream never advanced the queue (cursor still %d)", before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
