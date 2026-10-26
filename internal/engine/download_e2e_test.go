package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// TestManagerWiresMediaManager proves the engine constructs the media manager
// from its options (which FromConfig builds from the config), and that a
// session can resolve a direct media URL to a local file.
func TestManagerWiresMediaManager(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(BrowserConfig{}, MediaOptions{
		DefaultVolume:   80,
		DownloadEnabled: true, DownloadDir: dir, DownloadTTLMinutes: 60,
		DownloadMaxSizeMB: 64, DownloadTimeoutSec: 30, DownloadConcurrent: 1,
		DownloadBaseDir: dir,
	}, MeetOptions{}, nil)
	if mgr.MediaManager() == nil {
		t.Fatal("the engine did not build a media manager for an enabled config")
	}
	if !mgr.MediaManager().Enabled() {
		t.Fatal("the wired media manager reports disabled")
	}
	if got := mgr.MediaManager().Opts().Dir; got != dir {
		t.Fatalf("media manager dir = %q, want %q", got, dir)
	}
	// The manager must be nil (and nothing must crash) when disabled.
	off := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{}, nil)
	if off.MediaManager() != nil {
		t.Fatal("a disabled config should not build a media manager")
	}
}

// newDownloadManager builds an engine manager with Download & Stream enabled
// against a real (loopback-allowed) media manager. All e2e download tests
// share it so the wiring under test is the production one.
func newDownloadManager(t *testing.T, dir string) *Manager {
	t.Helper()
	return NewManager(BrowserConfig{}, MediaOptions{
		DownloadEnabled: true, DownloadDir: dir, DownloadTTLMinutes: 60,
		DownloadMaxSizeMB: 64, DownloadTimeoutSec: 30, DownloadConcurrent: 1,
		DownloadBaseDir: dir,
	}, MeetOptions{}, nil)
}

// loopbackMediaServer serves a small MP4 payload the downloader will accept.
func loopbackMediaServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestActiveDownloadSurvivesTTLDeadline is the core lifecycle regression: a
// session in Download mode resolves a direct media URL to a local file, and
// the file stays playable past its one-hour-equivalent deadline because the
// session holds an active lease. The TTL is an orphan-cleanup policy, not a
// hard playback deadline. Playback (including an HTTP range request against
// the loopback file server) must still succeed after expiry.
func TestActiveDownloadSurvivesTTLDeadline(t *testing.T) {
	mediateam.SetAllowLoopbackForTest(true)
	defer mediateam.SetAllowLoopbackForTest(false)

	srv := loopbackMediaServer(t, make([]byte, 8192))

	dir := t.TempDir()
	mgr := newDownloadManager(t, dir)
	// A short TTL for the test (the production default is one hour).
	mgr.SetMediaTTLForTest(400 * time.Millisecond)

	s := &Session{ID: "s-e2e", MediaURL: srv.URL, MediaKind: MediaDirect, mgr: mgr}
	if err := s.SetPlaybackMode(mediateam.ModeDownload); err != nil {
		t.Fatalf("SetPlaybackMode: %v", err)
	}
	if err := s.prepareLocalMedia(); err != nil {
		t.Fatalf("prepareLocalMedia: %v", err)
	}
	st := s.Snapshot()
	if st.PlayMode != "download" {
		t.Fatalf("PlayMode = %q", st.PlayMode)
	}
	if st.LocalFile.Bytes == 0 || st.LocalFile.URL == "" {
		t.Fatalf("the session card does not report the local file: %+v", st.LocalFile)
	}
	if _, err := os.Stat(st.LocalFile.Path); err != nil {
		t.Fatalf("the local file is missing: %v", err)
	}
	if s.MediaExpiresAt().IsZero() {
		t.Fatal("the session must report an orphan-cleanup deadline for downloaded media")
	}

	// Wait until the deadline has genuinely passed, then let the janitor
	// run repeatedly: an ACTIVE session must keep its file.
	deadline := s.MediaExpiresAt()
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		mgr.MediaManager().Sweep(context.Background())
		if _, err := os.Stat(st.LocalFile.Path); os.IsNotExist(err) {
			t.Fatalf("the janitor deleted an active session's media right after its TTL deadline")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The lease is what protects it: the manager must report this session as
	// an owner, and the loopback file server must still serve the file —
	// including HTTP range requests, which is what a media player issues
	// when it seeks.
	owners := mgr.MediaManager().LeaseOwners(s.downloadID())
	if len(owners) == 0 {
		t.Fatal("no lease protects the active session's media")
	}

	if err := assertPlayableOverHTTP(st.LocalFile.URL, true); err != nil {
		t.Fatalf("playback after the TTL deadline failed: %v", err)
	}

	// Only when the session stops does the file go away.
	s.cleanup()
	if _, err := os.Stat(st.LocalFile.Path); !os.IsNotExist(err) {
		t.Fatalf("the file survived the session cleanup that should have released it: %v", err)
	}
}

// assertPlayableOverHTTP fetches the loopback URL a media tab would load and
// asserts the file is served. When range is set, it asserts a 206 Partial
// Content response for a byte range — the exact request shape a player makes
// on a seek — so a janitor race would surface as a 404 here.
func assertPlayableOverHTTP(url string, rangeRequest bool) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if rangeRequest {
		req.Header.Set("Range", "bytes=0-1023")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	if rangeRequest && res.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("range request to %s returned %d, want 206 (body=%q)", url, res.StatusCode, string(body))
	}
	if !rangeRequest && res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d, want 200", url, res.StatusCode)
	}
	if len(body) == 0 {
		return fmt.Errorf("GET %s returned an empty body", url)
	}
	return nil
}

