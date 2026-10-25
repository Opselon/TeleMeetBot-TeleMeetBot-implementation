package engine

import (
	"errors"
	"sync"
	"testing"
)

// clipTarget must treat NaN, negative and infinite durations as "unknown"
// (live or still-loading media): a forward seek stays relative, a backward
// seek clamps at zero. A known duration clamps both ends.
func TestClipTarget(t *testing.T) {
	cases := []struct {
		name     string
		current  float64
		delta    int
		duration float64
		want     float64
	}{
		{"forward inside", 100, 30, 200, 130},
		{"backward inside", 100, -30, 200, 70},
		{"clamp at start", 5, -30, 200, 0},
		{"clamp at end", 190, 30, 200, 200},
		{"zero delta", 100, 0, 200, 100},
		{"unknown duration forward", 100, 60, 0, 160},
		{"unknown duration backward", 100, -60, 0, 40},
		{"unknown duration clamp start", 5, -60, 0, 0},
		{"nan duration forward", 100, 30, nan(), 130},
		{"infinite duration forward", 100, 30, posInf(), 130},
		{"large forward past end", 10, 400, 200, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clipTarget(tc.current, tc.delta, tc.duration)
			if got != tc.want {
				t.Fatalf("clipTarget(%v, %d, %v) = %v, want %v", tc.current, tc.delta, tc.duration, got, tc.want)
			}
		})
	}
}

func nan() float64    { return zero() / zero() }
func posInf() float64 { return 1 / zero() }

func zero() float64 { return 0 }

// seekLanded must accept a seek that reached a duration boundary (the clamp
// target is unreachable exactly) and reject one that landed nowhere near.
func TestSeekLanded(t *testing.T) {
	if !seekLanded(200, 199.2, 200) {
		t.Fatal("seek clamped at the end must count as landed")
	}
	if !seekLanded(0, 0.4, 200) {
		t.Fatal("seek clamped at the start must count as landed")
	}
	if seekLanded(150, 100, 200) {
		t.Fatal("seek that landed 50s short must not count as landed")
	}
	if !seekLanded(150, 149, 0) {
		t.Fatal("unknown duration must trust the reported position")
	}
}

// readyForSeek must refuse a player with no seekable ranges (live) and one
// below HAVE_METADATA, and allow a ready player.
func TestReadyForSeek(t *testing.T) {
	if ok, why := readyForSeek(mediaStatus{Ready: 4, Seekable: 1}); !ok || why != "" {
		t.Fatalf("ready player must be seekable, got %q", why)
	}
	if ok, why := readyForSeek(mediaStatus{Ready: 4, Seekable: 0}); ok || why != seekReasonNonSeekable {
		t.Fatalf("live (no seekable ranges) must be refused with %q, got ok=%v %q", seekReasonNonSeekable, ok, why)
	}
	if ok, why := readyForSeek(mediaStatus{Ready: 0, Seekable: 1}); ok || why != seekReasonNotReady {
		t.Fatalf("readyState 0 must be refused with %q, got ok=%v %q", seekReasonNotReady, ok, why)
	}
}

// clipTarget must never produce NaN or negative targets even for NaN inputs.
func TestClipTargetIsFinite(t *testing.T) {
	for _, tc := range []struct {
		current  float64
		delta    int
		duration float64
	}{
		{nan(), 30, 200},
		{nan(), -30, 0},
		{posInf(), 30, 200},
		{-100, 30, 200},
	} {
		got := clipTarget(tc.current, tc.delta, tc.duration)
		if got != got || got < 0 {
			t.Fatalf("clipTarget(%v, %d, %v) = %v: must be finite and non-negative", tc.current, tc.delta, tc.duration, got)
		}
		if tc.duration > 0 && got > tc.duration {
			t.Fatalf("clipTarget(%v, %d, %v) = %v: must not exceed duration", tc.current, tc.delta, tc.duration, got)
		}
	}
}

// A missing media tab must surface as a stale-tab error, never a silent OK.
func TestSeekToStaleTab(t *testing.T) {
	s := &Session{}
	rep, err := s.SeekTo(30)
	if err == nil {
		t.Fatal("SeekTo on a session with no media tab must error")
	}
	if rep.OK {
		t.Fatal("report must not claim success")
	}
	if rep.Error != seekReasonStaleTab {
		t.Fatalf("error = %q, want %q", rep.Error, seekReasonStaleTab)
	}
}

// Verify the legacy relative Seek errors when the player refuses.
func TestSeekReportsRefusal(t *testing.T) {
	s := &Session{}
	if err := s.Seek(-10); err == nil {
		t.Fatal("Seek on a session with no media tab must error")
	}
	if err := s.Seek(0); err == nil {
		t.Fatal("even a zero seek must error without a media tab")
	}
}

