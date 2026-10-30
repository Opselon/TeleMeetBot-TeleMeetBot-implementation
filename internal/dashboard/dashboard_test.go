package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/logx"
)

// ---- fakes ------------------------------------------------------------------

type fakeEngine struct {
	mu       sync.Mutex
	sessions map[string]*engine.Session
	started  []engine.StartRequest
}

func (f *fakeEngine) Start(ctx context.Context, req engine.StartRequest) (*engine.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, req)
	sess := &engine.Session{ID: engine.SessionID(fmt.Sprintf("ses-%d", len(f.started)+1))}
	f.sessions[string(sess.ID)] = sess
	return sess, nil
}
func (f *fakeEngine) Get(id engine.SessionID) *engine.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[string(id)]
}
func (f *fakeEngine) List() []*engine.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*engine.Session, 0, len(f.sessions))
	for _, s := range f.sessions {
		out = append(out, s)
	}
	return out
}
func (f *fakeEngine) Stop(id engine.SessionID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, string(id))
	return nil
}

func testServer(t *testing.T) (*Server, *fakeEngine) {
	t.Helper()
	eng := &fakeEngine{sessions: map[string]*engine.Session{}}
	rec := logx.NewRecorder(64)
	s, err := New(Config{
		Enabled:    true,
		BotToken:   "123:ABC",
		IsAdmin:    func(id int64) bool { return id == 42 },
		SecretPath: "",
	}, Dependencies{
		Engine: eng,
		Logs:   rec,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, eng
}

// signAuth builds a correctly-signed Telegram payload for testing.
func signAuth(t *testing.T, token string, fields map[string]string) map[string]string {
	t.Helper()
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k == "hash" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k + "=" + fields[k])
	}
	key := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(b.String()))
	out := map[string]string{}
	for k, v := range fields {
		out[k] = v
	}
	out["hash"] = hex.EncodeToString(mac.Sum(nil))
	return out
}

// ---- Telegram auth verification --------------------------------------------

func TestVerifyTelegramAuth(t *testing.T) {
	token := "123:ABC"
	now := time.Now()
	fields := map[string]string{
		"id": "42", "first_name": "Ada", "username": "ada",
		"auth_date": fmt.Sprintf("%d", now.Unix()),
	}
	signed := signAuth(t, token, fields)

	a, err := VerifyTelegramAuth(token, signed, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if a.ID != 42 || a.Username != "ada" {
		t.Fatalf("auth = %+v", a)
	}

	// Tampered field.
	tampered := map[string]string{}
	for k, v := range signed {
		tampered[k] = v
	}
	tampered["id"] = "999"
	if _, err := VerifyTelegramAuth(token, tampered, now); err == nil {
		t.Fatal("tampered payload must be rejected")
	}

	// Expired payload (auth_date is signed, so an old payload is stale).
	old := map[string]string{}
	for k, v := range signed {
		old[k] = v
	}
	old["auth_date"] = fmt.Sprintf("%d", now.Add(-25*time.Hour).Unix())
	oldSigned := signAuth(t, token, old)
	if _, err := VerifyTelegramAuth(token, oldSigned, now); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("stale payload must be rejected, got %v", err)
	}

	// Missing hash.
	noHash := map[string]string{"id": "42"}
	if _, err := VerifyTelegramAuth(token, noHash, now); err == nil {
		t.Fatal("missing hash must be rejected")
	}
}

// ---- HTTP flow --------------------------------------------------------------

func doJSON(t *testing.T, h http.Handler, method, path string, body interface{}, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, rdr)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLoginAndAdminGate(t *testing.T) {
	s, _ := testServer(t)
	h := s.handler()
	now := time.Now()

	// Non-admin is rejected even with a valid signature.
	nonAdmin := signAuth(t, "123:ABC", map[string]string{
		"id": "7", "auth_date": fmt.Sprintf("%d", now.Unix()),
	})
	w := doJSON(t, h, http.MethodPost, "/api/auth/telegram", nonAdmin, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin login = %d, want 403", w.Code)
	}

	// Admin gets a cookie; the cookie grants /api/state.
	admin := signAuth(t, "123:ABC", map[string]string{
		"id": "42", "auth_date": fmt.Sprintf("%d", now.Unix()), "username": "root",
	})
	w = doJSON(t, h, http.MethodPost, "/api/auth/telegram", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin login = %d: %s", w.Code, w.Body.String())
	}
	cookie := ""
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie issued")
	}

	// Bad cookie → 401 on the API.
	w = doJSON(t, h, http.MethodGet, "/api/state", nil, "forged.token.sig")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie = %d, want 401", w.Code)
	}
	// Good cookie works.
	w = doJSON(t, h, http.MethodGet, "/api/state", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/state = %d: %s", w.Code, w.Body.String())
	}

	// Rate limiting: 5 failures lock the IP out.
	for i := 0; i < maxLoginFailures; i++ {
		bad := map[string]string{"id": "42", "hash": "deadbeef"}
		doJSON(t, h, http.MethodPost, "/api/auth/telegram", bad, "")
	}
	w = doJSON(t, h, http.MethodPost, "/api/auth/telegram", admin, "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit = %d, want 429", w.Code)
	}
}

