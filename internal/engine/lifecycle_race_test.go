package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// Concurrency requirements of the cancellation contract: every hard
// timing in the spec, each driven deterministically against the REAL
// session lifecycle (a real manager, a real lifespan context, the real
// Stop), not a mock of the function under test.

// TestStopImmediatelyAfterStart: a Stop issued the instant a session
// exists must not race the session goroutine's first step. The session
// must still reach a clean terminal state and close done.
func TestStopImmediatelyAfterStart(t *testing.T) {
	dir := t.TempDir()
	s, _ := reproYTDLPSession(t, dir, "early")
	// Stop before the fetch even gets a slot: the lifespan is cancelled
	// first, so the download stage observes a dead context on entry.
	s.Stop()
	select {
	case <-s.Done():
	case <-time.After(stopGracePeriod + 2*time.Second):
		t.Fatal("a stop issued immediately after creation never completed")
	}
	st := s.Snapshot()
	if !IsTerminalState(st.State) {
		t.Fatalf("state = %q after stop, want a terminal state", st.State)
	}
}

// TestStopWhileInitializing: Stop landing while the session is still in
// its STARTING stage must still produce a terminated session, not one
// stuck in a half-built state.
func TestStopWhileInitializing(t *testing.T) {
	dir := t.TempDir()
	s, _ := reproYTDLPSession(t, dir, "init")
	go func() { _ = s.prepareLocalMedia() }()
	time.Sleep(20 * time.Millisecond) // mid-initialize
	s.Stop()
	select {
	case <-s.Done():
	case <-time.After(stopGracePeriod + 2*time.Second):
		t.Fatal("a stop during initialization never completed")
	}
}

// TestStopFollowedByNewSession: stopping one session and immediately
// starting another must work — the stopped session must not leave the
// manager's single download concurrency slot occupied (the orphan-
// worker bug's second symptom: the zombie held the gate hostage).
func TestStopFollowedByNewSession(t *testing.T) {
	dir := t.TempDir()
	mgr, err := mediateam.NewManager(mediateam.Options{
		Enabled: true, Dir: dir, YTDLPPath: "yt-dlp",
		MaxBytes: 64 << 20, Timeout: 30 * time.Second, TTL: time.Hour, MaxConcurrent: 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	s1 := newLifecycleSession(t, dir, "slot-1", mgr, release)
	go func() { _ = s1.prepareLocalMedia() }()
	<-s1.started
	s1.Stop()
	<-s1.done

	// The slot must be free: a new session's download starts promptly.
	s2 := newLifecycleSession(t, dir, "slot-2", mgr, release)
	go func() { _ = s2.prepareLocalMedia() }()
	select {
	case <-s2.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the new session's download never started — the stopped session left the concurrency slot occupied")
	}
	close(release)
	<-s2.done
}

// TestTwoStopsArrivingSimultaneously is the idempotency requirement:
// two concurrent Stop calls must both succeed and must not corrupt the
// session's state (double-close of done would panic).
func TestTwoStopsArrivingSimultaneously(t *testing.T) {
	dir := t.TempDir()
	s, _ := reproYTDLPSession(t, dir, "double")
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Stop() // must not panic on the second call
		}()
	}
	wg.Wait()
	select {
	case <-s.Done():
	default:
		t.Fatal("concurrent stops left the session unterminated")
	}
}

// TestStopRacingDownloadCompletion: a Stop arriving at exactly the
// moment the download completes must not deadlock and must not report
// a spurious failure — the race must resolve to "stopped" cleanly.
func TestStopRacingDownloadCompletion(t *testing.T) {
	dir := t.TempDir()
	mgr, err := mediateam.NewManager(mediateam.Options{
		Enabled: true, Dir: dir, YTDLPPath: "yt-dlp",
		MaxBytes: 64 << 20, Timeout: 30 * time.Second, TTL: time.Hour, MaxConcurrent: 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	s := newLifecycleSession(t, dir, "race", mgr, release)
	go func() { _ = s.prepareLocalMedia() }()
	<-s.started
	// Let the download complete and stop at (approximately) the same
	// instant: whichever wins, the session must end up terminal.
	close(release)
	s.Stop()
	select {
	case <-s.Done():
	case <-time.After(stopGracePeriod + 2*time.Second):
		t.Fatal("a stop racing download completion never resolved")
	}
	st := s.Snapshot()
	if !IsTerminalState(st.State) {
		t.Fatalf("state = %q, want terminal", st.State)
	}
}

// ---- harness ----------------------------------------------------------

// lifecycleSession is a Download-mode session backed by a fake yt-dlp
// that blocks on the release channel, so a test can hold a fetch open
// or let it finish at an exact moment.
type lifecycleSession struct {
	*Session
	started chan struct{}
	done    chan error
	release chan struct{}
	once    sync.Once
}

func newLifecycleSession(t *testing.T, dir, id string, mgr *mediateam.Manager, release <-chan struct{}) *lifecycleSession {
	t.Helper()
	rf := &lifecycleSession{started: make(chan struct{}), done: make(chan error, 1)}
	s := &Session{
		ID:        SessionID(id),
		MediaURL:  "https://www.youtube.com/watch?v=" + id,
		MediaKind: MediaYouTube,
		playMode:  mediateam.ModeDownload,
		mgr:       &Manager{mediaMgr: mgr},
		log:       noopLogger{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel, s.done = ctx, cancel, make(chan struct{})
	mgr.SetYTDLPFnForTest(func(ctx context.Context, args []string) ([]byte, []byte, error) {
		rf.once.Do(func() { close(rf.started) })
		select {
		case <-ctx.Done():
			rf.done <- ctx.Err()
			return nil, nil, fmt.Errorf("cancelled: %w", ctx.Err())
		case <-release:
			rf.done <- nil
			return []byte{}, nil, nil
		}
	})
	rf.Session = s
	return rf
}
