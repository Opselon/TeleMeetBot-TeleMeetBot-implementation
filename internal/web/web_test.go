package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	resp, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