// TestPlaybackAndRangeRequestsSurviveJanitor proves the janitor cannot break
// an active session's playback: while the session owns the media, repeated
// sweeps interleaved with playback requests (including range requests) never
// produce a 404.
func TestPlaybackAndRangeRequestsSurviveJanitor(t *testing.T) {
	mediateam.SetAllowLoopbackForTest(true)
	defer mediateam.SetAllowLoopbackForTest(false)

	srv := loopbackMediaServer(t, make([]byte, 32*1024))

	dir := t.TempDir()
	mgr := newDownloadManager(t, dir)
	mgr.SetMediaTTLForTest(250 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Session{ID: "s-range", MediaURL: srv.URL, MediaKind: MediaDirect, mgr: mgr, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	if err := s.SetPlaybackMode(mediateam.ModeDownload); err != nil {
		t.Fatalf("SetPlaybackMode: %v", err)
	}
	if err := s.prepareLocalMedia(); err != nil {
		t.Fatalf("prepareLocalMedia: %v", err)
	}
	st := s.Snapshot()

	// Hammer sweeps across the deadline and verify every playback-shaped
	// request still succeeds. This is the exact class of failure the bug
	// produced: the janitor deleted the file mid-stream.
	var wg sync.WaitGroup
	var failed atomicError
	for i := 0; i < 20; i++ {
		time.Sleep(40 * time.Millisecond) // crosses the 250ms deadline
		mgr.MediaManager().Sweep(context.Background())
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := assertPlayableOverHTTP(st.LocalFile.URL, true); err != nil {
				failed.set(err)
			}
			if err := assertPlayableOverHTTP(st.LocalFile.URL, false); err != nil {
				failed.set(err)
			}
		}()
		if failed.get() != nil {
			t.Fatalf("a playback request failed while the session was active: %v", failed.get())
		}
	}
	wg.Wait()
	if err := failed.get(); err != nil {
		t.Fatalf("a playback request failed while the session was active: %v", err)
	}
	if _, err := os.Stat(st.LocalFile.Path); err != nil {
		t.Fatalf("the active session's media vanished under the janitor: %v", err)
	}
}

// TestInactiveExpiredFilesStillReclaimed proves the TTL still works as an
// orphan policy: once the owning session releases its lease (here by a media
// switch), an expired file is reclaimed by the very next sweep.
func TestInactiveExpiredFilesStillReclaimed(t *testing.T) {
	mediateam.SetAllowLoopbackForTest(true)
	defer mediateam.SetAllowLoopbackForTest(false)

	srv := loopbackMediaServer(t, make([]byte, 4096))

	dir := t.TempDir()
	mgr := newDownloadManager(t, dir)
	mgr.SetMediaTTLForTest(300 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newSwitchRecorder()
	s := &Session{
		ID:        "s-inactive-reclaim",
		MediaURL:  srv.URL + "/first.mp4",
		MediaKind: MediaDirect,
		mgr:       mgr,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		mediaPage: &rod.Page{},
		switchRun: rec.run,
		state:     StateStreaming,
	}
	if err := s.SetPlaybackMode(mediateam.ModeDownload); err != nil {
		t.Fatalf("SetPlaybackMode: %v", err)
	}
	if err := s.prepareLocalMedia(); err != nil {
		t.Fatalf("prepareLocalMedia: %v", err)
	}
	st1 := s.Snapshot()
	oldPath := st1.LocalFile.Path
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("first download missing: %v", err)
	}

	// Switching media releases the old file promptly.
	if err := s.SwitchMedia(srv.URL + "/second.mp4"); err != nil {
		t.Fatalf("SwitchMedia: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old downloaded file still exists after switch: %v", err)
	}
}

// TestSessionStopFreesDownloadedFileImmediately proves that when a session
// stops/cleans up, its downloaded media file is removed immediately from disk
// without waiting for the TTL.
func TestSessionStopFreesDownloadedFileImmediately(t *testing.T) {
	mediateam.SetAllowLoopbackForTest(true)
	defer mediateam.SetAllowLoopbackForTest(false)

	srv := loopbackMediaServer(t, make([]byte, 4096))

	dir := t.TempDir()
	mgr := newDownloadManager(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{ID: "s-stop-immed", MediaURL: srv.URL, MediaKind: MediaDirect, mgr: mgr, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	if err := s.SetPlaybackMode(mediateam.ModeDownload); err != nil {
		t.Fatalf("SetPlaybackMode: %v", err)
	}
	if err := s.prepareLocalMedia(); err != nil {
		t.Fatalf("prepareLocalMedia: %v", err)
	}
	st := s.Snapshot()
	if _, err := os.Stat(st.LocalFile.Path); err != nil {
		t.Fatalf("downloaded file missing before stop: %v", err)
	}

	// Session stops / cleans up:
	s.cleanup()

	// The file MUST be deleted from disk immediately!
	if _, err := os.Stat(st.LocalFile.Path); !os.IsNotExist(err) {
		t.Fatalf("downloaded file was not removed immediately on session cleanup: %v", err)
	}
	if s.Snapshot().LocalFile.Path != "" {
		t.Fatal("session still reports local file after cleanup")
	}
}

// TestSessionSwitchMediaFreesOldDownloadedFileImmediately proves that
// switching media in Download mode frees the previous download from disk.
func TestSessionSwitchMediaFreesOldDownloadedFileImmediately(t *testing.T) {
	mediateam.SetAllowLoopbackForTest(true)
	defer mediateam.SetAllowLoopbackForTest(false)

	srv := loopbackMediaServer(t, make([]byte, 4096))

	dir := t.TempDir()
	mgr := newDownloadManager(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newSwitchRecorder()
	s := &Session{
		ID:        "s-switch-immed",
		MediaURL:  srv.URL + "/first.mp4",
		MediaKind: MediaDirect,
		mgr:       mgr,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		mediaPage: &rod.Page{},
		switchRun: rec.run,
		state:     StateStreaming,
	}
	if err := s.SetPlaybackMode(mediateam.ModeDownload); err != nil {
		t.Fatalf("SetPlaybackMode: %v", err)
	}
	if err := s.prepareLocalMedia(); err != nil {
		t.Fatalf("prepareLocalMedia: %v", err)
	}
	st1 := s.Snapshot()
	oldPath := st1.LocalFile.Path
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("first download missing: %v", err)
	}

	// Switch to a new media URL:
	if err := s.SwitchMedia(srv.URL + "/second.mp4"); err != nil {
		t.Fatalf("SwitchMedia: %v", err)
	}

	// The old file must be freed from disk:
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old downloaded file still exists after switch: %v", err)
	}
}

// TestSessionCleanupFreesFilesEvenWhenLocalFileIsNil proves that even if
// s.localFile is nil (e.g. download failed or was cancelled mid-way), session
// cleanup frees any files matching downloadID.
func TestSessionCleanupFreesFilesEvenWhenLocalFileIsNil(t *testing.T) {
	dir := t.TempDir()
	mgr := newDownloadManager(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{ID: "s-fail-cleanup", mgr: mgr, ctx: ctx, cancel: cancel, done: make(chan struct{})}

	// Simulate leftover partial file on disk for this session ID
	leftover := filepath.Join(dir, s.downloadID()+".mp4.part")
	if err := os.WriteFile(leftover, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	// s.localFile is nil!
	s.cleanup()

	// Leftover file must be deleted:
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("leftover file %s was not deleted by cleanup when localFile was nil", leftover)
	}
}
