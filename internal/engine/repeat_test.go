package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/go-rod/rod"
)

// TestDecideQueueEnd pins every branch of the end-of-media decision.
func TestDecideQueueEnd(t *testing.T) {
	cases := []struct {
		name     string
		autoplay bool
		repeat   string
		cursor   int
		length   int
		want     queueEndAction
	}{
		{"autoplay off stops", false, RepeatOff, 0, 3, queueEndStop},
		{"empty queue stops", true, RepeatAll, -1, 0, queueEndStop},
		{"off advances", true, RepeatOff, 0, 3, queueEndNext},
		{"off stops at end", true, RepeatOff, 2, 3, queueEndStop},
		{"one restarts", true, RepeatOne, 0, 3, queueEndRestart},
		{"one restarts single item", true, RepeatOne, 0, 1, queueEndRestart},
		{"all advances", true, RepeatAll, 0, 3, queueEndNext},
		{"all wraps at end", true, RepeatAll, 2, 3, queueEndWrap},
		{"all wraps single item", true, RepeatAll, 0, 1, queueEndWrap},
		{"unknown repeat behaves as off", true, "sometimes", 0, 3, queueEndNext},
		{"unknown repeat stops at end", true, "sometimes", 2, 3, queueEndStop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideQueueEnd(tc.autoplay, tc.repeat, tc.cursor, tc.length)
			if got != tc.want {
				t.Fatalf("decideQueueEnd(%v,%q,%d,%d) = %v, want %v",
					tc.autoplay, tc.repeat, tc.cursor, tc.length, got, tc.want)
			}
		})
	}
}

// TestNormalizeRepeatMode checks the session-level fence.
func TestNormalizeRepeatMode(t *testing.T) {
	for raw, want := range map[string]string{
		"off": RepeatOff, "": RepeatOff, "bogus": RepeatOff,
		"one": RepeatOne, "all": RepeatAll,
	} {
		if got := NormalizeRepeatMode(raw); got != want {
			t.Fatalf("NormalizeRepeatMode(%q) = %q, want %q", raw, got, want)
		}
	}
}

// newQueueSession builds a bare session: enough for decision tests, but
// SwitchMedia reports "media page is not available" without a page.
func newQueueSession(t *testing.T, n int) *Session {
	t.Helper()
	return newQueueSessionOpts(t, n, false)
}

// newQueueSessionWithPage adds the zero-value media page that lets
// SwitchMedia pass its availability check and reach the switchRun test seam.
func newQueueSessionWithPage(t *testing.T, n int) *Session {
	t.Helper()
	return newQueueSessionOpts(t, n, true)
}

func newQueueSessionOpts(t *testing.T, n int, withPage bool) *Session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Session{ID: "s-repeat", log: dummyLogger{}, ctx: ctx}
	if withPage {
		s.mediaPage = &rod.Page{}
	}
	extra := make([]string, n-1)
	for i := range extra {
		extra[i] = fmt.Sprintf("https://example.com/t%02d.mp4", i+2)
	}
	s.InitQueue("https://example.com/t01.mp4", extra)
	return s
}

// TestRepeatTogglesRoundTrip exercises the setters and the Status projection.
func TestRepeatTogglesRoundTrip(t *testing.T) {
	s := newQueueSession(t, 3)

	if got := s.RepeatMode(); got != RepeatOff {
		t.Fatalf("initial repeat = %q, want off", got)
	}
	if err := s.SetRepeatMode("all"); err != nil {
		t.Fatalf("set repeat: %v", err)
	}
	s.SetShuffle(true)
	s.SetAutoAdvance(false)
	st := s.Snapshot()
	if st.RepeatMode != RepeatAll || !st.Shuffle || st.AutoAdvance {
		t.Fatalf("status = repeat %q shuffle %v auto %v", st.RepeatMode, st.Shuffle, st.AutoAdvance)
	}
	// An invalid mode falls back to off rather than poisoning the session.
	if err := s.SetRepeatMode("sometimes"); err != nil {
		t.Fatalf("set invalid repeat: %v", err)
	}
	if got := s.RepeatMode(); got != RepeatOff {
		t.Fatalf("invalid repeat = %q, want off", got)
	}
}

