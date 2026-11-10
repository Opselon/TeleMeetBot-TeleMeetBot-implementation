package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeTransportPC is the fixture shape of the publisher hook's media-transport
// ground truth: a CONNECTED RTCPeerConnection whose senders carry a live track
// — what Meet builds only inside the call. Bare connections (the prejoin
// screen's capability checks) must never count; see
// TestIntegrationPrejoinPeerConnectionsDoNotImpersonateMeeting.
const fakeTransportPC = `{connectionState: 'connected', getSenders: function () { return [{track: {readyState: 'live'}}]; }}`

// fakeOpenMeetingPage mimics Google Meet's open-meeting guest flow: the
// prejoin screen offers "Join now" (quick access on — no admission needed) and
// clicking it lands directly in the call. The join click swaps the DOM to the
// meeting UI and records the peer-connection ground truth the publisher hook
// normally installs (meetPeerHookJS) — a connected connection carrying live
// tracks exists only inside the call.
const fakeOpenMeetingPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet</title></head>
<body>
  <div id="prejoin">
    <input placeholder="Your name" aria-label="Your name">
    <button id="join" aria-label="Join now">Join now</button>
  </div>
  <div id="call" style="display:none">
    <button id="leave" aria-label="Leave call">Leave call</button>
    <span>Meeting details</span>
  </div>
<script>
  var joined = false;
  document.getElementById('join').addEventListener('click', function () {
    if (joined) return;
    joined = true;
    document.getElementById('prejoin').style.display = 'none';
    document.getElementById('call').style.display = 'block';
    window.__tmPCs = [` + fakeTransportPC + `];
  });
</script>
</body></html>`

// fakeKnockPage mimics a meeting that requires admission: the prejoin screen
// offers "Ask to join", clicking it moves the bot to the knock lobby ("Asking
// to be let in…"), and the host admits after three seconds — the same states
// the join loop must distinguish and wait through on the real Meet.
const fakeKnockPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet</title></head>
<body>
  <div id="prejoin">
    <button id="knock" aria-label="Ask to join">Ask to join</button>
  </div>
  <div id="lobby" style="display:none">Asking to be let in&hellip; You'll join when someone lets you in.</div>
  <div id="call" style="display:none">
    <button id="leave" aria-label="Leave call">Leave call</button>
  </div>
<script>
  var knocked = false;
  document.getElementById('knock').addEventListener('click', function () {
    if (knocked) return;
    knocked = true;
    document.getElementById('prejoin').style.display = 'none';
    document.getElementById('lobby').style.display = 'block';
    setTimeout(function () {
      document.getElementById('lobby').style.display = 'none';
      document.getElementById('call').style.display = 'block';
      window.__tmPCs = [` + fakeTransportPC + `];
    }, 3000);
  });
</script>
</body></html>`

// fakePrejoinCapabilityPage mimics the FALSE POSITIVE the live probe caught
// (real meeting, 2026-11-10): Meet's prejoin screen creates capability-check
// RTCPeerConnections. Treating bare connections as "in the meeting" made the
// bot claim "[4/6] Admitted to meeting" without ever clicking Join — and no
// participant ever saw the bot. The page must classify as prejoin.
const fakePrejoinCapabilityPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet</title></head>
<body>
  <div id="prejoin">
    <input placeholder="Your name" aria-label="Your name">
    <button id="join" aria-label="Join now">Join now</button>
  </div>
<script>
  window.__tmPCs = [
    {},
    {connectionState: 'connected'},
    {connectionState: 'connected', getSenders: function () { return [{track: null}]; }}
  ];
