package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// meetPresentingIndicatorPage reproduces the production failure of
// 2026-10-28 (session s-832a530c, 45s present-flow timeout):
//
//	19:19:38.050 opened present menu (narges yazdanpanah is presenting compute)
//	19:19:39.860 Meet is starting the capture ("presentation is starting...")
//	19:20:23.443 present flow timed out after 45s
//
// Another participant ("narges yazdanpanah") was mid-presentation, so Meet
// showed the always-visible status indicator
// "narges yazdanpanah is presenting compute" as a [role=button] element.
// The real "Share screen" control lives in the auto-hiding call-controls
// bar, which was hidden at that moment. clickPresentButton step 2 therefore
// scored the only visible present-labelled element — the indicator — at 80
// (aria contains "present"), the old exclusion list only rejected
// "stop presenting"/"is starting" and the indicator says neither, so it was
// clicked and its label returned to the caller. No getDisplayMedia was ever
// requested for the bot's own share; "presentation is starting..." was the
// OTHER participant's stream coming up, and the flow waited 45s for a
// stream that could never be delivered to this bot.
//
// The follow-up session s-cea84b7b at 19:30:01 confirms the mechanism: it
// hit the same Meet build with no indicator visible and logged
// "opened present menu (share screen computer_arrow_up)" → stream live.
//
// The page mirrors the real DOM: the controls bar is hidden
// ( Meet hides it until pointer movement) and only the indicator is visible
// at scan time. The toolbar reveals on mousemove, which the production code
// dispatches before scanning, so a correct matcher sees the real Share
// button and clicks it.
const meetPresentingIndicatorPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet - amw-mtyr-opp</title></head>
<body>
  <!-- The auto-hiding call-controls bar. Hidden when the bot scanned, which
       is why the indicator below won the matcher race in production. Meet's
       real auto-hide does not react to a synthetic document-level mousemove
       — it needs a genuine hover over the bar itself — so this page models
       exactly that. -->
  <div id="controls" role="button" aria-label="call controls keyboard_arrow_up audio settings microphone camera present now chat people leave call" style="display:none">
    <button id="mic" aria-label="Turn off microphone">Microphone</button>
    <button id="cam" aria-label="Turn off camera">Camera</button>
    <button id="share" aria-label="Share screen" onclick="startShare()">computer_arrow_up</button>
    <button id="leave" aria-label="Leave call">Leave call</button>
  </div>

  <!-- The always-visible presentation indicator: a label whose accessible
       name carries "present" and which is NOT a share control. -->
  <div id="indicator" role="button" aria-label="narges yazdanpanah is presenting compute">
    narges yazdanpanah is presenting compute
  </div>

  <div id="banner"></div>
<script>
  // Meet reveals the toolbar only on a genuine hover of the bar itself, not
  // on a synthetic document-level mousemove — that is exactly why the
  // production scan saw only the indicator.
  var controls = document.getElementById('controls');
  controls.addEventListener('mouseenter', function () {
    controls.style.display = 'block';
  });
  function startShare() {
    document.getElementById('banner').textContent = 'You are presenting';
    var stop = document.createElement('button');
    stop.setAttribute('aria-label', 'Stop presenting');
    stop.textContent = 'Stop presenting';
    document.body.appendChild(stop);
    window.__shared = true;
  }
  document.getElementById('indicator').addEventListener('click', function () {
    window.__indicatorClicked = true;
  });
</script>
</body></html>`

// TestClickPresentButtonSkipsPresentingIndicator is the regression test for
// the 45s share timeout: when another participant is presenting and the
// toolbar is hidden, the "X is presenting" status indicator must never be
// mistaken for the Present control.
//
// Two outcomes are acceptable: the matcher reveals/clicks the real Share
// button (toolbar woken by the mousemove the production code dispatches),
// or it honestly reports no visible present control. It must never return
// the indicator's label as the matched control — that is the bug, because
// the caller then treats it as an opened present menu and waits for a
// stream that was never requested.
func TestClickPresentButtonSkipsPresentingIndicator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(meetPresentingIndicatorPage))
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
		ID:       "s-presenting-indicator",
		state:    StateSharing,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{PresentMode: "tab", PresentTimeoutSeconds: 8},
			log:     testLogger{t},
		},
	}

	opened, label := s.clickPresentButton()

	// The production bug: the matcher returned the indicator's own label
	// verbatim, and the caller logged "opened present menu (narges
	// yazdanpanah is presenting compute)". Any label carrying the indicator
	// text is the bug regardless of toolbar visibility at scan time.
	if opened && (containsFold(label, "is presenting") || containsFold(label, "narges")) {
		t.Fatalf("clickPresentButton matched the presentation indicator %q instead of the Share control", label)
	}

	if opened {
		// The share must actually start: only the real Share button runs
		// startShare().
		if got := evaluate(t, page, "String(window.__shared === true)"); got != "true" {
			t.Fatalf("clickPresentButton reported %q but the share never started (window.__shared false)", label)
		}
		if got := evaluate(t, page, "String(window.__indicatorClicked === true)"); got == "true" {
			t.Fatal("clickPresentButton clicked the 'is presenting' indicator")
		}
	}
}

func containsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, z := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if z >= 'A' && z <= 'Z' {
				z += 32
			}
			if a != z {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