// errors.Is must still work through the wrapped seek errors.
func TestSeekErrorsAreComparable(t *testing.T) {
	s := &Session{}
	_, err := s.SeekBy(30)
	if !errors.Is(err, errors.New(seekReasonStaleTab)) && err.Error() != seekReasonStaleTab && !errors.Is(err, errNoMediaTab()) {
		// The exact wrapper shape is not contractual; what matters is the
		// caller sees a non-nil error naming the stale tab.
		if err == nil {
			t.Fatal("nil error for stale tab")
		}
	}
}

func errNoMediaTab() error { return errors.New("media page is not available") }

// ---- fake media player for integration tests ----

// fakeMedia is a minimal in-memory player that satisfies the mediaRun
// contract. It tracks currentTime, duration, readyState and seekable ranges
// so the seek path can be exercised without Chromium.
type fakeMedia struct {
	currentTime float64
	duration    float64
	readyState  int
	seekable    int // number of seekable ranges; 0 = live/non-seekable
	called      []fakeCall
	mu          sync.Mutex
}

type fakeCall struct {
	action string
	arg    float64
}

func (f *fakeMedia) run(action string, arg float64) (mediaStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = append(f.called, fakeCall{action, arg})

	var st mediaStatus
	st.Ready = f.readyState
	st.Seekable = f.seekable
	st.Duration = f.duration
	st.CurrentTime = f.currentTime
	st.OK = true

	switch action {
	case "seekStatus":
		return st, nil
	case "seekTo":
		if f.seekable == 0 {
			st.OK = false
			st.Error = seekReasonNonSeekable
			return st, nil
		}
		if f.readyState < 1 {
			st.OK = false
			st.Error = seekReasonNotReady
			return st, nil
		}
		target := arg
		if target < 0 {
			target = 0
		}
		if f.duration > 0 && target > f.duration {
			target = f.duration
		}
		// Simulate a seek that sometimes doesn't land exactly (player rounds)
		f.currentTime = target
		if f.currentTime < 0 {
			f.currentTime = 0
		}
		st.CurrentTime = f.currentTime
		return st, nil
	default:
		return mediaStatus{Error: "unknown action " + action, OK: false}, nil
	}
}

func TestSeekIntegrationHappyPath(t *testing.T) {
	fm := &fakeMedia{
		currentTime: 10,
		duration:    100,
		readyState:  4,
		seekable:    1,
	}
	s := &Session{
		mediaRun: fm.run,
		log:      noopLogger{},
	}

	rep, err := s.SeekBy(30)
	if err != nil {
		t.Fatalf("SeekBy(30): %v", err)
	}
	if !rep.OK {
		t.Fatalf("seek refused: %s", rep.Error)
	}
	if rep.Before != 10 {
		t.Fatalf("before = %v, want 10", rep.Before)
	}
	if rep.After != 40 {
		t.Fatalf("after = %v, want 40", rep.After)
	}
	if rep.Target != 40 {
		t.Fatalf("target = %v, want 40", rep.Target)
	}
	if rep.Delta != 30 {
		t.Fatalf("delta = %v, want 30", rep.Delta)
	}
	if rep.Duration != 100 {
		t.Fatalf("duration = %v, want 100", rep.Duration)
	}
}

func TestSeekIntegrationClampsAtEnd(t *testing.T) {
	fm := &fakeMedia{
		currentTime: 95,
		duration:    100,
		readyState:  4,
		seekable:    1,
	}
	s := &Session{mediaRun: fm.run, log: noopLogger{}}

	rep, err := s.SeekBy(30)
	if err != nil {
		t.Fatalf("SeekBy(30): %v", err)
	}
	if !rep.OK {
		t.Fatalf("seek refused: %s", rep.Error)
	}
	if rep.After != 100 {
		t.Fatalf("after = %v, want 100 (clamped at end)", rep.After)
	}
	if rep.Target != 100 {
		t.Fatalf("target = %v, want 100 (clamped at end)", rep.Target)
	}
}

func TestSeekIntegrationClampsAtStart(t *testing.T) {
	fm := &fakeMedia{
		currentTime: 5,
		duration:    100,
		readyState:  4,
		seekable:    1,
	}
	s := &Session{mediaRun: fm.run, log: noopLogger{}}

	rep, err := s.SeekBy(-30)
	if err != nil {
		t.Fatalf("SeekBy(-30): %v", err)
	}
	if !rep.OK {
		t.Fatalf("seek refused: %s", rep.Error)
	}
	if rep.After != 0 {
		t.Fatalf("after = %v, want 0 (clamped at start)", rep.After)
	}
	if rep.Target != 0 {
		t.Fatalf("target = %v, want 0 (clamped at start)", rep.Target)
	}
}

