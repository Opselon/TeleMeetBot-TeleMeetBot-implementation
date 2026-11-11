// Package web exposes a tiny local HTTP surface: /healthz for watchdogs and a
// root page that points at the Telegram dashboard. No control endpoints — the
// dashboard is Telegram-only by design.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/googleauth"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/netcap"
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

// Option configures Server.
type Option func(*Server)

// WithGoogleAuth attaches Google OAuth 2.0 / OpenID Connect callback handling.
func WithGoogleAuth(gauth *googleauth.Manager, onConnected func(userID int64, email, name string)) Option {
	return func(s *Server) {
		s.gauth = gauth
		s.onAuthConnected = onConnected
	}
}

// Server is the HTTP health surface.
type Server struct {
	addr            string
	snap            StatusFunc
	log             Logger
	server          *http.Server
	gauth           *googleauth.Manager
	onAuthConnected func(userID int64, email, name string)
}

// New builds the server (not yet listening).
func New(addr string, snap StatusFunc, log Logger, opts ...Option) *Server {
	if log == nil {
		log = nopLogger{}
	}
	s := &Server{addr: addr, snap: snap, log: log}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// handler builds the router (separate for tests).
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	if s.gauth != nil {
		mux.HandleFunc("/auth/google/callback", s.googleCallback)
	}
	mux.HandleFunc("/", s.index)
	return mux
}

