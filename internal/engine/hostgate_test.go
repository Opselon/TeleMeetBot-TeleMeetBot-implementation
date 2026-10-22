package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gMeetBlockedArticle is Google's host-restriction screen, transcribed from a
// live Meet ("You can't join this video call" full page): it classifies as
// blocked but is waitable — the gate clears once the host starts the meeting.
const gMeetBlockedArticle = "you can't join this video call\nreturn to home screen\nyour meeting is safe\nno one can join a meeting unless invited or admitted by the host\nlearn more"

// TestBlockedFatalPhrase pins the split between terminal rejections (fail
// fast) and Google's host-gated access screen (wait out until the host joins).
// Regression for s-cb2d2ba0: "no one can join" was reported as a terminal
// error although it clears as soon as a host joins.
func TestBlockedFatalPhrase(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		want   string
	}{
		{"google host gate", gMeetBlockedArticle, ""},
		{"no-one-can-join only", "no one can join a meeting unless invited or admitted by the host", ""},
		{"meeting full", "meeting is full\ntry another meeting", "meeting is full"},
		{"ended", "this meeting has ended", "this meeting has ended"},
		{"call ended", "the call has ended", "the call has ended"},
		{"invalid code", "invalid meeting — check your meeting code", "invalid meeting"},
		{"check code", "check your meeting code", "check your meeting code"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockedFatalPhrase(tc.detail); got != tc.want {
				t.Fatalf("blockedFatalPhrase(%q) = %q, want %q", tc.detail, got, tc.want)
			}
		})
	}
}

// TestMeetStageScriptShape guards the rod contract: page scripts are function
// form, never IIFE-wrapped (an IIFE breaks rod's Eval).
func TestMeetStageScriptShape(t *testing.T) {
	js := meetStageJS()
	if !strings.HasPrefix(js, "() => {") {
		t.Fatal("meetStageJS must be function form for rod Eval")
	}
	if strings.HasPrefix(js, "(() =>") {
		t.Fatal("meetStageJS must not be IIFE-wrapped")
	}
	if strings.Contains(js, "})( )") || strings.Contains(js, "})()") {
		t.Fatal("meetStageJS must not self-invoke")
	}
}

// TestIntegrationHostGatedMeetingRecovers drives the host-rejection screen
// through a real Chromium: the first load is blocked ("no one can join…"),
// the bot must stay in the admitting state, re-request entry (reload), and
// reach the meeting stage once Google stops gating — never fail outright.
func TestIntegrationHostGatedMeetingRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(fakeHostGatedPage))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, srv.URL+"/lookup/abc-defg-hij")

	s := &Session{
		ID:       "s-gate",
		MeetURL:  srv.URL + "/lookup/abc-defg-hij",
		state:    StateJoining,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{
				GuestName:          "TeleMeet Bot",
				PresentMode:        "tab",
				JoinTimeoutSeconds: 30,
				AdmitWaitSeconds:   30,
			},
			log: testLogger{t},
		},
	}

	stage, info := s.meetStage()
	if stage != "blocked" {
		t.Fatalf("stage = %q (%s), want blocked", stage, info)
	}
	if phrase := blockedFatalPhrase(info); phrase != "" {
		t.Fatalf("google host gate classified as fatal (%q)", phrase)
	}

	start := time.Now()
	if err := s.joinMeeting(); err != nil {
		t.Fatalf("joinMeeting failed on a host-gated meeting: %v", err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("join took %v; host gate recovery should be quick", d)
	}

	// The bot must have re-requested entry instead of failing on the first
	// rejection: the fake counts loads and only admits from the second one.
	if got := evaluate(t, page, `String(document.body.dataset.visits)`); got == "1" {
		t.Fatal("no re-entry reload happened; the gate was never retried")
	}
}

// TestIntegrationBlockedMeetingFailsFast keeps terminal rejections terminal:
// a "meeting is full" page must fail immediately with a clear reason instead
// of waiting out the whole admit timeout.
func TestIntegrationBlockedMeetingFailsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body>Meeting is full — try a different meeting.</body></html>`))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, srv.URL)

	s := &Session{
		ID:       "s-full",
		MeetURL:  srv.URL,
		state:    StateJoining,
		ctx:      context.Background(),
		meetPage: page,
		log:      testLogger{t},
		mgr: &Manager{
			meetCfg: MeetOptions{
				PresentMode:        "tab",
				JoinTimeoutSeconds: 5,
				AdmitWaitSeconds:   30,
			},
			log: testLogger{t},
		},
	}

	start := time.Now()
	err := s.joinMeeting()
	if err == nil {
		t.Fatal("joinMeeting succeeded on a full meeting")
	}
	if !strings.Contains(err.Error(), "cannot join this meeting") || !strings.Contains(err.Error(), "meeting is full") {
		t.Fatalf("error = %v, want a clear cannot-join reason", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("a fatal rejection took %v; it must fail fast", d)
	}
}