// TestRepeatOneRestartsCurrentItem drives the end-of-media path and checks
// the decision plus the queue cursor: under repeat-one the item restarts and
// the cursor never moves. RestartMedia drives the real browser tab (not the
// switch seam), so only the decision and the unchanged queue are asserted.
func TestRepeatOneRestartsCurrentItem(t *testing.T) {
	s := newQueueSession(t, 3)
	s.SetRepeatMode(RepeatOne)

	if got := s.handleQueueEnded(); got != queueEndRestart {
		t.Fatalf("action = %v, want restart", got)
	}
	_, cur := s.QueueSnapshot()
	if cur != 0 {
		t.Fatalf("cursor moved under repeat-one: %d", cur)
	}
}

// TestRepeatAllWrapsAtQueueEnd drives the wrap branch: the last item ends and
// the first item plays again.
func TestRepeatAllWrapsAtQueueEnd(t *testing.T) {
	s := newQueueSessionWithPage(t, 2)
	s.SetRepeatMode(RepeatAll)
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error {
		return nil
	})
	if err := s.Next(); err != nil { // move to the last item
		t.Fatalf("next: %v", err)
	}
	if got := s.handleQueueEnded(); got != queueEndWrap {
		t.Fatalf("action = %v, want wrap", got)
	}
	waitFor(t, func() bool {
		_, cur := s.QueueSnapshot()
		return cur == 0
	}, "queue did not wrap to item 0")
}

// TestAutoAdvanceOffStops pins the autoplay toggle: nothing advances when it
// is off, even with repeat-all set (autoplay wins).
func TestAutoAdvanceOffStops(t *testing.T) {
	s := newQueueSession(t, 3)
	s.SetAutoAdvance(false)
	s.SetRepeatMode(RepeatAll)
	if got := s.handleQueueEnded(); got != queueEndStop {
		t.Fatalf("action = %v, want stop", got)
	}
}

// TestShuffleAdvancePicksFromUpcomingOnly verifies that a shuffled advance
// never leaves the upcoming range and updates the cursor to the played item.
func TestShuffleAdvancePicksFromUpcomingOnly(t *testing.T) {
	s := newQueueSessionWithPage(t, 8)
	s.SetShuffle(true)
	s.rng = rand.New(rand.NewSource(7))
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error {
		return nil
	})

	successes := 0
	for {
		_, before := s.QueueSnapshot()
		err := s.advanceShuffled()
		if errors.Is(err, ErrQueueEnd) {
			break // the tail was exhausted; the boundary is honest
		}
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		successes++
		items, cur := s.QueueSnapshot()
		if cur <= before || cur >= len(items) {
			t.Fatalf("success %d: cursor %d not in upcoming (%d..%d)", successes, cur, before+1, len(items)-1)
		}
	}
	if successes == 0 {
		t.Fatal("shuffle never advanced")
	}
	// Once exhausted, further advances keep reporting the boundary instead
	// of corrupting state.
	if err := s.advanceShuffled(); !errors.Is(err, ErrQueueEnd) {
		t.Fatalf("exhausted shuffle = %v, want ErrQueueEnd", err)
	}
}

// TestShuffleDeterministicWithSeed pins the rng contract: the same seed
// produces the same sequence, so shuffle behaviour is reproducible in tests.
func TestShuffleDeterministicWithSeed(t *testing.T) {
	s1 := newQueueSession(t, 10)
	s2 := newQueueSession(t, 10)
	s1.rng = rand.New(rand.NewSource(1234))
	s2.rng = rand.New(rand.NewSource(1234))

	for i := 0; i < 5; i++ {
		a := s1.pickShuffledNext(0, 10, s1.rng)
		b := s2.pickShuffledNext(0, 10, s2.rng)
		if a != b {
			t.Fatalf("seeded picks diverged: %d vs %d at round %d", a, b, i)
		}
		if a < 1 || a >= 10 {
			t.Fatalf("pick %d outside upcoming range", a)
		}
	}
}

