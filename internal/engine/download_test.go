package engine

import (
	"testing"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// The Download & Stream mode must default to Browser so existing callers and
// stored sessions keep the original behaviour.
func TestPlaybackModeDefaultsToBrowser(t *testing.T) {
	s := &Session{}
	if got := s.PlaybackMode(); got != mediateam.ModeBrowser {
		t.Fatalf("expected the default mode to be Browser, got %q", got)
	}
	if got := NormalizePlaybackMode(""); got != MediaModeBrowser {
		t.Fatalf("NormalizePlaybackMode(\"\") = %q, want %q", got, MediaModeBrowser)
	}
	if got := NormalizePlaybackMode("download"); got != MediaModeDownload {
		t.Fatalf("NormalizePlaybackMode(download) = %q", got)
	}
	if got := NormalizePlaybackMode("nonsense"); got != MediaModeBrowser {
		t.Fatalf("unknown mode should fall back to Browser, got %q", got)
	}
}

// SetPlaybackMode must persist into the snapshot the Telegram card renders.
func TestPlaybackModeSurfacesInSnapshot(t *testing.T) {
	s := &Session{ID: "s-test"}
	if err := s.SetPlaybackMode(mediateam.ModeDownload); err != nil {
		t.Fatalf("SetPlaybackMode: %v", err)
	}
	st := s.Snapshot()
	if st.PlayMode != MediaModeDownload {
		t.Fatalf("snapshot PlayMode = %q, want %q", st.PlayMode, MediaModeDownload)
	}
	if PlaybackModeDisplay(st.PlayMode) != "Download & Stream" {
		t.Fatalf("display label wrong: %q", PlaybackModeDisplay(st.PlayMode))
	}
}

// A session without a local file reports a zero expiry deadline.
func TestMediaExpiresAtZeroWithoutDownload(t *testing.T) {
	s := &Session{ID: "s-test"}
	if !s.MediaExpiresAt().IsZero() {
		t.Fatal("a browser-mode session must not report an expiry deadline")
	}
}