func TestSessionControlEndpoints(t *testing.T) {
	s, eng := testServer(t)
	h := s.handler()
	cookie := s.signToken(42, time.Now().Add(time.Hour))

	// Start.
	w := doJSON(t, h, http.MethodPost, "/api/session/start",
		map[string]interface{}{"meet_url": "https://meet.google.com/abc", "media_url": "https://example.com/x.mp4", "mode": "browser"},
		cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("start = %d: %s", w.Code, w.Body.String())
	}
	var started struct {
		Session sessionView `json:"session"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	if started.Session.ID == "" {
		t.Fatalf("no session in response: %s", w.Body.String())
	}
	if len(eng.started) != 1 || eng.started[0].MeetLink != "https://meet.google.com/abc" {
		t.Fatalf("engine got %+v", eng.started)
	}

	// Start validation.
	w = doJSON(t, h, http.MethodPost, "/api/session/start", map[string]string{"meet_url": "x"}, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("partial start = %d, want 400", w.Code)
	}

	// Action on the session.
	w = doJSON(t, h, http.MethodPost, "/api/session/action?id="+started.Session.ID,
		map[string]interface{}{"action": "volume", "arg": 80}, cookie)
	// The fake session has no browser; the action may fail at the player
	// level — but a bad id must 404 and a bad action must 400.
	w404 := doJSON(t, h, http.MethodPost, "/api/session/action?id=nope",
		map[string]interface{}{"action": "pause"}, cookie)
	if w404.Code != http.StatusNotFound {
		t.Fatalf("action on unknown id = %d, want 404", w404.Code)
	}
	w400 := doJSON(t, h, http.MethodPost, "/api/session/action?id="+started.Session.ID,
		map[string]interface{}{"action": "explode"}, cookie)
	if w400.Code != http.StatusBadRequest {
		t.Fatalf("unknown action = %d, want 400", w400.Code)
	}
	_ = w

	// Stop.
	w = doJSON(t, h, http.MethodPost, "/api/session/stop?id="+started.Session.ID, nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("stop = %d: %s", w.Code, w.Body.String())
	}
	if eng.Get(engine.SessionID(started.Session.ID)) != nil {
		t.Fatal("session not removed from the fake engine")
	}
}

func TestStateAndLogs(t *testing.T) {
	s, _ := testServer(t)
	h := s.handler()
	cookie := s.signToken(42, time.Now().Add(time.Hour))
	s.deps.Logs.Record(logx.Entry{Level: logx.LevelError, Component: "bot", Message: "boom"})

	w := doJSON(t, h, http.MethodGet, "/api/logs?level=2&n=10", nil, cookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("logs = %d %s", w.Code, w.Body.String())
	}
	w = doJSON(t, h, http.MethodGet, "/api/logs?component=download", nil, cookie)
	if strings.Contains(w.Body.String(), "boom") {
		t.Fatal("component filter must exclude other components")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	s, _ := testServer(t)
	tok := s.signToken(42, time.Now().Add(time.Hour))
	if id, ok := s.verifyToken(tok); !ok || id != 42 {
		t.Fatalf("verify = %d %v", id, ok)
	}
	// Expired.
	if _, ok := s.verifyToken(s.signToken(42, time.Now().Add(-time.Second))); ok {
		t.Fatal("expired token accepted")
	}
	// Forged signature.
	forged := strings.Split(tok, ".")
	forged[2] = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, ok := s.verifyToken(strings.Join(forged, ".")); ok {
		t.Fatal("forged token accepted")
	}
}
