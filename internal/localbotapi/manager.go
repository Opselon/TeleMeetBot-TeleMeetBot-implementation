package localbotapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Logger defines the minimal logging interface needed by Manager.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	Debugf(format string, args ...any)
}

type noopLogger struct{}

func (noopLogger) Infof(string, ...any)  {}
func (noopLogger) Warnf(string, ...any)  {}
func (noopLogger) Errorf(string, ...any) {}
func (noopLogger) Debugf(string, ...any) {}

// Config controls the embedded or supervised telegram-bot-api daemon.
type Config struct {
	Enabled    bool   `yaml:"enabled"`
	APIID      int    `yaml:"api_id"`
	APIHash    string `yaml:"api_hash"`
	HTTPPort   int    `yaml:"http_port"`
	Dir        string `yaml:"dir"`
	TempDir    string `yaml:"temp_dir"`
	BinaryPath string `yaml:"binary_path"`
	LogFile    string `yaml:"log_file"`
	Verbosity  int    `yaml:"verbosity"`
}

// Manager supervises a local telegram-bot-api server child process.
type Manager struct {
	cfg  Config
	log  Logger
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

// New creates a new local Bot API supervisor.
func New(cfg Config, log Logger) *Manager {
	if log == nil {
		log = noopLogger{}
	}
	if cfg.HTTPPort <= 0 {
		cfg.HTTPPort = 8081
	}
	if cfg.APIID <= 0 {
		cfg.APIID = 17349
	}
	if cfg.APIHash == "" {
		cfg.APIHash = "344583e45741c457fe1862106095a5eb"
	}
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = "telegram-bot-api"
	}
	return &Manager{
		cfg: cfg,
		log: log,
	}
}

// Endpoint returns the Bot API URL format required by go-telegram-bot-api.
func (m *Manager) Endpoint() string {
	return fmt.Sprintf("http://127.0.0.1:%d/bot%%s/%%s", m.cfg.HTTPPort)
}

// Dir returns the working directory where files are stored in local mode.
func (m *Manager) Dir() string {
	return m.cfg.Dir
}

// IsRunning reports whether the supervised server process is running.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil && m.cmd.Process != nil
}

// Start launches the local telegram-bot-api server and blocks until it is healthy.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cmd != nil {
		return errors.New("telegram-bot-api server is already running")
	}

	bin, err := m.resolveBinary()
	if err != nil {
		return fmt.Errorf("resolve telegram-bot-api binary: %w", err)
	}

	if err := m.ensureDirectories(); err != nil {
		return fmt.Errorf("prepare working directories: %w", err)
	}

	args := []string{
		fmt.Sprintf("--api-id=%d", m.cfg.APIID),
		fmt.Sprintf("--api-hash=%s", m.cfg.APIHash),
		"--local",
		fmt.Sprintf("--http-port=%d", m.cfg.HTTPPort),
		fmt.Sprintf("--dir=%s", m.cfg.Dir),
	}
	if m.cfg.TempDir != "" {
		args = append(args, fmt.Sprintf("--temp-dir=%s", m.cfg.TempDir))
	}
	if m.cfg.LogFile != "" {
		args = append(args, fmt.Sprintf("--log=%s", m.cfg.LogFile))
	}
	if m.cfg.Verbosity > 0 {
		args = append(args, fmt.Sprintf("--verbosity=%d", m.cfg.Verbosity))
	}

	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Dir = m.cfg.Dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	setProcessGroup(cmd)

	m.log.Infof("[PIPELINE:localbotapi] starting local telegram-bot-api server (port=%d, dir=%s)",
		m.cfg.HTTPPort, m.cfg.Dir)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn telegram-bot-api process: %w", err)
	}

	done := make(chan struct{})
	m.cmd = cmd
	m.done = done

	// Background monitor: watches for process exit.
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		m.cmd = nil
		m.mu.Unlock()
		close(done)

		if err != nil {
			m.log.Warnf("[PIPELINE:localbotapi] server process terminated: %v", err)
		} else {
			m.log.Infof("[PIPELINE:localbotapi] server process exited cleanly")
		}
	}()

	// Wait for the HTTP endpoint to become healthy.
	if err := m.waitForHealthy(ctx); err != nil {
		m.mu.Unlock()
		_ = m.Stop()
		m.mu.Lock()
		return fmt.Errorf("health check failed: %w", err)
	}

	m.log.Infof("[PIPELINE:localbotapi] local bot API server healthy on 127.0.0.1:%d (max file size up to 2000 MiB)",
		m.cfg.HTTPPort)
	return nil
}

