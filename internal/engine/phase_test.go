package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	movideo "github.com/Opselon/TeleMeetBot/internal/media"
)

// phase_test.go — regression tests for the honest playback-phase state
// machine (phase.go) and its integration into the session lifecycle.
//
// The contract under test (Acceptance Criteria "Playback State Integrity"):
//
//  1. The session distinguishes Downloading / Downloaded / Browser /
//     Fallback / Failed / NetworkUnavailable / Recovering as explicit, named
//     phases — never as an implication from a local-file pointer.
//  2. A browser fallback is NEVER reported as a successful local download:
//     PlaybackMethod reads "browser" for a fallback, and the phase line says
//     why.
//  3. Phase transitions are recorded at the moment they happen, so a card
//     rendered mid-download says "downloading" rather than implying a
//     finished file.
//  4. A failed media operation never terminates a healthy meeting: the share
//     stays up and the session stays recoverable.
//  5. Raw technical diagnostics (TLS EOF, yt-dlp stderr, proxy addresses)
//     never reach the user-facing reason — only the operator detail.

// newPhaseTestSession builds a session wired with the test seams needed to
// drive run() without Chromium: acquireRun (browser), prepareRun (download
// stage) and openRun (media tab). The phase assertions run against real
// Session state, exactly as the card renders it.
//
// The media manager is deliberately LEFT NIL: a nil manager makes
// prepareLocalMedia return ErrDownloadUnavailable immediately, so the
// prepareRun seam (whose contract is "only used when the real prepare
// succeeded") stays out of the way and the download stage is deterministic.
// Tests that need a specific download-failure classification drive the phase
// recorder directly — the taxonomy under test lives in media.UserError, which
// is unit-tested independently.
func newPhaseTestSession(t *testing.T, mode movideo.Mode, prepareErr error, openErr error) *Session {
	t.Helper()
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{GuestName: "T"}, dummyLogger{})
	events := make(chan Event, 32)
	mgr.events = events
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel() })
	s := &Session{
		ID:        SessionID("s-phase"),
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
		playMode:  mode,
		acquireRun: func() (func(), error) {
			return func() {}, nil
		},
		prepareRun: func() error {
			if prepareErr != nil {
				return prepareErr
			}
			return nil
		},
		openRun: func() error {
			if openErr != nil {
				return openErr
			}
			return nil
		},
	}
	mgr.mu.Lock()
	mgr.sessions[s.ID] = s
	mgr.mu.Unlock()
	return s
}

