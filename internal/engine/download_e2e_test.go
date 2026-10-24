package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

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
