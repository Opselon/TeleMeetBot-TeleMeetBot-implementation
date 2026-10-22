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

// fakeMeetPage mimics the parts of Google Meet's DOM that the automation code
// depends on: the prejoin name field, "Ask to join", the lobby, the meeting
// toolbar, the present menu and the share confirmation dialog. It lets us test
// the real click chain in Chromium without a Google account.
const fakeMeetPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet</title>
<style>button{display:inline-block;margin:8px;padding:6px 12px}</style></head>
<body>
  <div id="prejoin">
    <input id="name" aria-label="Your name" placeholder="Your name" value="">
    <button id="ask" aria-label="Ask to join" onclick="askToJoin()">Ask to join</button>
  </div>

  <div id="lobby" style="display:none">Waiting for the host to admit you…</div>

  <div id="meeting" style="display:none">
    <div id="banner"></div>
    <button id="present" aria-label="Present now" onclick="toggleMenu()">Present now</button>
    <div id="menu" role="menu" style="display:none">
      <div role="menuitem" id="tab" onclick="chooseTab()">A tab</div>
      <div role="menuitem" id="win" onclick="noop()">A window</div>
      <div role="menuitem" id="screen" onclick="noop()">Entire screen</div>
    </div>
  </div>

  <div id="dialog" role="dialog" style="display:none">
    <div>You are about to share a tab</div>
    <button id="share" aria-label="Share" onclick="startSharing()">Share</button>
  </div>

<script>
  function noop() {}
  function askToJoin() {
    document.getElementById('prejoin').style.display = 'none';
    document.getElementById('lobby').style.display = 'block';
    setTimeout(function () {
      document.getElementById('lobby').style.display = 'none';
      document.getElementById('meeting').style.display = 'block';
    }, 700);
  }
  function toggleMenu() {
    var m = document.getElementById('menu');
    m.style.display = m.style.display === 'none' ? 'block' : 'none';
  }
  function chooseTab() {
    document.getElementById('menu').style.display = 'none';
    document.getElementById('dialog').style.display = 'block';
  }
  function startSharing() {
    document.getElementById('dialog').style.display = 'none';
    document.getElementById('banner').textContent = 'You are presenting';
    var stop = document.createElement('button');
    stop.setAttribute('aria-label', 'Stop presenting');
    stop.textContent = 'Stop presenting';
    stop.onclick = function () { document.getElementById('banner').textContent = ''; stop.remove(); };
    document.getElementById('meeting').appendChild(stop);
  }
</script>
</body></html>`

// TestIntegrationFakeMeetJoinAndPresent drives meetStage → fillGuestName →
// clickJoin → (lobby) → clickPresentButton → clickPresentSource →
// clickShareConfirm → presenting against a real Chromium.
func TestIntegrationFakeMeetJoinAndPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(fakeMeetPage))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()

	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	if err := page.Timeout(30 * time.Second).Navigate(srv.URL + "/lookup/abc-defg-hij"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	_ = page.WaitLoad()

	s := &Session{
		ID:       "s-fake",
		MeetURL:  srv.URL,
		state:    StateJoining,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{
				GuestName:             "TeleMeet Bot",
				PresentMode:           "tab",
				CaptureSource:         "TeleMeet Media",
				JoinTimeoutSeconds:    5,
				PresentTimeoutSeconds: 8,
			},
			log: testLogger{t},
		},
	}

	stage, info := s.meetStage()
	if stage != "prejoin" {
		t.Fatalf("stage = %q (%s), want prejoin", stage, info)
	}

	if !s.fillGuestName() {
		t.Fatal("fillGuestName failed")
	}
	if got := evaluate(t, page, `document.getElementById('name').value`); got != "TeleMeet Bot" {
		t.Fatalf("name field = %q", got)
	}

	if clicked, label := s.clickJoin(); !clicked {
		t.Fatalf("clickJoin failed (label=%q)", label)
	}

	// The fake lobby admits the bot after ~700ms; meetStage must see the lobby
	// first and then the meeting.
	deadline := time.Now().Add(5 * time.Second)
	sawLobby := false
	joined := false
	for time.Now().Before(deadline) {
		stage, info = s.meetStage()
		if stage == "lobby" {
			sawLobby = true
		}
		if stage == "meeting" {
			joined = true
			break
		}
		_ = info
		time.Sleep(100 * time.Millisecond)
	}
	if !sawLobby {
		t.Fatal("lobby stage was never observed")
	}
	if !joined {
		t.Fatalf("never reached the meeting stage (last=%q)", stage)
	}

	if presenting, _ := s.presenting(); presenting {
		t.Fatal("presenting before clicking present")
	}
	if opened, label := s.clickPresentButton(); !opened {
		t.Fatalf("clickPresentButton failed (label=%q)", label)
	}
	if clicked, item := s.clickPresentSource(); !clicked {
		t.Fatalf("clickPresentSource failed (item=%q)", item)
	}
	// The fake shows a share dialog, which must be confirmed before the share is live.
	if presenting, _ := s.presenting(); presenting {
		t.Fatal("presenting before the share dialog was confirmed")
	}
	if confirmed, label := s.clickShareConfirm(); !confirmed {
		t.Fatalf("clickShareConfirm failed (label=%q)", label)
	}
	if presenting, _ := s.presenting(); !presenting {
		t.Fatal("presenting() false after share")
	}

	body := evaluate(t, page, `document.body.innerText`)
	if !strings.Contains(body, "You are presenting") {
		t.Fatalf("banner missing: %q", body)
	}
}