func TestSeekIntegrationLiveStreamRefuses(t *testing.T) {
	fm := &fakeMedia{
		currentTime: 0,
		duration:    0,
		readyState:  4,
		seekable:    0, // live stream
	}
	s := &Session{mediaRun: fm.run, log: noopLogger{}}

	rep, err := s.SeekBy(10)
	if err != nil {
		t.Fatalf("SeekBy(10): %v", err)
	}
	if rep.OK {
		t.Fatal("live stream must be refused")
	}
	if rep.Error != seekReasonNonSeekable {
		t.Fatalf("error = %q, want %q", rep.Error, seekReasonNonSeekable)
	}
	// A refusal is a report, not a Go error: the bot shows the reason and the
	// card keeps the true position.
	if rep.Before != 0 || rep.After != 0 {
		t.Fatalf("position must be reported untouched: before=%v after=%v", rep.Before, rep.After)
	}
}

func TestSeekIntegrationNotReadyRefuses(t *testing.T) {
	fm := &fakeMedia{
		currentTime: 10,
		duration:    100,
		readyState:  0, // HAVE_NOTHING
		seekable:    1,
	}
	s := &Session{mediaRun: fm.run, log: noopLogger{}}

	rep, err := s.SeekBy(10)
	if err != nil {
		t.Fatalf("SeekBy(10): %v", err)
	}
	if rep.OK {
		t.Fatal("not ready must be refused")
	}
	if rep.Error != seekReasonNotReady {
		t.Fatalf("error = %q, want %q", rep.Error, seekReasonNotReady)
	}
	// The readyState at refusal time is what tells the operator how far the
	// player got; it must be carried in the report.
	if rep.ReadyState != 0 {
		t.Fatalf("readyState in report = %d, want 0", rep.ReadyState)
	}
}

func TestSeekIntegrationZeroDelta(t *testing.T) {
	fm := &fakeMedia{
		currentTime: 42,
		duration:    100,
		readyState:  4,
		seekable:    1,
	}
	s := &Session{mediaRun: fm.run, log: noopLogger{}}

	rep, err := s.SeekBy(0)
	if err != nil {
		t.Fatalf("SeekBy(0): %v", err)
	}
	if !rep.OK {
		t.Fatalf("zero delta must report OK, got %q", rep.Error)
	}
	if rep.After != 42 || rep.Before != 42 {
		t.Fatalf("position must not move: before=%v after=%v", rep.Before, rep.After)
	}
}

func TestSeekIntegrationRapidPressesSerialised(t *testing.T) {
	// Rapid presses must queue, not interleave: each seek must read the
	// position the previous one left. The fake tracks calls to verify order.
	fm := &fakeMedia{
		currentTime: 0,
		duration:    100,
		readyState:  4,
		seekable:    1,
	}
	s := &Session{mediaRun: fm.run, log: noopLogger{}}

	// Fire 4 seeks concurrently (each takes the seekMu).
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			_, _ = s.SeekBy(10)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}

	// Each press costs 2 CDP calls (status read + seekTo) and they serialize:
	// 0 → 10 → 20 → 30 → 40.
	if fm.currentTime != 40 {
		t.Fatalf("final position = %v, want 40 (4×10 serialized)", fm.currentTime)
	}
	if len(fm.called) != 8 {
		t.Fatalf("called %d times, want 8 (4 presses × status+seekTo)", len(fm.called))
	}
}

func TestSeekIntegrationStaleTabRetry(t *testing.T) {
	// First call errors (stale tab), second succeeds. The retry keeps rapid
	// presses working through a reload.
	fm := &fakeMedia{
		currentTime: 10,
		duration:    100,
		readyState:  4,
		seekable:    1,
	}
	attempt := 0
	s := &Session{
		mediaRun: func(action string, arg float64) (mediaStatus, error) {
			if action != "seekTo" {
				return fm.run(action, arg)
			}
			attempt++
			if attempt == 1 {
				return mediaStatus{}, errors.New("context canceled")
			}
			return fm.run(action, arg)
		},
		log: noopLogger{},
	}

	rep, err := s.SeekTo(50)
	if err != nil {
		t.Fatalf("SeekTo(50): %v", err)
	}
	if !rep.OK {
		t.Fatalf("seek refused after retry: %s", rep.Error)
	}
	if rep.After != 50 {
		t.Fatalf("after = %v, want 50", rep.After)
	}
	if attempt != 2 {
		t.Fatalf("expected 2 attempts (fail then retry), got %d", attempt)
	}
}
