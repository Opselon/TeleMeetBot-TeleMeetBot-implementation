package localbotapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	port := os.Getenv("HELPER_PORT")
	if port == "" {
		port = "8081"
	}
	server := &http.Server{
		Addr: "127.0.0.1:" + port,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":404,"description":"Not Found"}`))
		}),
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		_ = server.ListenAndServe()
	}()
	<-sigCh
	_ = server.Close()
	os.Exit(0)
}

func TestNewDefaults(t *testing.T) {
	m := New(Config{}, nil)
	if m.cfg.HTTPPort != 8081 {
		t.Errorf("expected default port 8081, got %d", m.cfg.HTTPPort)
	}
	if m.cfg.APIID != 17349 {
		t.Errorf("expected default API ID 17349, got %d", m.cfg.APIID)
	}
	if m.cfg.APIHash != "344583e45741c457fe1862106095a5eb" {
		t.Errorf("expected default API hash, got %s", m.cfg.APIHash)
	}
	if m.Endpoint() != "http://127.0.0.1:8081/bot%s/%s" {
		t.Errorf("unexpected endpoint format: %s", m.Endpoint())
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("TELEMEETBOT_TELEGRAM_LOCAL_API_ENABLED", "true")
	t.Setenv("TELEMEETBOT_TELEGRAM_API_ID", "999888")
	t.Setenv("TELEMEETBOT_TELEGRAM_API_HASH", "mycustomhash")
	t.Setenv("TELEMEETBOT_TELEGRAM_LOCAL_API_PORT", "9090")
	t.Setenv("TELEMEETBOT_TELEGRAM_LOCAL_API_DIR", "/tmp/custom-tbapi-dir")

	var cfg Config
	cfg.EnvOverrides()

	if !cfg.Enabled {
		t.Error("expected Enabled=true")
	}
	if cfg.APIID != 999888 {
		t.Errorf("expected APIID 999888, got %d", cfg.APIID)
	}
	if cfg.APIHash != "mycustomhash" {
		t.Errorf("expected APIHash 'mycustomhash', got %s", cfg.APIHash)
	}
	if cfg.HTTPPort != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.HTTPPort)
	}
	if cfg.Dir != "/tmp/custom-tbapi-dir" {
		t.Errorf("expected dir '/tmp/custom-tbapi-dir', got %s", cfg.Dir)
	}
}

func TestManagerResolveBinaryNotFound(t *testing.T) {
	m := New(Config{
		BinaryPath: "/nonexistent/path/to/telegram-bot-api",
	}, nil)

	_, err := m.resolveBinary()
	if err == nil {
		t.Error("expected error for nonexistent binary path, got nil")
	}
}

func TestManagerLifecycleWithMockBinary(t *testing.T) {
	tmpDir := t.TempDir()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	scriptPath := filepath.Join(tmpDir, "mock-tbapi.sh")
	scriptContent := fmt.Sprintf(`#!/bin/sh
export GO_WANT_HELPER_PROCESS=1
export HELPER_PORT=%d
exec "%s" -test.run=^TestHelperProcess$ -- "$@"
`, port, testBin)

	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	m := New(Config{
		Enabled:    true,
		APIID:      12345,
		APIHash:    "dummyhash",
		HTTPPort:   port,
		Dir:        filepath.Join(tmpDir, "data"),
		BinaryPath: scriptPath,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := m.Start(ctx); err != nil {
		t.Fatalf("m.Start failed: %v", err)
	}

	if !m.IsRunning() {
		t.Error("expected m.IsRunning() to be true")
	}

	// Verify HTTP probe
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("HTTP get failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}

	// Stop manager
	if err := m.Stop(); err != nil {
		t.Errorf("m.Stop failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if m.IsRunning() {
		t.Error("expected m.IsRunning() to be false after Stop")
	}
}

func TestManagerStartupFailureQuickExit(t *testing.T) {
	tmpDir := t.TempDir()

	scriptPath := filepath.Join(tmpDir, "failing-tbapi.sh")
	scriptContent := `#!/bin/sh
echo "Fatal: invalid credentials" >&2
exit 1
`
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0o755); err != nil {
		t.Fatalf("write mock script: %v", err)
	}

	m := New(Config{
		Enabled:    true,
		HTTPPort:   49152,
		Dir:        filepath.Join(tmpDir, "data"),
		BinaryPath: scriptPath,
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.Start(ctx)
	if err == nil {
		t.Fatal("expected Start() to fail when process exits immediately, got nil")
	}
	_ = m.Stop()
}
