// Package web exposes a tiny local HTTP surface: /healthz for watchdogs and a
// root page that points at the Telegram dashboard. No control endpoints — the
// dashboard is Telegram-only by design.
package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/health"
)

// Logger is the minimal logging surface.
type Logger interface {
	Infof(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

type nopLogger struct{}

func (nopLogger) Infof(string, ...interface{})  {}
func (nopLogger) Warnf(string, ...interface{})  {}
func (nopLogger) Errorf(string, ...interface{}) {}

// StatusFunc produces the current snapshot.
type StatusFunc func() health.Snapshot

// Server is the HTTP health surface.
type Server struct {
	addr   string
	snap   StatusFunc
	log    Logger
	server *http.Server
}

// New builds the server (not yet listening).
func New(addr string, snap StatusFunc, log Logger) *Server {
	if log == nil {
		log = nopLogger{}
	}
	return &Server{addr: addr, snap: snap, log: log}
}

// handler builds the router (separate for tests).
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", s.index)
	return mux
}

// Start listens until ctx is cancelled, then shuts down gracefully.
func (s *Server) Start(ctx context.Context) error {
	s.server = &http.Server{Addr: s.addr, Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(sh)
	}()
	s.log.Infof("health endpoint listening on http://%s/healthz", s.addr)
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// health returns JSON status for watchdogs.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := s.snap()
	payload := map[string]interface{}{
		"status":          "ok",
		"version":         snap.Version,
		"go":              snap.GoVersion,
		"uptime_seconds":  int(snap.Uptime.Seconds()),
		"active_sessions": snap.ActiveCount,
		"max_sessions":    snap.MaxSessions,
		"chromium":        snap.BrowserPath,
		"disk_free_gb":    round1(snap.DiskFreeGB),
		"config":          snap.ConfigPath,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("TeleMeetBot is running.\n\nThe dashboard lives in Telegram.\nHealth: /healthz\n"))
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
