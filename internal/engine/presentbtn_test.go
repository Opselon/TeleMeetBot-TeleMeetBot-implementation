package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// meetToolbarPage reproduces the real Google Meet call-controls toolbar that
// broke screen sharing: the whole bar is a [role=button] container whose
// combined text includes every control ("call controls ... audio settings
// keyboard_arrow_up ... Present now"), and the audio-settings arrow inside it
// is a button too. A text-substring matcher therefore clicks the container
// (or the audio arrow) instead of "Present now", the share menu never opens,
// and the session dies with "present flow timed out".
const meetToolbarPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet</title></head>
<body>
  <div id="controls" role="button" aria-label="call controls keyboard_arrow_up audio settings microphone camera present now chat people leave call">
    <button id="audio" aria-label="Audio settings keyboard_arrow_up">Audio settings</button>
    <button id="mic" aria-label="Turn off microphone">Microphone</button>
    <button id="cam" aria-label="Turn off camera">Camera</button>
    <button id="present" aria-label="Present now" onclick="openMenu()">Present now</button>
    <button id="leave" aria-label="Leave call">Leave call</button>
  </div>

  <div id="menu" role="menu" style="display:none">
    <div role="menuitem" id="itemtab" onclick="pickTab()">A tab</div>
    <div role="menuitem" id="itemwin">A window</div>
    <div role="menuitem" id="itemscreen">Entire screen</div>
  </div>

  <div id="dialog" role="dialog" style="display:none">
    <div>You are about to share a tab</div>
    <button id="share" aria-label="Share" onclick="startShare()">Share</button>
  </div>

  <div id="banner"></div>

<script>
  function openMenu() {
    document.getElementById('menu').style.display = 'block';
    window.__menuOpened = true;
  }
  function pickTab() {
    document.getElementById('menu').style.display = 'none';
    document.getElementById('dialog').style.display = 'block';
    window.__tabPicked = true;
  }
  function startShare() {
    document.getElementById('dialog').style.display = 'none';
    document.getElementById('banner').textContent = 'You are presenting';
    var stop = document.createElement('button');
    stop.setAttribute('aria-label', 'Stop presenting');
    stop.textContent = 'Stop presenting';
    stop.onclick = function () { document.getElementById('banner').textContent = ''; stop.remove(); };
    document.body.appendChild(stop);
    window.__sharing = true;
  }
</script>
</body></html>`

// TestClickPresentButtonSkipsToolbarContainer is the regression test for the
// screen-share timeout: the Present button must be hit, never the surrounding
// call-controls container nor the audio-settings arrow.
func TestClickPresentButtonSkipsToolbarContainer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(meetToolbarPage))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()

	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	if err := page.Timeout(30 * time.Second).Navigate(srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	_ = page.WaitLoad()

	s := &Session{
		ID:       "s-toolbar",
		state:    StateSharing,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{PresentMode: "tab", PresentTimeoutSeconds: 8},
			log:     testLogger{t},
		},
	}

	if stage, info := s.meetStage(); stage != "meeting" {
		t.Fatalf("meetStage = %q (%s), want meeting; the toolbar must read as in-call", stage, info)
	}

	opened, label := s.clickPresentButton()
	if !opened {
		t.Fatal("clickPresentButton found no button")
	}
	if got := evaluate(t, page, "String(window.__menuOpened === true)"); got != "true" {
		t.Fatalf("present menu did not open — clicked %q instead of the Present now button", label)
	}

	if picked, item := s.clickPresentSource(); !picked {
		t.Fatalf("clickPresentSource found no menu item (item=%q)", item)
	}
	if got := evaluate(t, page, "String(window.__tabPicked === true)"); got != "true" {
		t.Fatal("A tab item was not chosen")
	}
	if confirmed, cl := s.clickShareConfirm(); !confirmed {
		t.Fatalf("clickShareConfirm failed (label=%q)", cl)
	}
	if ok, _ := s.presenting(); !ok {
		t.Fatal("presenting() false after the share flow")
	}
}

// TestPresentSourceSkipsMenuContainer pins the second half of the same defect:
// the menu wrapper carries all item labels in its text, so matching must land
// on the "A tab" item itself.
func TestPresentSourceSkipsMenuContainer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(meetToolbarPage))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()

	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	if err := page.Timeout(30 * time.Second).Navigate(srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	_ = page.WaitLoad()
	_ = evaluate(t, page, "document.getElementById('menu').style.display = 'block'")

	s := &Session{
		ID:       "s-menu",
		state:    StateSharing,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{PresentMode: "tab"},
			log:     testLogger{t},
		},
	}

	if picked, item := s.clickPresentSource(); !picked {
		t.Fatalf("clickPresentSource found no item (%q)", item)
	}
	if got := evaluate(t, page, "String(window.__tabPicked === true)"); got != "true" {
		t.Fatalf("label-matched an element other than the A tab item")
	}
}