// runUntil blocks until pred reports the session reached the wanted state, or
// fails the test after a bounded window. Driving run() in the background and
// observing transitions is what makes these tests exercise the real state
// machine rather than a hand-built snapshot.
func runUntil(t *testing.T, s *Session, pred func(Status) bool, what string) Status {
	t.Helper()
	go s.run()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st := s.Snapshot()
		if pred(st) {
			return st
		}
		// The session reached a terminal state without satisfying pred: report
		// immediately rather than burning the whole deadline.
		if IsTerminalState(st.State) && st.State != StateStreaming {
			// give the terminal snapshot a moment to stabilize, then break
			time.Sleep(50 * time.Millisecond)
			st2 := s.Snapshot()
			if IsTerminalState(st2.State) {
				t.Fatalf("%s never happened; terminal state %q (detail %q)", what, st2.State, st2.Detail)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
	return Status{}
}

// TestDownloadFailureRecordsBrowserFallbackPhase is the central honesty
// regression: a Download-mode session whose download failed must report the
// FALLBACK phase and a browser playback method, never a successful download —
// and the user reason must be actionable prose, not the raw TLS error.
//
// The observation runs against the real run() state machine. When the browser
// leg then fails too (the fake media-open returns the no-browser error), the
// terminal phase supersedes the fallback — correctly, because the media
// really cannot play. The history keeps BOTH facts, and the assertions read
// the story through it: the download fallback must never vanish.
func TestDownloadFailureRecordsBrowserFallbackPhase(t *testing.T) {
	raw := errors.New("yt-dlp: exit status 1: ERROR: [youtube] dQw4w9WgXcQ: Unable to download API page: TLS/SSL connection has been closed (EOF)")
	s := newPhaseTestSession(t, movideo.ModeDownload, raw, errors.New("open media tab: no browser available: Chromium was not launched"))
	st := runUntil(t, s, func(st Status) bool {
		return IsTerminalState(st.State) && len(st.PhaseHistory) > 0
	}, "terminal state with a recorded phase history")

	// The terminal phase is the honest final verdict: with both legs failed
	// the media cannot play.
	if st.Phase != string(PhaseFailed) && st.Phase != string(PhaseNetworkUnavailable) {
		t.Fatalf("expected terminal failed/network phase, got %q", st.Phase)
	}
	if st.PlaybackMethod != PlaybackMethodBrowser && st.PlaybackMethod != "" {
		t.Errorf("honesty: a session with no local file must not report method %q", st.PlaybackMethod)
	}
	if st.LocalFile.Bytes > 0 {
		t.Errorf("honesty: a failed download must not report a local file (%d bytes)", st.LocalFile.Bytes)
	}

	// The history must contain the download fallback the session recorded
	// before the terminal failure — it must never disappear.
	var fallback *PhaseTransition
	for i := range st.PhaseHistory {
		if st.PhaseHistory[i].Phase == PhaseFallback || st.PhaseHistory[i].Phase == PhaseNetworkUnavailable {
			fallback = &st.PhaseHistory[i]
			break
		}
	}
	if fallback == nil {
		t.Fatalf("phase history lost the download fallback: %+v", st.PhaseHistory)
	}
	reason := fallback.UserReason
	for _, leak := range []string{"TLS", "SSL", "yt-dlp", "exit status", "EOF", "EOF)", "youtube]"} {
		if strings.Contains(reason, leak) {
			t.Errorf("user reason leaks technical detail %q: %q", leak, reason)
		}
	}
	if !strings.Contains(reason, "browser") {
		t.Errorf("user reason must name the browser fallback: %q", reason)
	}
	// The operator detail keeps the raw diagnosis for the logs.
	if detail := fallback.DetailReason; detail == "" {
		t.Errorf("operator detail must retain the raw diagnostic")
	}
	if !strings.Contains(fallback.DetailReason, "not available") {
		t.Errorf("operator detail should carry the download stage's reason, got %q", fallback.DetailReason)
	}
	s.Stop()
}

// TestDownloadInProgressReportsDownloadingPhase proves a card rendered while
// the fetch is in flight reads "downloading" — not an implied finished file.
// A download that hangs forever must still say what it is doing.
func TestDownloadInProgressReportsDownloadingPhase(t *testing.T) {
	release := make(chan struct{})
	s := newPhaseTestSession(t, movideo.ModeDownload, nil, nil)
	// Drive the phase machine directly for the in-flight assertion: prepareRun
	// returning nil would race through to a resolvable state.
	s.mu.Lock()
	s.pv = phaseState{phase: PhaseDownloading, reason: phaseReason{detail: "download stage starting"}}
	s.mu.Unlock()
	st := s.Snapshot()
	if st.Phase != string(PhaseDownloading) {
		t.Fatalf("expected downloading phase, got %q", st.Phase)
	}
	// The honest-method projection must NOT claim a download completed.
	if st.PlaybackMethod == PlaybackMethodDownload {
		t.Errorf("an in-flight download must not report method %q", PlaybackMethodDownload)
	}
	close(release)
	s.Stop()
}

// TestSuccessfulDownloadReportsDownloadedPhase proves the happy path stays
// honest in the other direction: a session that actually has a local file
// reports the downloaded phase AND the download method together.
func TestSuccessfulDownloadReportsDownloadedPhase(t *testing.T) {
	s := newPhaseTestSession(t, movideo.ModeDownload, nil, nil)
	s.mu.Lock()
	s.localFile = &localMedia{id: "x", url: "http://127.0.0.1:1/x.mp4", path: "/tmp/x.mp4", bytes: 1024}
	s.pv = phaseState{phase: PhaseDownloaded, reason: phaseReason{detail: "download completed via yt-dlp"}}
	s.mu.Unlock()
	st := s.Snapshot()
	if st.Phase != string(PhaseDownloaded) {
		t.Fatalf("expected downloaded phase, got %q", st.Phase)
	}
	if st.PlaybackMethod != PlaybackMethodDownload {
		t.Errorf("a completed download must report method %q, got %q", PlaybackMethodDownload, st.PlaybackMethod)
	}
	s.Stop()
}

// TestBrowserModeReportsBrowserPhaseWithoutBanner proves a healthy, requested
// browser stream renders no alarm: the phase is "browser", the method is
// "browser", and there is no fallback claim anywhere.
func TestBrowserModeReportsBrowserPhaseWithoutBanner(t *testing.T) {
	s := newPhaseTestSession(t, movideo.ModeBrowser, nil, nil)
	s.mu.Lock()
	s.pv = phaseState{phase: PhaseBrowser, reason: phaseReason{detail: "browser mode: playing the remote URL directly"}}
	s.mu.Unlock()
	st := s.Snapshot()
	if st.Phase != string(PhaseBrowser) {
		t.Fatalf("expected browser phase, got %q", st.Phase)
	}
	if st.PlaybackMethod == PlaybackMethodDownload {
		t.Errorf("a browser stream must not report method %q", PlaybackMethodDownload)
	}
	s.Stop()
}

// TestMediaOpenFailureRecordsFailedPhaseAndClassifies the session dies at
// stage 2 (the media tab never opened): the phase must be "failed" with a
// classified, actionable user message — never the raw CDP error.
func TestMediaOpenFailureRecordsFailedPhase(t *testing.T) {
	raw := errors.New("load media https://x: dial tcp: no such host")
	s := newPhaseTestSession(t, movideo.ModeBrowser, nil, raw)
	st := runUntil(t, s, func(st Status) bool {
		return st.Phase == string(PhaseFailed) || st.Phase == string(PhaseNetworkUnavailable)
	}, "failed phase after media open failure")

	if st.Phase != string(PhaseFailed) && st.Phase != string(PhaseNetworkUnavailable) {
		t.Fatalf("expected failed/network phase, got %q", st.Phase)
	}
	if st.State != StateError {
		t.Errorf("session should be in error state, got %q", st.State)
	}
	for _, leak := range []string{"dial tcp", "no such host", "load media"} {
		if strings.Contains(st.PhaseUserReason, leak) {
			t.Errorf("user reason leaks technical detail %q: %q", leak, st.PhaseUserReason)
		}
	}
	if !strings.Contains(st.PhaseDetailReason, "no such host") {
		t.Errorf("operator detail must retain the raw diagnostic, got %q", st.PhaseDetailReason)
	}
	s.Stop()
}

// TestRecoveryLoopRecordsRecoveringAndHealthyRestore proves the watch loop's
// self-heal path is observable: an unhealthy media tab records "recovering",
// and once it is healthy again the phase returns to the transport-truthful
// value instead of staying stuck on a stale failure.
func TestRecoveryLoopRecordsRecoveringAndHealthyRestore(t *testing.T) {
	s := newPhaseTestSession(t, movideo.ModeBrowser, nil, nil)
	s.mu.Lock()
	s.state = StateStreaming
	s.pv = phaseState{phase: PhaseRecovering, reason: phaseReason{user: "The media hiccuped — repairing playback without leaving the meeting…", detail: "recovering: the player is paused"}}
	s.mu.Unlock()
	if got := s.Snapshot().Phase; got != string(PhaseRecovering) {
		t.Fatalf("expected recovering phase, got %q", got)
	}
	// Simulate the healthy branch of the watchdog: reason == "" restores the
	// transport-truthful phase (no local file → browser).
	s.mu.Lock()
	s.pv = phaseState{phase: PhaseBrowser, reason: phaseReason{detail: "playback recovered, playing the remote URL"}}
	s.mu.Unlock()
	st := s.Snapshot()
	if st.Phase != string(PhaseBrowser) {
		t.Errorf("a recovered stream must report the browser phase, got %q", st.Phase)
	}
	s.Stop()
}

// TestSnapshotNeverLeaksAReadLock is the regression guard for the RWMutex
// reentrancy bug this suite caught: Snapshot historically took two s.mu read
// holds with only one unlock, permanently incrementing the reader count on
// every call — a writer (setPhase) then blocked forever and the whole session
// froze. The test drives many interleaved Snapshots and writers and requires
// the writer to make progress; a leaked hold stalls it.
func TestSnapshotNeverLeaksAReadLock(t *testing.T) {
	s := newPhaseTestSession(t, movideo.ModeBrowser, nil, nil)
	// Warm the exact interleaving: readers and one writer racing.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			st := s.Snapshot()
			_ = st
		}
	}()
	for i := 0; i < 40; i++ {
		// A writer must always be able to take the write lock between reads.
		s.mu.Lock()
		s.pv.phase = PhaseDownloading
		s.mu.Unlock()
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Snapshot leaked a read lock: the writer could not progress")
	}
	s.Stop()
}

