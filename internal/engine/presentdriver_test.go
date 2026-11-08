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

// driveDisplayMedia installs the capture stream through page JavaScript, so
// its test needs only a page that implements the contract the production code
// relies on: window.__tmPCs (a stub peer connection with addTrack/getSenders,
// exactly the shape meetPeerHookJS records) plus a getDisplayMedia that grants
// a video track. Chromium's --auto-select-tab-capture-source-by-title would
// answer that call in production; the stub stands in for it.
const meetDrivePage = `<!doctype html>
<html><body>
<script>
window.__tmPCs = [ (function(){
  var self = { senders: [] };
  self.getSenders = function(){ return self.senders; };
  self.addTrack = function(t){ self.senders.push({track: t}); };
  return self;
})() ];
window.__calls = 0;
Object.defineProperty(navigator, "mediaDevices", { value: {
  getDisplayMedia: function(){
    window.__calls++;
    var mk = function(kind, label){ return {kind: kind, label: label, readyState: "live"}; };
    return Promise.resolve({ getVideoTracks: function(){ return [mk("video", "web-contents-media-stream:0")]; },
                             getAudioTracks: function(){ return [mk("audio", "web-contents-media-stream:0")]; } });
  }
}, configurable: true });
</script>
</body></html>
`

// driveTestSession builds the minimal Session wired to one page, the same
// shape presentbtn_test.go builds for the toolbar click tests.
func driveTestSession(t *testing.T, body string) *Session {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	b, closeFn := newTestBrowser(t)
	t.Cleanup(closeFn)

	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	if err := page.Timeout(30 * time.Second).Navigate(srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	_ = page.WaitLoad()
	return &Session{
		ID:       SessionID("s-drive"),
		state:    StateSharing,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{PresentMode: "tab", PresentTimeoutSeconds: 8},
			log:     testLogger{t},
		},
	}
}

func (s *Session) driveEval(t *testing.T, js string) string {
	t.Helper()
	res, err := s.meetPage.Timeout(10 * time.Second).Eval(js)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	return res.Value.Str()
}

// TestDriveDisplayMediaAddsCaptureToMeetPeerConnection pins the primary
// presentation path used when the Meet toolbar exposes no present/share
// control at all (verified live: sessions s-8a1200a6 / s-c436cdfe /
// s-56c126e9 — the bottom bar carries camera/mic/leave/chat only and the
// "More options" menu has no share item).
func TestDriveDisplayMediaAddsCaptureToMeetPeerConnection(t *testing.T) {
	s := driveTestSession(t, meetDrivePage)

	ok, note := s.driveDisplayMedia()
	if !ok {
		t.Fatalf("driveDisplayMedia failed: %s", note)
	}
	if !strings.Contains(note, "web-contents-media-stream") {
		t.Errorf("note should name the capture track, got %q", note)
	}
	if got := s.driveEval(t, "() => String(window.__calls)"); got != "1" {
		t.Fatalf("getDisplayMedia called %s times, want exactly 1", got)
	}
	if got := s.driveEval(t, "() => window.__tmPCs[0].senders.map(x => x.track.kind).join(\",\")"); got != "video,audio" {
		t.Fatalf("expected the video track plus the audio track on the peer connection, got %q", got)
	}
}

// TestDriveDisplayMediaRefusesToDoublePublish pins the guard that keeps the
// driver from stacking a second share on top of a live one: a peer connection
// that already publishes a live video track must be left alone.
func TestDriveDisplayMediaRefusesToDoublePublish(t *testing.T) {
	// A peer connection that already publishes a LIVE video track: the
	// driver must refuse instead of stacking a second share.
	body := strings.Replace(meetDrivePage,
		"var self = { senders: [] };",
		`var self = { senders: [{track: {kind: "video", label: "existing", readyState: "live"}}] };`, 1)
	if !strings.Contains(body, `readyState: "live"`) {
		t.Fatal("test fixture failed to pre-populate the peer connection senders")
	}
	s := driveTestSession(t, body)

	ok, note := s.driveDisplayMedia()
	if ok {
		t.Fatal("driveDisplayMedia must NOT add a track when the peer connection already publishes a live video track")
	}
	if !strings.Contains(note, "already publishes video") {
		t.Fatalf("note must name the double-publish refusal, got %q", note)
	}
	if got := s.driveEval(t, "() => String(window.__calls)"); got != "0" {
		t.Fatalf("getDisplayMedia must not run when a live video track exists, ran %s times", got)
	}
}

// TestDriveDisplayMediaSurvivesNoPeerConnections pins the fail-open contract:
// a page with no peer connections yet must never panic or hang — the present
// loop falls back to the toolbar click path unchanged.
func TestDriveDisplayMediaSurvivesNoPeerConnections(t *testing.T) {
	s := driveTestSession(t, `<!doctype html><html><body>
<script>
Object.defineProperty(navigator, "mediaDevices", { value: {
  getDisplayMedia: function(){ return Promise.resolve({getVideoTracks: function(){return []}, getAudioTracks: function(){return []}}); }
}, configurable: true });
</script>
</body></html>`)

	ok, note := s.driveDisplayMedia()
	if ok {
		t.Fatal("driveDisplayMedia must report NOT ok when no peer connection exists")
	}
	if note == "" {
		t.Fatal("note should explain the refusal")
	}
}

// TestDriveDisplayMediaSurvivesRefusedCapture pins the second fail-open path:
// getDisplayMedia itself rejecting (the tab-capture flag did not match, a
// user gesture is demanded on some builds) must surface as ok=false with a
// diagnostic note, never as a JS exception swallowed by timeout.
func TestDriveDisplayMediaSurvivesRefusedCapture(t *testing.T) {
	body := strings.Replace(meetDrivePage,
		`return Promise.resolve({ getVideoTracks: function(){ return [mk("video", "web-contents-media-stream:0")]; },
                             getAudioTracks: function(){ return [mk("audio", "web-contents-media-stream:0")]; } });`,
		`return Promise.reject(new Error("NotAllowedError: Permission denied"));`, 1)
	s := driveTestSession(t, body)

	ok, note := s.driveDisplayMedia()
	if ok {
		t.Fatal("driveDisplayMedia must report NOT ok when the capture is refused")
	}
	if note == "" {
		t.Fatal("note should carry the refusal reason for the operator log")
	}
}