// googleCallback handles the Google OAuth 2.0 / OpenID Connect redirect.
func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src https: data:;")

	q := r.URL.Query()
	state := q.Get("state")
	code := q.Get("code")
	errParam := q.Get("error")

	prof, err := s.gauth.HandleCallback(r.Context(), state, code, errParam)
	if err != nil {
		s.log.Warnf("[PIPELINE:googleauth] web callback error: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Google Sign-In Error — TeleMeetBot</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0f172a; color: #f8fafc; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; padding: 1rem; }
    .card { background: #1e293b; border: 1px solid #ef4444; border-radius: 12px; padding: 2rem; max-width: 480px; width: 100%%; text-align: center; box-shadow: 0 10px 25px rgba(0,0,0,0.5); }
    h1 { color: #ef4444; font-size: 1.5rem; margin-top: 0; }
    p { color: #94a3b8; font-size: 0.95rem; line-height: 1.5; }
    .err-msg { background: #334155; color: #fca5a5; padding: 0.75rem; border-radius: 6px; font-family: monospace; font-size: 0.85rem; word-break: break-all; margin: 1rem 0; }
  </style>
</head>
<body>
  <div class="card">
    <h1>Authentication Failed</h1>
    <p>Could not complete Google authentication:</p>
    <div class="err-msg">%s</div>
    <p>Please return to Telegram and try initiating sign-in again.</p>
  </div>
</body>
</html>`, templateEscape(err.Error()))))
		return
	}

	if s.onAuthConnected != nil {
		go s.onAuthConnected(prof.UserID, prof.Email, prof.Name)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Google Account Connected — TeleMeetBot</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #0f172a; color: #f8fafc; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; padding: 1rem; }
    .card { background: #1e293b; border: 1px solid #10b981; border-radius: 12px; padding: 2.2rem 2rem; max-width: 480px; width: 100%%; text-align: center; box-shadow: 0 10px 25px rgba(0,0,0,0.5); }
    .badge { width: 64px; height: 64px; margin: 0 auto 1rem; background: rgba(16, 185, 129, 0.15); border-radius: 50%%; display: flex; align-items: center; justify-content: center; font-size: 2rem; }
    h1 { color: #10b981; font-size: 1.5rem; margin-top: 0; }
    p { color: #cbd5e1; font-size: 0.95rem; line-height: 1.5; }
    .user-info { background: #334155; padding: 0.85rem; border-radius: 8px; margin: 1.2rem 0; }
    .user-name { font-weight: 600; color: #ffffff; }
    .user-email { color: #94a3b8; font-size: 0.85rem; font-family: monospace; }
    .hint { color: #64748b; font-size: 0.85rem; }
  </style>
</head>
<body>
  <div class="card">
    <div class="badge">&#10003;</div>
    <h1>Account Connected!</h1>
    <p>Your Google account has been authenticated with TeleMeetBot:</p>
    <div class="user-info">
      <div class="user-name">%s</div>
      <div class="user-email">%s</div>
    </div>
    <p class="hint">You can safely close this browser window and return to your Telegram chat.</p>
  </div>
</body>
</html>`, templateEscape(prof.Name), templateEscape(prof.Email))))
}

func templateEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
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

// ServeOn serves on an already-bound listener until ctx is cancelled. Used by
// the --probe diagnosis mode, which owns its binding.
func (s *Server) ServeOn(l net.Listener, ctx context.Context) error {
	s.server = &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(sh)
	}()
	s.log.Infof("health endpoint listening on http://%s/healthz", l.Addr())
	if err := s.server.Serve(l); err != nil && err != http.ErrServerClosed {
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
	// The network capability block is diagnostics: route identifiers and
	// outcomes only, never credentials. It is emitted whenever the engine
	// provided a view, including an all-unknown view (nothing measured yet),
	// so an operator can tell "not measured" apart from "not reported".
	if net := networkPayload(snap.Network); net != nil {
		payload["network"] = net
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

// networkPayload renders the network capability summary as JSON. It returns
// nil only when no route was configured AND nothing was measured — the
// zero-config install, where a "network" key with an empty ladder would be
// noise. Every measured route appears with its outcome and a human-readable
// reason; proxy URLs are identifiers, not credentials.
func networkPayload(s netcap.Summary) map[string]interface{} {
	if s.LastProbeAt.IsZero() && len(s.Proxies) == 0 && s.Direct.Outcome == netcap.OutcomeUnknown && s.Recommended.Route == "" {
		return nil
	}
	out := map[string]interface{}{
		"direct":     outcomePayload(s.Direct),
		"route":      routePayload(s.Recommended),
		"probe_at":   s.LastProbeAt.UTC().Format(time.RFC3339),
		"ladder":     ladderPayload(s),
		"suppressed": s.Suppressed,
		// WebRTC media transport viability: whether real UDP datagrams get
		// answers on this host. "blocked" on a datacenter network means every
		// meeting rides TURN-over-TCP (slower, CPU-heavier) or has no media
		// path at all — the single most useful signal for join failures that
		// a TCP-only probe cannot see.
		"udp": udpPayload(s.UDP),
	}
	return out
}

// udpPayload renders one UDP egress observation. The STUN-mapped address is
// intentionally omitted: /healthz is publicly readable and the mapping would
// publish this host's transport address.
func udpPayload(r netcap.UDPResult) map[string]interface{} {
	outcome := r.Outcome
	if outcome == "" {
		outcome = "unknown"
	}
	m := map[string]interface{}{
		"outcome":   outcome,
		"attempted": r.Attempted,
	}
	if r.LatencyMS > 0 {
		m["latency_ms"] = r.LatencyMS
	}
	if r.Error != "" {
		m["error"] = r.Error
	}
	if !r.CheckedAt.IsZero() {
		m["checked_at"] = r.CheckedAt.UTC().Format(time.RFC3339)
	}
	return m
}

// outcomePayload renders one measured route.
func outcomePayload(r netcap.Result) map[string]interface{} {
	m := map[string]interface{}{
		"outcome": r.Outcome.String(),
	}
	if r.HTTPStatus != 0 {
		m["http_status"] = r.HTTPStatus
	}
	if r.Reason != "" {
		m["reason"] = r.Reason
	}
	if !r.CheckedAt.IsZero() {
		m["checked_at"] = r.CheckedAt.UTC().Format(time.RFC3339)
	}
	return m
}

// routePayload renders the selected playback route.
func routePayload(rec netcap.Recommendation) map[string]interface{} {
	m := map[string]interface{}{
		"via":    "direct",
		"reason": rec.Reason,
	}
	if rec.Route == netcap.RouteProxy {
		m["via"] = "proxy"
		m["proxy"] = rec.Proxy
	}
	return m
}

// ladderPayload renders the configured egress ladder in operator order.
func ladderPayload(s netcap.Summary) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(s.Proxies)+1)
	out = append(out, map[string]interface{}{
		"route":   "direct",
		"outcome": s.Direct.Outcome.String(),
	})
	for _, p := range s.Proxies {
		entry := map[string]interface{}{
			"route":   "proxy",
			"proxy":   p.Proxy,
			"outcome": p.Outcome.String(),
		}
		if p.Reason != "" {
			entry["reason"] = p.Reason
		}
		out = append(out, entry)
	}
	return out
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
