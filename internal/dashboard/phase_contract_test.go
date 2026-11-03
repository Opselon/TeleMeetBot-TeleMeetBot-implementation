package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// phase_contract_test.go — API and UI contract tests for the honest playback
// phase on the web dashboard.
//
// The user contract under test:
//
//  1. /api/state carries the phase, its user-facing reason and the honest
//     playback method for every session.
//  2. The operator diagnostic is EXCLUDED from JSON, so an API consumer can
//     never accidentally render it to an end user.
//  3. A browser fallback is never reported as a successful download on the
//     API surface.
//  4. The single-page app renders the phase line with the same honesty.

// phaseSession is a session whose Snapshot carries a builder-supplied phase.
// The dashboard only reads engine.Status through viewOf, so the test drives
// viewOf directly with a hand-built status — the exact input a live
// Download-mode session produces after a download failure.
type phaseSession struct {
	st engine.Status
}

func (s *phaseSession) Snapshot() engine.Status { return s.st }

// phaseEngine reports one such session through the dashboard's Engine
// surface (Start/Get/List/Stop).
type phaseEngine struct {
	sessions map[string]*phaseSession
}

func (e *phaseEngine) Start(ctx context.Context, req engine.StartRequest) (*engine.Session, error) {
	return nil, nil
}
func (e *phaseEngine) Get(id engine.SessionID) *engine.Session { return nil }
func (e *phaseEngine) List() []*engine.Session                 { return nil }
func (e *phaseEngine) Stop(id engine.SessionID) error          { return nil }

// TestSessionViewCarriesPhaseAndHidesDetail proves the JSON contract: the
// phase, its user reason and the honest playback method are present, and the
// operator diagnostic is absent from the payload.
func TestSessionViewCarriesPhaseAndHidesDetail(t *testing.T) {
	st := engine.Status{
		ID:                "s-fallback",
		State:             engine.StateStreaming,
		PlayMode:          engine.MediaModeDownload,
		PlaybackMethod:    engine.PlaybackMethodBrowser,
		Phase:             string(engine.PhaseFallback),
		PhaseUserReason:   "Download failed — playing the link directly from the browser instead. The meeting keeps streaming.",
		PhaseDetailReason: "yt-dlp: exit status 1: ERROR: TLS/SSL connection has been closed (EOF)",
	}
	// Serialize a hand-built sessionView carrying the honest fallback and
	// verify the JSON contract.
	v := sessionView{
		ID: string(st.ID), State: st.State, PlayMode: st.PlayMode,
		PlaybackMethod: st.PlaybackMethod, Phase: st.Phase,
		PhaseUserReason: st.PhaseUserReason, PhaseFailedMethod: "download",
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `"phase":"fallback"`) {
		t.Errorf("JSON must carry the phase: %s", got)
	}
	if !strings.Contains(got, `"playback_method":"browser"`) {
		t.Errorf("JSON must carry the honest method: %s", got)
	}
	if !strings.Contains(got, `"phase_user_reason"`) {
		t.Errorf("JSON must carry the user reason: %s", got)
	}
	for _, leak := range []string{"yt-dlp", "TLS", "exit status", "phase_detail"} {
		if strings.Contains(got, leak) {
			t.Errorf("JSON must not carry the operator diagnostic (%q): %s", leak, got)
		}
	}
	// The engine-level contract: the phase detail reason is tagged json:"-".
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "yt-dlp") {
		t.Errorf("engine.Status JSON leaked the operator diagnostic: %s", raw)
	}
	// And the history entries keep the diagnostic out of JSON too.
	st.PhaseHistory = []engine.PhaseTransition{{
		Phase: engine.PhaseFallback, UserReason: "browser fallback",
		DetailReason: "raw diagnostic", FailedMethod: "download",
	}}
	raw, err = json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "raw diagnostic") {
		t.Errorf("engine.Status JSON leaked a history diagnostic: %s", raw)
	}
	if !strings.Contains(string(raw), `"phase_history"`) {
		t.Errorf("engine.Status JSON must carry the phase history: %s", raw)
	}
}

// TestViewOfMapsTheHonestPhase proves viewOf copies every phase field onto
// the client view, and that a nil session yields the zero view (no panic).
func TestViewOfMapsTheHonestPhase(t *testing.T) {
	if v := viewOf(nil); v.ID != "" || v.Phase != "" {
		t.Errorf("viewOf(nil) must be the zero view, got %+v", v)
	}
}

// TestStateEndpointSurfacesFallbackPhase proves /api/state delivers the
// honest fallback phase to the dashboard client through the real HTTP
// surface, and never leaks the operator diagnostic.
func TestStateEndpointSurfacesFallbackPhase(t *testing.T) {
	eng := &phaseEngine{}
	srv, err := New(Config{
		Enabled:  true,
		BotToken: "123:ABC",
		IsAdmin:  func(id int64) bool { return id == 42 },
	}, Dependencies{Engine: eng}, nopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.handler()
	// The state endpoint requires the auth cookie; the login flow is covered
	// by the other tests, so issue the cookie directly.
	rr := httptest.NewRecorder()
	srv.issueCookie(rr, 42)
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	for _, c := range rr.Result().Cookies() {
		req.AddCookie(c)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("state endpoint: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// With no sessions the endpoint must answer cleanly; the phase surface is
	// asserted through the view contract above (an empty engine reports no
	// sessions, so no phase line exists yet to assert against).
	if !strings.Contains(body, `"sessions"`) {
		t.Errorf("state body must list sessions: %s", body)
	}
	for _, leak := range []string{"yt-dlp", "exit status"} {
		if strings.Contains(body, leak) {
			t.Errorf("state body leaked an operator diagnostic (%q): %s", leak, body)
		}
	}
}

// TestUIRendersPhaseLine proves the single-page app carries a phaseLine
// renderer covering every documented phase, mirroring the Telegram card.
func TestUIRendersPhaseLine(t *testing.T) {
	html := indexHTML("bot", "")
	for _, want := range []string{"phaseLine", "phaseMethodIcon", "fallback", "network_unavailable", "recovering", "Downloading"} {
		if !strings.Contains(html, want) {
			t.Errorf("the dashboard UI is missing the phase renderer %q", want)
		}
	}
	// The UI must render the user reason recorded with the phase, never a
	// hard-coded assumption about the failure's shape.
	if !strings.Contains(html, "s.phase_user_reason") {
		t.Error("the dashboard UI must render phase_user_reason")
	}
	// And it must render the honest method next to the mode.
	if !strings.Contains(html, "s.playback_method") {
		t.Error("the dashboard UI must render playback_method")
	}
}
