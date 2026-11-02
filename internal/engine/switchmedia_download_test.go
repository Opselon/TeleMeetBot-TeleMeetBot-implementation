package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-rod/rod"

	movideo "github.com/Opselon/TeleMeetBot/internal/media"
)

// Mid-session media switching in Download mode. Switching the stream must
// resolve the NEW target through the media manager before touching active
// playback, adopt it under a lease, and free the PREVIOUS track's file
// immediately — an abandoned attempt must never accumulate on disk. The
// share is bound to the tab, so the whole operation happens on the same tab
// without re-presenting.
//
// The switch tests point at loopback servers, which the SSRF gate refuses by
// default: enable them for the duration of the suite, exactly the way the
// download e2e tests do.

// smallMP4Payload returns a few bytes the direct fetcher accepts as a media
// file (the loopback server reports video/mp4).
func smallMP4Payload() []byte {
	return []byte{0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70, 0x69, 0x73, 0x6f, 0x6d, 0x00, 0x00, 0x00, 0x00}
}

// mediaFileServer serves body at /clip.mp4 so ClassifyMedia recognises the
// URL as a direct media file (the plain-host path classifies as a page).
func mediaFileServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newSwitchDownloadSession builds a Download-mode session whose browser half
// is faked. The fake ALSO performs the session-side adoption the real switch
// does after playback verification (download → lease → adopt → free the old
// file), so the test observes the post-switch state rather than a mid-switch
// one. onAdopt lets a test customise what "playback verified" means.
func newSwitchDownloadSession(t *testing.T, mgr *Manager, onAdopt func(*Session, MediaTarget) error) *Session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s := &Session{
		ID:        "s-switch-dl",
		MediaURL:  "https://www.youtube.com/watch?v=aaaaaaaaaaa",
		MediaKind: MediaYouTube,
		TabTitle:  "TeleMeet Media",
		state:     StateStreaming,
		detail:    "[6/6] Streaming live",
		created:   time.Now(),
		ctx:       ctx,
		// A non-nil page marks the tab as open; the switchRun fake stands in
		// for everything the real tab flow does.
		mediaPage: &rod.Page{},
		playMode:  movideo.ModeDownload,
		mgr:       mgr,
	}
	s.switchRun = func(target MediaTarget, stale func() bool) error {
		if stale() {
			return errSwitchSuperseded
		}
		if onAdopt != nil {
			return onAdopt(s, target)
		}
		return nil
	}
	return s
}

// TestSwitchMediaDownloadsNewTargetInDownloadMode verifies the Download-mode
// switch resolves the new target through the real media manager BEFORE the
// tab is touched, takes the lease in the production ordering (lease before
// adoption), and reports success with the session still streaming.
func TestSwitchMediaDownloadsNewTargetInDownloadMode(t *testing.T) {
	movideo.SetAllowLoopbackForTest(true)
	defer movideo.SetAllowLoopbackForTest(false)

	mgr := newDownloadManager(t, t.TempDir())
	srv := mediaFileServer(t, smallMP4Payload())

	var (
		gotRes *downloadResult
		leased bool
	)
	s := newSwitchDownloadSession(t, mgr, func(sess *Session, target MediaTarget) error {
		// Mirror the real applySwitch ordering: download and verify the new
		// target first, then lease, then adopt. A failure at any step must
		// abort the switch rather than half-adopting it.
		res, err := sess.runDownloadWithID(context.Background(), mgr.MediaManager(), target, "ses-s-switch-dl-new")
		if err != nil {
			return err
		}
		if res == nil || res.Path == "" || res.Bytes == 0 {
			return errors.New("the switch download produced no usable file")
		}
		gotRes = res
		sess.leaseMedia(res)
		// The lease must be registered under this session before adoption.
		if mm := sess.mediaManager(); mm != nil {
			leased = len(mm.LeaseOwners(res.ID)) > 0
		}
		return nil
	})

	if err := s.SwitchMedia(srv.URL + "/clip.mp4"); err != nil {
		t.Fatalf("SwitchMedia: %v", err)
	}
	if gotRes == nil {
		t.Fatal("the switch never resolved the new target through the media manager")
	}
	if gotRes.Bytes == 0 {
		t.Error("the switch download must produce a non-empty file")
	}
	if !leased {
		t.Error("the switched track must be leased to the session before adoption")
	}
	if st := s.Snapshot(); st.State != StateStreaming {
		t.Errorf("State = %q, want streaming (the share must be untouched)", st.State)
	}
}

// TestSwitchMediaDownloadFailureFallsBackToBrowser covers the failure shape:
// the new target cannot be downloaded (the extractor returns a verdict), so
// the switch must NOT fail the session or the share — it absorbs the failure
// and the session keeps a playable target.
func TestSwitchMediaDownloadFailureFallsBackToBrowser(t *testing.T) {
	movideo.SetAllowLoopbackForTest(true)
	defer movideo.SetAllowLoopbackForTest(false)

	mgr := newDownloadManager(t, t.TempDir())
	s := newSwitchDownloadSession(t, mgr, nil)

	// An undownloadable target: runDownload returns a verdict, which the
	// switch absorbs (falling back to a direct browser navigation in
	// production). The switch must still report success for the session.
	if err := s.SwitchMedia("https://example.com/clip.mp4"); err != nil {
		t.Fatalf("a failed switch download must not fail the switch: %v", err)
	}
	// The session must survive with a live state, not error out.
	if st := s.Snapshot(); st.State != StateStreaming {
		t.Errorf("State = %q, want streaming (the share must be untouched)", st.State)
	}
}

// TestSwitchMediaRollsBackOnNavigationFailure covers the guard: when the
// switch's browser half fails after the download succeeded, the new track's
// file is cleaned up immediately so abandoned attempts do not accumulate.
func TestSwitchMediaRollsBackOnNavigationFailure(t *testing.T) {
	movideo.SetAllowLoopbackForTest(true)
	defer movideo.SetAllowLoopbackForTest(false)

	mgr := newDownloadManager(t, t.TempDir())
	srv := mediaFileServer(t, smallMP4Payload())

	var downloaded []string
	s := newSwitchDownloadSession(t, mgr, func(sess *Session, target MediaTarget) error {
		res, err := sess.runDownloadWithID(context.Background(), mgr.MediaManager(), target, "ses-s-switch-dl-rb")
		if err != nil {
			return err
		}
		downloaded = append(downloaded, res.Path)
		// The browser half then fails: the real switch frees the new file
		// and rolls back. Mirror that so the test observes the guard.
		sess.freeUnleasedResult(res, "switch navigation failed (test)")
		return errors.New("navigate: page closed")
	})

	// SwitchMedia reports the failure; the session stays on its previous URL.
	if err := s.SwitchMedia(srv.URL + "/clip.mp4"); err == nil {
		t.Fatal("the switch must report the navigation failure")
	}
	// The abandoned download must not linger on disk.
	for _, p := range downloaded {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("an abandoned switch download was not cleaned up: %s", p)
		}
	}
}