// Stop cleanly terminates the supervised server process.
func (m *Manager) Stop() error {
	m.mu.Lock()
	cmd := m.cmd
	done := m.done
	m.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	m.log.Infof("[PIPELINE:localbotapi] shutting down local telegram-bot-api server...")

	terminateProcess(cmd)

	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
		m.log.Warnf("[PIPELINE:localbotapi] process did not exit within 3s, sending SIGKILL")
		killProcess(cmd)
		<-done
		return nil
	}
}

func (m *Manager) resolveBinary() (string, error) {
	// 1. Direct path check
	if filepath.IsAbs(m.cfg.BinaryPath) {
		if _, err := os.Stat(m.cfg.BinaryPath); err == nil {
			return m.cfg.BinaryPath, nil
		}
		return "", fmt.Errorf("binary not found at %s", m.cfg.BinaryPath)
	}

	// 2. Look in PATH
	if p, err := exec.LookPath(m.cfg.BinaryPath); err == nil {
		return p, nil
	}

	// 3. Fallback standard container paths
	standardPaths := []string{
		"/usr/local/bin/telegram-bot-api",
		"/usr/bin/telegram-bot-api",
	}
	for _, p := range standardPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("telegram-bot-api binary not found (looked for %s and standard paths)", m.cfg.BinaryPath)
}

func (m *Manager) ensureDirectories() error {
	if m.cfg.Dir == "" {
		m.cfg.Dir = filepath.Join(os.TempDir(), "telegram-bot-api")
	}
	if err := os.MkdirAll(m.cfg.Dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", m.cfg.Dir, err)
	}
	if m.cfg.TempDir != "" {
		if err := os.MkdirAll(m.cfg.TempDir, 0o700); err != nil {
			return fmt.Errorf("create temp dir %s: %w", m.cfg.TempDir, err)
		}
	}
	return nil
}

func (m *Manager) waitForHealthy(ctx context.Context) error {
	probeURL := fmt.Sprintf("http://127.0.0.1:%d/", m.cfg.HTTPPort)
	client := &http.Client{
		Timeout: 250 * time.Millisecond,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: (&net.Dialer{
				Timeout: 200 * time.Millisecond,
			}).DialContext,
		},
	}

	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.done:
			return fmt.Errorf("process exited unexpectedly during startup")
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				// Any response from the HTTP listener (including 404) confirms the server is up.
				return nil
			}
			lastErr = err
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("probe %s timed out: %v", probeURL, lastErr)
}

// EnvOverrides populates Config from standard environment variables.
func (c *Config) EnvOverrides() {
	if v := os.Getenv("TELEMEETBOT_TELEGRAM_LOCAL_API_ENABLED"); v != "" {
		c.Enabled = parseBool(v)
	} else if v := os.Getenv("TELEGRAM_LOCAL"); v != "" {
		c.Enabled = parseBool(v)
	}

	if v := os.Getenv("TELEMEETBOT_TELEGRAM_API_ID"); v != "" {
		if id, err := strconv.Atoi(v); err == nil && id > 0 {
			c.APIID = id
		}
	} else if v := os.Getenv("TELEGRAM_API_ID"); v != "" {
		if id, err := strconv.Atoi(v); err == nil && id > 0 {
			c.APIID = id
		}
	}

	if v := os.Getenv("TELEMEETBOT_TELEGRAM_API_HASH"); v != "" {
		c.APIHash = v
	} else if v := os.Getenv("TELEGRAM_API_HASH"); v != "" {
		c.APIHash = v
	}

	if v := os.Getenv("TELEMEETBOT_TELEGRAM_LOCAL_API_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil && port > 0 {
			c.HTTPPort = port
		}
	} else if v := os.Getenv("TELEGRAM_HTTP_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil && port > 0 {
			c.HTTPPort = port
		}
	}

	if v := os.Getenv("TELEMEETBOT_TELEGRAM_LOCAL_API_DIR"); v != "" {
		c.Dir = v
	} else if v := os.Getenv("TELEGRAM_WORK_DIR"); v != "" {
		c.Dir = v
	}
}

func parseBool(s string) bool {
	switch s {
	case "1", "t", "T", "true", "TRUE", "True", "yes", "YES", "on", "ON":
		return true
	default:
		return false
	}
}