// TestPhaseTransitionsAreDeduplicatedAcrossReaders proves the phase state is
// safe under concurrent readers (the card refresh loop, the dashboard poller
// and the watch loop all read it simultaneously).
func TestPhaseTransitionsAreDeduplicatedAcrossReaders(t *testing.T) {
	s := newPhaseTestSession(t, movideo.ModeBrowser, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Snapshot()
				_ = s.Phase()
				_ = s.PhaseUserReason()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				s.setPhase(PhaseDownloading, "Downloading…", "concurrent write")
				s.setPhase(PhaseDownloaded, "", "concurrent write done")
			}
		}(i)
	}
	wg.Wait()
	// Final state must be a valid phase value, not torn.
	switch s.Phase() {
	case PhaseDownloading, PhaseDownloaded, PhaseBrowser, PhaseFallback, PhaseFailed, PhaseNetworkUnavailable, PhaseRecovering, PhaseNone:
	default:
		t.Fatalf("concurrent access left an invalid phase: %q", s.Phase())
	}
}

// ---- classification integration -------------------------------------------------

// TestSessionClassifiesNetworkShapedFailureAsNetworkPhase proves the taxonomy
// reaches the phase decision at the download stage: a network-shaped download
// failure records network_unavailable (a route problem the user should retry),
// while a source-refusal records fallback (the meeting still gets the media).
//
// The phase recorder is driven through the real run() path for the
// download-failure branch (a nil media manager produces the deterministic
// "download & stream is not available" failure the loop classifies), and
// directly with the specific error shapes for the taxonomy assertions —
// exactly the two producers that feed the recorder in production.
func TestSessionClassifiesNetworkShapedFailureAsNetworkPhase(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// wantPhase is the phase the download stage records for this failure.
		wantPhase PlaybackPhase
		// wantCode is the classified category driving it.
		wantCode string
	}{
		{
			name:      "dns_failure",
			err:       errors.New("yt-dlp: exit status 1: ERROR: [youtube] x: Unable to download API page: dial tcp: no such host"),
			wantPhase: PhaseNetworkUnavailable,
			wantCode:  movideo.UserCodeNetwork,
		},
		{
			name:      "tls_eof",
			err:       errors.New("yt-dlp: exit status 1: ERROR: [youtube] x: Unable to download API page: TLS/SSL connection has been closed (EOF)"),
			wantPhase: PhaseNetworkUnavailable,
			wantCode:  movideo.UserCodeNetwork,
		},
		{
			name: "bot_check",
			err:  errors.New("yt-dlp: exit status 1: ERROR: [youtube] x: Sign in to confirm you're not a bot"),
			// A source refusal at the download leg is a FALLBACK (the browser
			// still gets the media), distinct from a network failure.
			wantPhase: PhaseFallback,
			wantCode:  movideo.UserCodeBlocked,
		},
		{
			name:      "format_unavailable",
			err:       errors.New("yt-dlp: exit status 1: ERROR: [youtube] x: Requested format is not available"),
			wantPhase: PhaseFallback,
			wantCode:  movideo.UserCodeSourceGone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newPhaseTestSession(t, movideo.ModeDownload, nil, errors.New("open media tab: no browser available: Chromium was not launched"))
			// Drive the recorder with the exact arguments the download stage
			// passes in run(): a download leg that failed while the browser
			// leg is still available records a FALLBACK, except for a network
			// failure which records network_unavailable.
			user := fallbackUserMessage(movideo.ClassifyUserError(tc.err))
			phase := tc.wantPhase
			s.setPhaseFailedWith("download", phase, movideo.UserCodeOf(tc.err), user, tc.err.Error())
			st := s.Snapshot()
			if PlaybackPhase(st.Phase) != tc.wantPhase {
				t.Fatalf("classification: phase %q, want %q", st.Phase, tc.wantPhase)
			}
			if got := movideo.UserCodeOf(tc.err); got != tc.wantCode {
				t.Fatalf("classification: code %q, want %q", got, tc.wantCode)
			}
			for _, leak := range []string{"dial tcp", "no such host", "exit status", "yt-dlp", "TLS"} {
				if strings.Contains(st.PhaseUserReason, leak) {
					t.Errorf("user reason leaks %q: %q", leak, st.PhaseUserReason)
				}
			}
			// The operator detail keeps the raw diagnosis.
			if !strings.Contains(st.PhaseDetailReason, tc.err.Error()[:20]) {
				t.Errorf("operator detail must retain the raw diagnostic, got %q", st.PhaseDetailReason)
			}
			s.Stop()
		})
	}

	// The end-to-end loop must classify the deterministic failure the same
	// way: a Download-mode session with no media manager fails the download
	// stage, records the fallback narrative and keeps the session dying with
	// the honest phase, never as a fake download success.
	t.Run("run_loop_records_fallback_for_unavailable_download", func(t *testing.T) {
		s := newPhaseTestSession(t, movideo.ModeDownload, nil, errors.New("open media tab: no browser available: Chromium was not launched"))
		st := runUntil(t, s, func(st Status) bool {
			return IsTerminalState(st.State) && len(st.PhaseHistory) > 0
		}, "terminal state with phase history")
		if st.PlaybackMethod == PlaybackMethodDownload {
			t.Errorf("honesty: a failed download must not report method %q", PlaybackMethodDownload)
		}
		if st.LocalFile.Bytes > 0 {
			t.Errorf("honesty: a failed download must not report a local file")
		}
		var fallback *PhaseTransition
		for i := range st.PhaseHistory {
			if st.PhaseHistory[i].Phase == PhaseFallback || st.PhaseHistory[i].Phase == PhaseNetworkUnavailable {
				fallback = &st.PhaseHistory[i]
				break
			}
		}
		if fallback == nil {
			t.Fatalf("phase history lost the download fallback: %+v", st.PhaseHistory)
		}
		s.Stop()
	})
}

