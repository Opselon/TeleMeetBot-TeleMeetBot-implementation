package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// REPRODUCTION TESTS — written RED, before the fix, to prove the reported
// bug exists and to pin the corrected behaviour afterwards.
//
// ROOT CAUSE UNDER TEST: prepareLocalMedia hands the media manager
// context.Background() (internal/engine/download.go), while Session.Stop()
// only calls s.cancel(). A fetch that never inherited the session context
// can never observe the cancellation, so a Stop issued while a Download &
// Stream fetch is in flight leaves the HTTP body / yt-dlp subprocess
// running until its own multi-minute timeout. The session goroutine moves
// on, the card says "stopping", and the actual work keeps going — the
// exact defect in the report.
//
// These tests use only the codebase's existing test seams:
//   - media.Manager with a fake yt-dlp subprocess (SetYTDLPFnForTest), so
//     the "download" is a goroutine that observes the ctx the manager was
//     handed — no real network, no real subprocess, no flakiness.
//   - a hand-built Session (same shape as download_e2e_test.go) with a real
//     cancellable context, calling the REAL prepareLocalMedia.

// reproFetch is the fake yt-dlp subprocess: it records the ctx it was
// handed and blocks until that ctx dies or the test releases it.
type reproFetch struct {
	started chan struct{}
	done    chan error
	finish  chan struct{}
}

func newReproFetch() *reproFetch {
	return &reproFetch{started: make(chan struct{}), done: make(chan error, 1), finish: make(chan struct{})}
}

// reproYTDLPSession builds a Download-mode session whose media manager runs
// a fake yt-dlp that never finishes on its own. The media kind is YouTube
// so the fetch goes through the yt-dlp route (the fake) rather than the
// plain HTTP path.
func reproYTDLPSession(t *testing.T, dir string, id string) (*Session, *reproFetch) {
	t.Helper()
	mgr, err := mediateam.NewManager(mediateam.Options{
		Enabled:       true,
		Dir:           dir,
		YTDLPPath:     "yt-dlp", // reported as available; the fake replaces the exec
		MaxBytes:      64 << 20,
		Timeout:       30 * time.Second,
		TTL:           time.Hour,
		MaxConcurrent: 1,
	}, nil)
	if err != nil {
		t.Fatalf("media manager: %v", err)
	}
	rf := newReproFetch()
	mgr.SetYTDLPFnForTest(func(ctx context.Context, args []string) ([]byte, []byte, error) {
		close(rf.started)
		select {
		case <-ctx.Done():
			rf.done <- ctx.Err()
			return nil, nil, fmt.Errorf("yt-dlp: cancelled: %w", ctx.Err())
		case <-rf.finish:
			rf.done <- nil
			return []byte{}, nil, nil
		}
	})
	s := &Session{
		ID:        SessionID(id),
		MediaURL:  "https://www.youtube.com/watch?v=repro",
		MediaKind: MediaYouTube,
		playMode:  mediateam.ModeDownload,
		mgr:       &Manager{mediaMgr: mgr},
		log:       noopLogger{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel, s.done = ctx, cancel, make(chan struct{})
	return s, rf
}

// TestReproStopDoesNotCancelTheFetch is the direct reproduction: press Stop
// while the fetch is running and assert whether the fetch observed it.
func TestReproStopDoesNotCancelTheFetch(t *testing.T) {
	dir := t.TempDir()
	s, rf := reproYTDLPSession(t, dir, "repro-1")

	prepareErr := make(chan error, 1)
	go func() { prepareErr <- s.prepareLocalMedia() }()

	select {
	case <-rf.started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started within 5s")
	}

	stopAt := time.Now()
	s.Stop()

	select {
	case err := <-rf.done:
		if err == nil {
			t.Fatalf("BUG: Stop() did not cancel the in-flight download — the fetch finished on its own after Stop")
		}
		t.Logf("PASS: the fetch observed cancellation %s after Stop()", time.Since(stopAt).Round(time.Millisecond))
	case <-time.After(10 * time.Second):
		select {
		case err := <-rf.done:
			if err == nil {
				t.Fatalf("BUG: download completed normally after Stop()")
			}
			t.Logf("PASS: the fetch observed cancellation after Stop (slow path)")
		default:
		}
		t.Fatal("BUG REPRODUCED: download still running 10s after Stop() — the session context never reached the fetch")
	case <-prepareErr:
		select {
		case err := <-rf.done:
			if err == nil {
				t.Fatalf("BUG: download completed normally after Stop()")
			}
			t.Logf("PASS: prepareLocalMedia unwound (%v) %s after Stop()", err, time.Since(stopAt).Round(time.Millisecond))
		case <-time.After(2 * time.Second):
			t.Fatal("prepareLocalMedia returned but the fake subprocess never observed anything")
		}
	}
	// Release the fake so no goroutine leaks even on failure. The fake
	// sends exactly once on rf.done, so once a branch above consumed it
	// the trailing wait must be non-blocking: a second blocking receive
	// would hang forever after a successful assertion.
	select {
	case <-rf.finish:
	default:
		close(rf.finish)
	}
	select {
	case <-rf.done:
	default:
	}
}

// TestReproStopLeavesOrphanYTDLP proves the orphan-worker half: a fetch that
// survives the session is exactly the "no orphan workers" requirement
// broken, and it also holds the manager's single concurrency slot hostage
// (a later session cannot download at all while the zombie holds it).
func TestReproStopLeavesOrphanYTDLP(t *testing.T) {
	dir := t.TempDir()
	s, rf := reproYTDLPSession(t, dir, "repro-2")

	go func() { _ = s.prepareLocalMedia() }()
	select {
	case <-rf.started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started within 5s")
	}

	s.Stop()

	select {
	case <-rf.done:
		t.Logf("PASS: no orphan — the fetch terminated with the session")
	case <-time.After(5 * time.Second):
		t.Fatal("BUG REPRODUCED: orphan yt-dlp still running 5s after the session was stopped")
	}
	select {
	case <-rf.finish:
	default:
		close(rf.finish)
	}
	select {
	case <-rf.done:
	case <-time.After(5 * time.Second):
	}
}