</script>
</body></html>`

// TestIntegrationOpenMeetingJoinsWithoutHostWait is the regression for the
// user's report: an open meeting ("Join now" prejoin, no admission) used to be
// labeled "[4/6] Waiting for host to admit bot" the moment the join button was
// clicked — so a join that took longer than expected (or that Google refused)
// showed a host-wait for the whole admit window even though the meeting was
// open for everyone. The open-meeting click must report "Joining the
// meeting…", reach the meeting stage, and never emit a host-wait state.
func TestIntegrationOpenMeetingJoinsWithoutHostWait(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(fakeOpenMeetingPage))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, srv.URL+"/lookup/abc-defg-hij")

	mgr := &Manager{
		meetCfg: MeetOptions{
			GuestName:          "TeleMeet Bot",
			PresentMode:        "tab",
			JoinTimeoutSeconds: 30,
			AdmitWaitSeconds:   30,
		},
		log:      testLogger{t},
		events:   make(chan Event, 128),
		sessions: map[SessionID]*Session{},
	}
	s := &Session{
		ID:       "s-open",
		MeetURL:  srv.URL + "/lookup/abc-defg-hij",
		state:    StateJoining,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr:      mgr,
	}

	start := time.Now()
	if err := s.joinMeeting(); err != nil {
		t.Fatalf("joinMeeting failed on an open meeting: %v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("open-meeting join took %v; the Join now click must land fast", d)
	}

	var joining bool
	for {
		select {
		case ev := <-mgr.events:
			if ev.Kind != EventState {
				continue
			}
			if strings.Contains(strings.ToLower(ev.Text), "waiting for host") {
				t.Fatalf("open meeting mislabeled as host-gated: %q", ev.Text)
			}
			if strings.Contains(ev.Text, "Joining the meeting") {
				joining = true
			}
			continue
		default:
		}
		break
	}
	if !joining {
		t.Fatal("the join click never reported \"Joining the meeting…\" — the honest open-meeting state is missing")
	}
}

// TestIntegrationKnockAdmissionWaitsThenJoins pins the genuine admission flow:
// "Ask to join" must move the bot to the knock lobby, the card must say
// "Waiting for host to admit bot" while the bot waits, and the join must
// complete when the host admits. This is the state the open-meeting flow must
// NOT borrow (see TestIntegrationOpenMeetingJoinsWithoutHostWait).
func TestIntegrationKnockAdmissionWaitsThenJoins(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(fakeKnockPage))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, srv.URL+"/lookup/abc-defg-hij")

	mgr := &Manager{
		meetCfg: MeetOptions{
			GuestName:          "TeleMeet Bot",
			PresentMode:        "tab",
			JoinTimeoutSeconds: 30,
			AdmitWaitSeconds:   30,
		},
		log:      testLogger{t},
		events:   make(chan Event, 128),
		sessions: map[SessionID]*Session{},
	}
	s := &Session{
		ID:       "s-knock",
		MeetURL:  srv.URL + "/lookup/abc-defg-hij",
		state:    StateJoining,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr:      mgr,
	}

	if err := s.joinMeeting(); err != nil {
		t.Fatalf("joinMeeting failed on a knocking meeting: %v", err)
	}

	var waitedForAdmission bool
	for {
		select {
		case ev := <-mgr.events:
			if ev.Kind != EventState {
				continue
			}
			if strings.Contains(ev.Text, "Waiting for host to admit bot") {
				waitedForAdmission = true
			}
			continue
		default:
		}
		break
	}
	if !waitedForAdmission {
		t.Fatal("the knock lobby never reported \"Waiting for host to admit bot\" — real admission waits are indistinguishable from open joins")
	}
}

// TestJoinClickRetryPolicy pins the label-aware retry cadence: an open
// meeting's "Join now" must be retried quickly (a swallowed click costs
// seconds of join latency), while "Ask to join" knocks are spaced wider —
// Meet registers knocks server-side and throttles hammering.
func TestJoinClickRetryPolicy(t *testing.T) {
	cases := []struct {
		name  string
		label string
		want  time.Duration
	}{
		{"open meeting join now", "join now", openReclickWait},
		{"open meeting uppercase", "JOIN NOW", openReclickWait},
		{"knock ask to join", "ask to join", knockReclickWait},
		{"empty label", "", openReclickWait},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clickRetryWait(tc.label); got != tc.want {
				t.Fatalf("clickRetryWait(%q) = %v, want %v", tc.label, got, tc.want)
			}
		})
	}
}

// TestIntegrationPrejoinPeerConnectionsDoNotImpersonateMeeting is the
// regression for the user's live report: the bot announced "[4/6] Admitted to
// meeting" ~250ms after the page opened, without ever clicking Join, because
// Meet's prejoin screen creates capability-check RTCPeerConnections and the
// classifier counted bare connections as in-meeting ground truth. A prejoin
// page carrying bare or trackless connected connections must classify as
// prejoin — the bot must go on to click Join and let the participant-grade
// self-check (verifyInMeeting) confirm real admission.
func TestIntegrationPrejoinPeerConnectionsDoNotImpersonateMeeting(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(fakePrejoinCapabilityPage))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, srv.URL+"/lookup/abc-defg-hij")

	s := &Session{
		ID:       "s-capability",
		MeetURL:  srv.URL + "/lookup/abc-defg-hij",
		state:    StateJoining,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{GuestName: "TeleMeet Bot", PresentMode: "tab"},
			log:     testLogger{t},
		},
	}

	stage, info := s.meetStage()
	if stage != "prejoin" {
		t.Fatalf("capability-check peer connections impersonated the meeting: stage=%q (%s), want prejoin", stage, info)
	}

	ok, why := s.verifyInMeeting()
	if ok {
		t.Fatalf("verifyInMeeting confirmed a prejoin page as in-meeting (%s)", why)
	}
}

// TestAwaitMeetStageScriptShape guards the waiter's rod contract the same way
// the one-shot classifier is guarded: function form, no IIFE, and the embedded
// classifier must be present verbatim.
func TestAwaitMeetStageScriptShape(t *testing.T) {
	js := awaitMeetStageJS(joinPollWait, joinPollTick)
	if !strings.HasPrefix(js, "() => new Promise(") {
		t.Fatal("awaitMeetStageJS must be function form for rod Eval")
	}
	if strings.Contains(js, "})()") || strings.Contains(js, "})( )") {
		t.Fatal("awaitMeetStageJS must not self-invoke")
	}
	if !strings.Contains(js, "resolve(r)") || !strings.Contains(js, "setInterval") {
		t.Fatal("awaitMeetStageJS lost its in-page observation loop")
	}
	// The baked-in timings must be present as integers (ms).
	if !strings.Contains(js, "1200") || !strings.Contains(js, "250") {
		t.Fatal("awaitMeetStageJS must bake the max wait and tick timings in")
	}
}