// TestHandleQueueEndedShuffleAdvances covers the watch-loop path with
// shuffle on: the action is next, and playback lands on an upcoming item.
func TestHandleQueueEndedShuffleAdvances(t *testing.T) {
	s := newQueueSessionWithPage(t, 5)
	s.SetShuffle(true)
	s.rng = rand.New(rand.NewSource(42))
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error {
		return nil
	})
	if got := s.handleQueueEnded(); got != queueEndNext {
		t.Fatalf("action = %v, want next", got)
	}
	waitFor(t, func() bool {
		_, cur := s.QueueSnapshot()
		return cur > 0
	}, "shuffle advance did not move the cursor")
}

// TestManagerStartAppliesPerUserOverrides checks that StartRequest playback
// settings reach the session (volume, speed, repeat, shuffle, autoplay).
func TestManagerStartAppliesPerUserOverrides(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{DefaultVolume: 80, DefaultSpeed: 1.0}, MeetOptions{}, dummyLogger{})
	mgr.maxSessions = 8
	auto := false
	s, err := mgr.Start(context.Background(), StartRequest{
		MeetLink:   "https://meet.google.com/abc-defg-hij",
		MediaLink:  "https://example.com/a.mp4",
		Volume:     30,
		Speed:      1.75,
		RepeatMode: "all",
		Shuffle:    true,
		AutoPlay:   &auto,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)
	if s.playerOpts.Volume != 30 || s.playerOpts.Speed != 1.75 {
		t.Fatalf("player opts = %+v, want volume 30 speed 1.75", s.playerOpts)
	}
	if s.RepeatMode() != RepeatAll || !s.Shuffle() || s.AutoAdvance() {
		t.Fatalf("session flags = repeat %q shuffle %v auto %v", s.RepeatMode(), s.Shuffle(), s.AutoAdvance())
	}

	// Defaults hold when the request does not override them. A different
	// meeting keeps the single-flight dedup out of the way.
	s2, err := mgr.Start(context.Background(), StartRequest{
		MeetLink:  "https://meet.google.com/def-ghi-jkl",
		MediaLink: "https://example.com/b.mp4",
	})
	if err != nil {
		t.Fatalf("start default: %v", err)
	}
	t.Cleanup(s2.Stop)
	if s2.playerOpts.Volume != 80 || s2.playerOpts.Speed != 1.0 {
		t.Fatalf("default player opts = %+v", s2.playerOpts)
	}
	if s2.RepeatMode() != RepeatOff || s2.Shuffle() || !s2.AutoAdvance() {
		t.Fatalf("default flags = repeat %q shuffle %v auto %v", s2.RepeatMode(), s2.Shuffle(), s2.AutoAdvance())
	}
}

// TestRepeatDoesNotChangeQueueOnPlayFailure pins the "no success on failure"
// rule: when the media switch fails, Next must report the error and leave
// the cursor pointing at the item that could not play (so a retry is
// possible), not silently skip ahead.
func TestRepeatDoesNotChangeQueueOnPlayFailure(t *testing.T) {
	s := newQueueSessionWithPage(t, 2)
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error {
		return errors.New("browser gone")
	})
	err := s.Next()
	if err == nil {
		t.Fatal("Next succeeded despite a failing switch")
	}
	_, cur := s.QueueSnapshot()
	if cur != 1 {
		t.Fatalf("cursor = %d after failed Next, want 1 (the attempted item)", cur)
	}
}

// waitFor spins until cond is true or the deadline passes.
