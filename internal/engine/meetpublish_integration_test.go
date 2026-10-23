package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeMeetWithWebRTC is a fake Meet page that creates a real RTCPeerConnection
// when "Share" is clicked, simulating both a healthy share (encodes video) and a
// regressed share (calls getDisplayMedia, flips the "You are presenting" banner,
// but never sends/encodes the track).
const fakeMeetWithWebRTC = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet WebRTC</title></head>
<body>
  <div id="meeting">
    <div id="banner"></div>
    <button id="present" aria-label="Present now" onclick="startShare()">Present now</button>
  </div>
<script>
  window.__simulateBrokenShare = location.search.indexOf('broken=1') !== -1;
  async function startShare() {
    let stream;
    try {
      stream = await navigator.mediaDevices.getDisplayMedia({video: true});
    } catch (e) {
      document.getElementById('banner').textContent = 'capture failed: ' + e;
      return;
    }
    // Flip the DOM to presenting: this is what Meet does.
    document.getElementById('banner').textContent = 'You are presenting';
    var stop = document.createElement('button');
    stop.setAttribute('aria-label', 'Stop presenting');
    stop.textContent = 'Stop presenting';
    document.getElementById('meeting').appendChild(stop);

    // If broken, stop here: DOM says presenting, getDisplayMedia was called, but
    // the track was never handed to an RTCPeerConnection.
    if (window.__simulateBrokenShare) return;

    // Healthy path: push the track through a real RTCPeerConnection to loopback.
    const pc1 = new RTCPeerConnection();
    const pc2 = new RTCPeerConnection();
    window.__pc1 = pc1; window.__pc2 = pc2;
    pc1.onicecandidate = e => { if (e.candidate) pc2.addIceCandidate(e.candidate).catch(()=>{}); };
    pc2.onicecandidate = e => { if (e.candidate) pc1.addIceCandidate(e.candidate).catch(()=>{}); };
    pc1.addTrack(stream.getVideoTracks()[0], stream);
    const offer = await pc1.createOffer();
    await pc1.setLocalDescription(offer);
    await pc2.setRemoteDescription(offer);
    const answer = await pc2.createAnswer();
    await pc2.setLocalDescription(answer);
    await pc1.setRemoteDescription(answer);
  }
</script>
</body></html>`

// TestMeetPublishDiagnosticsDifferentiatesBrokenFromLive proves the new
// logMeetPublish hook: when the DOM says "you are presenting" but no track was
// actually published to a peer connection, it names the failure instead of
// claiming success.
func TestMeetPublishDiagnosticsDifferentiatesBrokenFromLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(fakeMeetWithWebRTC))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()

	// 1) The media tab: must exist so getDisplayMedia auto-select succeeds.
	cfg := BrowserConfig{Executable: browserBin(t), Headless: true, WindowWidth: 1280, WindowHeight: 800}
	mediaPage, err := newPage(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mediaPage.Close() }()
	mediaURL, err := players.playerURL(captureTestDoc())
	if err != nil {
		t.Fatal(err)
	}
	if err := mediaPage.Timeout(30 * time.Second).Navigate(mediaURL); err != nil {
		t.Fatal(err)
	}
	_ = mediaPage.WaitLoad()
	if _, err := mediaPage.Eval(titleLockJS("TeleMeet Media")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1000 * time.Millisecond)

	// Subtest 1: Broken share (DOM says presenting, but no sender / no encoder).
	t.Run("broken_share_is_caught", func(t *testing.T) {
		meetPage, err := newPage(b, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = meetPage.Close() }()
		if _, err := meetPage.EvalOnNewDocument(meetPeerHookJS()); err != nil {
			t.Fatal(err)
		}
		if err := meetPage.Timeout(30 * time.Second).Navigate(srv.URL + "/?broken=1"); err != nil {
			t.Fatal(err)
		}
		_ = meetPage.WaitLoad()

		s := &Session{
			ID:       "s-broken",
			MeetURL:  srv.URL + "/?broken=1",
			state:    StateStreaming,
			ctx:      context.Background(),
			meetPage: meetPage,
			log:      testLogger{t},
			mgr: &Manager{
				browserCfg: BrowserConfig{Executable: browserBin(t), Headless: true},
				meetCfg:    MeetOptions{PresentMode: "tab", CaptureSource: "TeleMeet Media"},
				log:        testLogger{t},
			},
		}

		// Click "Present now"
		if _, err := meetPage.Eval(`() => document.getElementById('present').click()`); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1500 * time.Millisecond)

		// The DOM claim passes:
		if pres, _ := s.presenting(); !pres {
			t.Fatal("presenting() should be true from DOM")
		}

		// First sample seeds the tracker.
		_ = s.logMeetPublish()
		time.Sleep(1200 * time.Millisecond)
		problem := s.logMeetPublish()
		t.Logf("broken share problem detected: %q", problem)
		if problem == "" {
			t.Fatalf("expected logMeetPublish to flag the missing sender, got empty problem")
		}
		if !strings.Contains(problem, "no video sender") {
			t.Errorf("expected problem to mention 'no video sender', got %q", problem)
		}
	})

	// Subtest 2: Healthy share (real WebRTC sender, real encoded frames).
	t.Run("healthy_share_passes", func(t *testing.T) {
		meetPage, err := newPage(b, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = meetPage.Close() }()
		if _, err := meetPage.EvalOnNewDocument(meetPeerHookJS()); err != nil {
			t.Fatal(err)
		}
		if err := meetPage.Timeout(30 * time.Second).Navigate(srv.URL + "/?broken=0"); err != nil {
			t.Fatal(err)
		}
		_ = meetPage.WaitLoad()

		s := &Session{
			ID:       "s-healthy",
			MeetURL:  srv.URL + "/?broken=0",
			state:    StateStreaming,
			ctx:      context.Background(),
			meetPage: meetPage,
			log:      testLogger{t},
			mgr: &Manager{
				browserCfg: BrowserConfig{Executable: browserBin(t), Headless: true},
				meetCfg:    MeetOptions{PresentMode: "tab", CaptureSource: "TeleMeet Media"},
				log:        testLogger{t},
			},
		}

		if _, err := meetPage.Eval(`() => document.getElementById('present').click()`); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2000 * time.Millisecond)

		if pres, _ := s.presenting(); !pres {
			t.Fatal("presenting() should be true from DOM")
		}

		_ = s.logMeetPublish()
		time.Sleep(2500 * time.Millisecond)
		problem := s.logMeetPublish()
		t.Logf("healthy share report problem: %q", problem)
		if problem != "" {
			t.Fatalf("healthy share falsely reported a problem: %q", problem)
		}
	})
}
