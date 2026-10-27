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

// meetToolbarMoreOptionsPage reproduces narrow Google Meet layout where "Present now"
// is nested inside the "More options" popup menu rather than on the toolbar.
const meetToolbarMoreOptionsPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet</title></head>
<body>
  <div id="controls" role="button">
    <button id="audio" aria-label="Audio settings">Audio settings</button>
    <button id="mic" aria-label="Turn off microphone">Microphone</button>
    <button id="cam" aria-label="Turn off camera">Camera</button>
    <button id="more" aria-label="More options" onclick="openMoreMenu()">more_vert</button>
    <button id="leave" aria-label="Leave call">Leave call</button>
  </div>

  <div id="moreMenu" role="menu" style="display:none">
    <div role="menuitem" id="itempresent" onclick="openShare()">Share screen</div>
    <div role="menuitem" id="itemsettings">Settings</div>
  </div>

  <div id="banner"></div>

<script>
  function openMoreMenu() {
    document.getElementById('moreMenu').style.display = 'block';
    window.__moreOpened = true;
  }
  function openShare() {
    document.getElementById('moreMenu').style.display = 'none';
    window.__shareClicked = true;
  }
</script>
</body></html>`

func TestClickPresentButtonUnderMoreOptionsMenu(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(meetToolbarMoreOptionsPage))
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
		ID:       "s-more-opt",
		state:    StateSharing,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{PresentMode: "tab", PresentTimeoutSeconds: 8},
			log:     testLogger{t},
		},
	}

	// First call should click "More options" and open the menu
	opened, label := s.clickPresentButton()
	if !opened || label != "opened more options" {
		t.Fatalf("expected opened more options, got opened=%v label=%q", opened, label)
	}
	if got := evaluate(t, page, "String(window.__moreOpened === true)"); got != "true" {
		t.Fatal("more options menu was not opened")
	}

	// Second call should find "Share screen" in the open menu and click it
	opened2, label2 := s.clickPresentButton()
	if !opened2 {
		t.Fatal("clickPresentButton failed to click item from open menu")
	}
	if got := evaluate(t, page, "String(window.__shareClicked === true)"); got != "true" {
		t.Fatalf("share screen menu item was not clicked, label=%q", label2)
	}
}

// meetMoreOptionsTogglePage reproduces the REAL production failure seen in
// session s-e94053ec: Meet's toolbar has no "Present now" button at all, only
// "More options" (more_vert). Clicking "More options" opens a menu containing
// "Share screen". The critical behavior: if clickPresentButton is called again
// in a later loop iteration, it must NOT click "More options" again (which
// TOGGLES the menu closed) — it must resolve the share item in the same eval.
const meetMoreOptionsTogglePage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet</title></head>
<body>
  <div id="controls" role="button">
    <button id="audio" aria-label="Audio settings">Audio settings</button>
    <button id="mic" aria-label="Turn on microphone">mic_off</button>
    <button id="cam" aria-label="Turn on camera">videocam_off</button>
    <button id="more" aria-label="More options" onclick="toggleMoreMenu()">more_vert</button>
    <button id="leave" aria-label="Leave call">Leave call</button>
  </div>

  <div id="moreMenu" role="menu" style="display:none">
    <li role="menuitem" id="itemshare" onclick="sharePicked()">Share screen</li>
    <li role="menuitem" id="itemsettings">Settings</li>
  </div>

  <div id="banner"></div>

<script>
  var menuOpen = false;
  var openCount = 0;
  function toggleMoreMenu() {
    menuOpen = !menuOpen;
    if (menuOpen) openCount++;
    document.getElementById('moreMenu').style.display = menuOpen ? 'block' : 'none';
    window.__openCount = openCount;
  }
  function sharePicked() {
    document.getElementById('moreMenu').style.display = 'none';
    menuOpen = false;
    document.getElementById('banner').textContent = 'You are presenting';
    window.__shareClicked = true;
  }
</script>
</body></html>`

// TestPresentFlowNeverTogglesMoreOptionsMenuClosed is the regression test for
// the s-e94053ec outage: the present flow must open "More options" exactly
// once and then pick the share item — never re-click the toggle.
func TestPresentFlowNeverTogglesMoreOptionsMenuClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(meetMoreOptionsTogglePage))
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
		ID:       "s-toggle",
		state:    StateSharing,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{PresentMode: "tab", PresentTimeoutSeconds: 15},
			log:     testLogger{t},
		},
	}

	// First call: opens the More options menu.
	opened, label := s.clickPresentButton()
	if !opened || label != "opened more options" {
		t.Fatalf("first call: expected 'opened more options', got opened=%v label=%q", opened, label)
	}
	if got := evaluate(t, page, "String(window.__openCount)"); got != "1" {
		t.Fatalf("More options opened %s times, want 1", got)
	}

	// Second call: must pick "Share screen" from the already-open menu in the
	// SAME eval, without clicking the toggle again (which would close it).
	opened2, label2 := s.clickPresentButton()
	if !opened2 {
		t.Fatal("second call found no share item in the open menu")
	}
	if label2 == "opened more options" {
		t.Fatal("second call re-clicked the More options toggle instead of picking the share item (menu toggle bug)")
	}
	if got := evaluate(t, page, "String(window.__shareClicked === true)"); got != "true" {
		t.Fatalf("share item was not clicked, label=%q", label2)
	}
	if got := evaluate(t, page, "String(window.__openCount)"); got != "1" {
		t.Fatalf("More options was toggled %s times after two calls, want exactly 1", got)
	}
}

// TestPresentFlowEndToEndFromMoreOptions drives the full startPresenting loop
// against a page whose share button lives only inside the More options menu —
// proving the session reaches "presenting" without timing out.
func TestPresentFlowEndToEndFromMoreOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(meetMoreOptionsTogglePage))
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
		ID:       "s-e2e-present",
		state:    StateSharing,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{PresentMode: "tab", PresentTimeoutSeconds: 20},
			log:     testLogger{t},
		},
	}

	done := make(chan error, 1)
	go func() { done <- s.startPresenting() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("startPresenting failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("startPresenting timed out — the More options flow is still broken end to end")
	}
	if got := evaluate(t, page, "String(window.__shareClicked === true)"); got != "true" {
		t.Fatal("share item was not clicked by startPresenting")
	}
}