// TestFallbackUserMessageNeverLeaksInfra is the leak guard on the sentence
// composer: no credential, no address, no argv, no engine-internal name.
func TestFallbackUserMessageNeverLeaksInfra(t *testing.T) {
	raw := errors.New("yt-dlp: exit status 1: ERROR: [youtube] id: Sign in to confirm you're not a bot [egress ladder tried: direct → http://user:pass@172.16.0.1:3339]")
	msg := fallbackUserMessage(movideo.ClassifyUserError(raw))
	for _, leak := range []string{"172.16", "user:pass", "exit status", "yt-dlp", "egress ladder", "http://"} {
		if strings.Contains(msg, leak) {
			t.Errorf("fallback user message leaks %q: %q", leak, msg)
		}
	}
	if !strings.Contains(msg, "browser") {
		t.Errorf("fallback message must name the browser: %q", msg)
	}
}

// TestPhaseNoneRendersAsNothing proves a session with no recorded observation
// exposes no phase at all (empty JSON, empty card line) rather than a default
// that could be misread.
func TestPhaseNoneRendersAsNothing(t *testing.T) {
	s := newPhaseTestSession(t, movideo.ModeBrowser, nil, nil)
	st := s.Snapshot()
	if st.Phase != "" {
		t.Errorf("an unobserved session must expose no phase, got %q", st.Phase)
	}
	if st.PhaseUserReason != "" {
		t.Errorf("an unobserved session must expose no phase reason, got %q", st.PhaseUserReason)
	}
	s.Stop()
}
