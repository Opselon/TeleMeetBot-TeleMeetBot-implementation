package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// TestCSRFBlocksEveryStateChange is the regression test for the dashboard's
// request forgery defense. Every state-changing POST was guarded only by the
// signed session cookie: no CSRF token, no Origin check, and not even a
// Content-Type gate — verified that a text/plain POST started a session and
// a foreign-Origin POST stopped one. SameSite=Strict was the only barrier,
// which does nothing against a same-site injection and nothing against the
// day it is relaxed or a legacy browser ignores it.
//
// The defense is layered and stateless: an Origin/Referer equality check
// (a cross-site form or fetch always carries a foreign Origin) plus a JSON
// Content-Type gate (a plain HTML form cannot set a Content-Type header at
// all). This test walks each forged shape and asserts the engine state is
// untouched.
func TestCSRFBlocksEveryStateChange(t *testing.T) {
	s, eng := testServer(t)
	h := s.handler()
	cookie := s.signToken(42, time.Now().Add(time.Hour))

	do := func(method, path, origin, ctype, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if ctype != "" {
			r.Header.Set("Content-Type", ctype)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	startBody := `{"meet_url":"https://meet.google.com/a-bcd-efg","media_url":"https://example.com/m.mp4"}`

	cases := []struct {
		name        string
		path        string
		origin      string
		contentType string
		body        string
	}{
		{"stop with foreign Origin", "/api/session/stop?id=abc", "https://evil.example", "", ""},
		{"stop with foreign Referer", "/api/session/stop?id=abc", "", "", ""},
		{"start with foreign Origin", "/api/session/start", "https://evil.example", "application/json", startBody},
		{"action with foreign Origin", "/api/session/action?id=abc", "https://evil.example", "application/json", `{"action":"pause"}`},
		{"logout with foreign Origin", "/api/auth/logout", "https://evil.example", "", ""},
		// The native form attack: a form cannot set a Content-Type header,
		// so it arrives as text/plain or urlencoded and must be refused.
		{"start as a plain form (text/plain)", "/api/session/start", "https://evil.example", "text/plain", startBody},
		{"start as a urlencoded form", "/api/session/start", "https://evil.example", "application/x-www-form-urlencoded", "meet_url=x&media_url=y"},
		// A same-origin form is still a form: the JSON gate is what stops it.
		{"start as a same-origin form", "/api/session/start", "", "text/plain", startBody},
		{"stop with a malformed Origin", "/api/session/stop?id=abc", "://not-a-url", "", ""},
	}

	// Seed one live session the stop probes aim at.
	eng.sessions["abc"] = &engine.Session{ID: engine.SessionID("abc")}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(eng.started)
			w := do(http.MethodPost, tc.path, tc.origin, tc.contentType, tc.body)
			if w.Code != http.StatusForbidden {
				t.Fatalf("forged %s: code = %d (%s), want 403", tc.path, w.Code, strings.TrimSpace(w.Body.String()))
			}
			if len(eng.started) != before {
				t.Fatalf("forged %s started a session: %d → %d", tc.path, before, len(eng.started))
			}
			if _, ok := eng.sessions["abc"]; !ok {
				t.Fatalf("forged %s removed the live session", tc.path)
			}
		})
	}

	// "stop with foreign Referer" needs an actual Referer to be a distinct
	// path: rerun it with Referer set instead of Origin.
	t.Run("stop with foreign Referer set", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/session/stop?id=abc", strings.NewReader(""))
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		r.Header.Set("Referer", "https://evil.example/page")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("referenced forged stop: code = %d, want 403", w.Code)
		}
		if _, ok := eng.sessions["abc"]; !ok {
			t.Fatal("the forged stop removed the live session")
		}
	})
}

// TestCSRFLegitimateRequestsStillWork is the positive control: the
// dashboard's own client (fetch + JSON + same Origin) must be unaffected.
// A CSRF defense that breaks the app is worse than none.
func TestCSRFLegitimateRequestsStillWork(t *testing.T) {
	s, eng := testServer(t)
	h := s.handler()
	cookie := s.signToken(42, time.Now().Add(time.Hour))

	r := httptest.NewRequest(http.MethodPost, "/api/session/start",
		strings.NewReader(`{"meet_url":"https://meet.google.com/aaa-bbbb-ccc","media_url":"https://example.com/y.mp4"}`))
	r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://dashboard.example")
	r.Host = "dashboard.example"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("legitimate start: code = %d: %s", w.Code, w.Body.String())
	}
	if len(eng.started) != 1 {
		t.Fatalf("legitimate start did not reach the engine: started=%d", len(eng.started))
	}

	// The login route keeps the origin check only: the Telegram widget
	// delivers its signed payload as form data, so a JSON gate there would
	// reject the only legitimate login flow.
	r = httptest.NewRequest(http.MethodPost, "/api/auth/telegram",
		strings.NewReader("id=1&hash=abc"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://dashboard.example")
	r.Host = "dashboard.example"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusForbidden {
		t.Fatalf("form-encoded login rejected by the CSRF gate: %s", w.Body.String())
	}
}

// TestCSRFSameOriginCasing keeps the origin comparison from being brittle:
// hosts are case-insensitive and must match regardless of case.
func TestCSRFSameOriginCasing(t *testing.T) {
	tt := []struct {
		host   string
		origin string
		want   bool
	}{
		{"dashboard.example", "https://dashboard.example", true},
		{"dashboard.example", "https://DASHBOARD.example", true},
		{"dashboard.example:8080", "https://dashboard.example:8080", true},
		{"dashboard.example", "https://evil.example", false},
		{"dashboard.example", "https://dashboard.evil.example", false},
		{"dashboard.example", "https://dashboard.example.evil.example", false},
		{"dashboard.example", "://not-a-url", false},
		{"dashboard.example", "dashboard.example", false}, // scheme-less is treated as foreign
	}
	for _, tc := range tt {
		r := httptest.NewRequest(http.MethodPost, "http://"+tc.host+"/x", nil)
		r.Host = tc.host
		if got := sameOrigin(tc.origin, r); got != tc.want {
			t.Errorf("sameOrigin(%q, host=%q) = %v, want %v", tc.origin, tc.host, got, tc.want)
		}
	}
}
