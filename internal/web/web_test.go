package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/googleauth"
	"github.com/Opselon/TeleMeetBot/internal/health"
)

func testServer(active int) *httptest.Server {
	snap := health.Snapshot{
		Version:     "test",
		GoVersion:   "go-test",
		BrowserPath: "/usr/bin/chromium",
		ActiveCount: active,
		MaxSessions: 4,
		DiskFreeGB:  9.256,
		ConfigPath:  "/tmp/config.yml",
	}
	s := New("127.0.0.1:0", func() health.Snapshot { return snap }, nil)
	return httptest.NewServer(s.handler())
}

func TestHealthzReportsJSON(t *testing.T) {
	ts := testServer(2)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload["active_sessions"].(float64) != 2 || payload["version"] != "test" {
		t.Fatalf("payload = %+v", payload)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatal("healthz must not be HTML")
	}
}

func TestLivezAndIndex(t *testing.T) {
	ts := testServer(0)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/livez")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("livez = %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "Telegram") {
		t.Fatalf("index = %q", string(buf[:n]))
	}
}

func TestUnknownPathIs404(t *testing.T) {
	ts := testServer(0)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/something-unknown")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGoogleOAuthCallbackWebHandler(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "web-oauth-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	gauth, err := googleauth.New(tmpDir, googleauth.Config{
		ClientID:     "web-client-id",
		ClientSecret: "web-secret",
		RedirectURL:  "http://localhost:18080/auth/google/callback",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var notifiedUser int64
	var notifiedEmail string
	onConnected := func(userID int64, email, name string) {
		notifiedUser = userID
		notifiedEmail = email
	}
	_ = notifiedUser
	_ = notifiedEmail

	snap := health.Snapshot{Version: "test"}
	srv := New("127.0.0.1:0", func() health.Snapshot { return snap }, nil, WithGoogleAuth(gauth, onConnected))
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	// 1. Missing or invalid state returns 400 with clean error page
	resp, err := http.Get(ts.URL + "/auth/google/callback?state=invalid-state&code=test-code")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Authentication Failed") {
		t.Fatalf("expected error page, got: %s", string(body))
	}

	// 2. Error from provider returns 400
	respErr, err := http.Get(ts.URL + "/auth/google/callback?error=access_denied")
	if err != nil {
		t.Fatal(err)
	}
	defer respErr.Body.Close()
	if respErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", respErr.StatusCode)
	}
}
