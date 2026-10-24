package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-rod/rod"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// TestManagerWiresMediaManager proves the engine constructs the media manager
// from its options (which FromConfig builds from the config), and that a
// session can resolve a direct media URL to a local file and then expire it.
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

// TestSessionDownloadsAndExpires proves the full path: a session in Download
// mode resolves a direct media URL to a local file, the file backs the
// session, and the mandatory expiry reclaims it even while "playing".
func TestSessionDownloadsAndExpires(t *testing.T) {
	mediateam.SetAllowLoopbackForTest(true)
	defer mediateam.SetAllowLoopbackForTest(false)

	body := make([]byte, 8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	mgr := NewManager(BrowserConfig{}, MediaOptions{
		DownloadEnabled: true, DownloadDir: dir, DownloadTTLMinutes: 60,
		DownloadMaxSizeMB: 64, DownloadTimeoutSec: 30, DownloadConcurrent: 1,
		DownloadBaseDir: dir,
	}, MeetOptions{}, nil)
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
		t.Fatal("the session must report an expiry deadline for downloaded media")
	}

	// Expiry is mandatory: sweep past the deadline and the file must vanish,
	// in-use or not.
	for i := 0; i < 40; i++ {
		mgr.MediaManager().Sweep(context.Background())
		if _, err := os.Stat(st.LocalFile.Path); os.IsNotExist(err) {
			return // pass
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the downloaded file was retained past its 1-hour-equivalent deadline")
}

// TestSessionStopFreesDownloadedFileImmediately proves that when a session stops/cleans up,
// its downloaded media file is removed immediately from disk without waiting for the TTL.
func TestSessionStopFreesDownloadedFileImmediately(t *testing.T) {
	mediateam.SetAllowLoopbackForTest(true)
	defer mediateam.SetAllowLoopbackForTest(false)

	body := make([]byte, 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	mgr := NewManager(BrowserConfig{}, MediaOptions{
		DownloadEnabled: true, DownloadDir: dir, DownloadTTLMinutes: 60,
		DownloadMaxSizeMB: 64, DownloadTimeoutSec: 30, DownloadConcurrent: 1,
		DownloadBaseDir: dir,
	}, MeetOptions{}, nil)

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

// TestSessionSwitchMediaFreesOldDownloadedFileImmediately proves that switching media
// in Download mode frees the previous download from disk.
func TestSessionSwitchMediaFreesOldDownloadedFileImmediately(t *testing.T) {
	mediateam.SetAllowLoopbackForTest(true)
	defer mediateam.SetAllowLoopbackForTest(false)

	body := make([]byte, 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	mgr := NewManager(BrowserConfig{}, MediaOptions{
		DownloadEnabled: true, DownloadDir: dir, DownloadTTLMinutes: 60,
		DownloadMaxSizeMB: 64, DownloadTimeoutSec: 30, DownloadConcurrent: 1,
		DownloadBaseDir: dir,
	}, MeetOptions{}, nil)

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
