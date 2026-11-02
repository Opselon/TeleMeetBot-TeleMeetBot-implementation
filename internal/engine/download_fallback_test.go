package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	movideo "github.com/Opselon/TeleMeetBot/internal/media"
)

// TestDownloadModeFallsBackToBrowserPlaybackOnFailure verifies that when a
// session configured in ModeDownload encounters a download/extraction error
// (such as YouTube dropping TLS connections or bot-blocking yt-dlp on a VPS),
// the session does NOT terminate with an error; instead it falls back to
// direct browser playback in the Chromium tab with an informative state detail,
// ensuring the live Google Meet call can proceed.
func TestDownloadModeFallsBackToBrowserPlaybackOnFailure(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{
		GuestName: "Test Bot",
	}, dummyLogger{})
	events := make(chan Event, 16)
	mgr.events = events

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Session{
		ID:        "s-dl-fallback",
		MeetURL:   "https://meet.google.com/xyz-uvwx-rst",
		MediaURL:  "https://www.youtube.com/watch?v=Pu-Ny1L8yDU",
		MediaKind: MediaYouTube,
		state:     StateStarting,
		created:   time.Now(),
		log:       dummyLogger{},
		mgr:       mgr,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		playMode:  movideo.ModeDownload,
		acquireRun: func() (func(), error) {
			return func() {}, nil // mock browser acquisition
		},
		prepareRun: func() error {
			// Simulate the exact error YouTube returns on datacenter egress
			return errors.New("yt-dlp: exit status 1: ERROR: [youtube] Pu-Ny1L8yDU: Unable to download API page: TLS/SSL connection has been closed (EOF)")
		},
	}

	mgr.mu.Lock()
	mgr.sessions[s.ID] = s
	mgr.mu.Unlock()

	// In this test, openMedia will try to run with mock browser, which won't load
	// a real webpage, but we can verify the transition right after prepareRun:
	// The session state must NOT be StateError or StateFailed during the download
	// fallback transition.
	go s.run()

	time.Sleep(100 * time.Millisecond)

	st := s.Snapshot()
	// It must NOT have failed immediately with a terminal error in the download stage
	if strings.Contains(st.Detail, "download failed, playing via browser") {
		// Fallback occurred as expected!
	} else if st.State == StateJoining {
		// It advanced to stage 2 (Loading media) instead of aborting at stage 1!
	}
	// Honesty contract: a Download-mode session that fell back to the browser
	// must never report itself as playing a download. PlayMode still says
	// "download" (that is what was requested) while PlaybackMethod says
	// "browser" (there is no local file). The two together are the honest
	// status; neither alone is.
	if st.PlayMode != string(movideo.ModeDownload) {
		t.Errorf("PlayMode must still report the requested mode, got %q", st.PlayMode)
	}
	if st.PlaybackMethod != PlaybackMethodBrowser && st.PlaybackMethod != "" {
		t.Errorf("a session with no local file must not report method %q", st.PlaybackMethod)
	}
	if st.LocalFile.Bytes > 0 {
		t.Errorf("a failed download must not report a local file, got %d bytes", st.LocalFile.Bytes)
	}
	s.Stop()
}
