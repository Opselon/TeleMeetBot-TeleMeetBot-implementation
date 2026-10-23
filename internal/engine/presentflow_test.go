package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// meetDirectSharePage mirrors the Meet build that has no "A tab" source menu:
// the toolbar button itself calls getDisplayMedia, the label flips to
// "Presentation is starting..." until Chromium answers the request, and the
// call is presenting once the stream is live. That is exactly the page the live
// session was stuck on when the share flow timed out.
const meetDirectSharePage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet - fake call</title></head>
<body>
  <div id="controls" role="button" aria-label="call controls share screen leave call">
    <button id="share" aria-label="Share screen" onclick="startShare()">computer_arrow_up</button>
    <button id="leave" aria-label="Leave call">call_end</button>
  </div>
<script>
  async function startShare() {
    const btn = document.getElementById('share');
    btn.setAttribute('aria-label', 'Presentation is starting...');
    try {
      const stream = await navigator.mediaDevices.getDisplayMedia({video: true, audio: true});
      window.__stream = stream;
      const v = stream.getVideoTracks()[0];
      window.__captureLabel = v ? v.label : '';
      window.__captureState = v ? v.readyState : '';
      btn.setAttribute('aria-label', 'Stop presenting');
      const banner = document.createElement('div');
      banner.id = 'banner';
      banner.textContent = 'You are presenting';
      document.body.appendChild(banner);
    } catch (e) {
      window.__captureError = String(e);
      btn.setAttribute('aria-label', 'Share screen');
      const d = document.createElement('div');
      d.id = 'caperr';
      d.textContent = 'capture failed: ' + String(e);
      document.body.appendChild(d);
    }
  }
</script>
</body></html>`

// TestStartPresentingGrantsRealTabCapture drives the production present flow
// (starting) against a Meet-shaped page that really calls getDisplayMedia, in a
// real Chromium launched with the real flags. It fails when the media tab is not
// reachable as the configured capture source: getDisplayMedia never resolves,
// the control stays on "Presentation is starting...", and the flow times out —
// the exact live failure this guards against.
func TestStartPresentingGrantsRealTabCapture(t *testing.T) {
	browserBin(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(meetDirectSharePage))
	}))
	defer srv.Close()

	m := NewManager(
		BrowserConfig{Executable: browserBin(t), Headless: true, WindowWidth: 1280, WindowHeight: 800},
		MediaOptions{},
		MeetOptions{PresentMode: "tab", CaptureSource: "TeleMeet Media", PresentTimeoutSeconds: 40},
		testLogger{t},
	)
	b, closeFn, err := launchBrowser(launchOptions{browser: m.browserCfg, meet: m.meetCfg}, testLogger{t})
	if err != nil {
		t.Fatalf("launch browser: %v", err)
	}
	defer closeFn()

	// The media tab exactly as openMedia prepares it: loopback document, then the
	// title lock that makes Chromium's capture matcher find it.
	media, err := newPage(b, m.browserCfg)
	if err != nil {
		t.Fatalf("media page: %v", err)
	}
	mediaTarget, err := players.playerURL(captureTestDoc())
	if err != nil {
		t.Fatalf("serve media: %v", err)
	}
	if err := media.Timeout(30 * time.Second).Navigate(mediaTarget); err != nil {
		t.Fatalf("navigate media: %v", err)
	}
	_ = media.WaitLoad()
	if _, err := media.Eval(titleLockJS("TeleMeet Media")); err != nil {
		t.Fatalf("title lock: %v", err)
	}

	meet, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("meet page: %v", err)
	}
	if err := meet.Timeout(30 * time.Second).Navigate(srv.URL); err != nil {
		t.Fatalf("navigate meet: %v", err)
	}
	_ = meet.WaitLoad()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s := &Session{
		ID:        "s-present-e2e",
		TabTitle:  "TeleMeet Media",
		MeetURL:   srv.URL,
		state:     StateSharing,
		ctx:       ctx,
		browser:   b,
		mediaPage: media,
		meetPage:  meet,
		log:       testLogger{t},
		mgr:       m,
	}

	if err := s.startPresenting(); err != nil {
		errText := evaluate(t, meet, "String(window.__captureError || '')")
		t.Fatalf("startPresenting: %v (page capture error=%q)", err, errText)
	}
	if ok, _ := s.presenting(); !ok {
		t.Fatal("presenting() false after a successful share")
	}
	label := evaluate(t, meet, "String(window.__captureLabel || '')")
	st := evaluate(t, meet, "String(window.__captureState || '')")
	t.Logf("capture label=%q state=%q", label, st)
	if !strings.HasPrefix(label, "web-contents-media-stream") {
		t.Fatalf("Meet captured %q (state=%q), want the media tab (web-contents-media-stream)", label, st)
	}
	if st != "live" {
		t.Fatalf("capture track state = %q, want live", st)
	}
}
