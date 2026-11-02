// Package dashboard is TeleMeetBot's light web dashboard: everything the
// Telegram side can do, driven from a browser.
//
// Login is Telegram's official Login Widget: the browser authenticates
// against Telegram, and the dashboard receives the signed payload
// (id, username, auth_date, hash). The hash is an HMAC-SHA256 over the
// alphabetically ordered fields keyed with SHA256(bot_token) — verification
// here is the same check Telegram documents, so a forged identity can never
// get in. The verified Telegram id must ALSO be on the admin allow-list
// (fail-closed, exactly like the bot): the dashboard has full control of the
// engine, so it must never trust "logged in with Telegram" alone.
//
// Sessions are HMAC-signed cookies (no server-side store to reset), keyed by
// a secret persisted next to the config. The API is a small JSON surface;
// the UI is one embedded page with zero dependencies.
package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/logx"
	"github.com/Opselon/TeleMeetBot/internal/userstate"
)

// Config controls the dashboard server.
type Config struct {
	Enabled bool
	// Addr is the listen address, ":8080" style.
	Addr string
	// BotToken verifies Telegram Login payloads (never leaves the process).
	BotToken string
	// BotUsername is the bot's @username (fetched at startup from getMe).
	BotUsername string
	// PublicBaseURL is the origin users open in a browser (used by the
	// Telegram widget's origin hint). Empty = same-origin relative URLs.
	PublicBaseURL string
	// SessionTTL bounds a login cookie's lifetime. Zero = 24h.
	SessionTTL time.Duration
	// SecretPath is where the cookie-signing secret is persisted. Empty =
	// no persistence (cookies survive only the process).
	SecretPath string
	// IsAdmin decides whether a Telegram id may use the dashboard. It must
	// be the same allow-list the bot enforces (fail-closed).
	IsAdmin func(userID int64) bool
}

// cookieName is the dashboard session cookie.
const cookieName = "tmb_session"

// Login rate limit: 5 failed attempts per IP per 15 minutes. The dashboard
// is admin-only; brute force has no place here.
const (
	maxLoginFailures   = 5
	loginFailureWindow = 15 * time.Minute
)

// Dependencies wires the dashboard to the application. Concrete internal
// types keep the surface small and honest; tests instantiate the real
// Recorder/Tracker and a fake engine.
type Dependencies struct {
	// Health produces the health snapshot for the overview.
	Health func() health.Snapshot
	// Engine starts/stops/lists sessions.
	Engine EngineAPI
	// Logs is the central log recorder (may be nil: log pages hide).
	Logs *logx.Recorder
	// Users is the per-user state tracker (may be nil).
	Users *userstate.Tracker
	// Media is the download manager status surface (may be nil).
	Media MediaAPI
}

// EngineAPI is the engine surface the dashboard drives. engine.Manager
// implements it directly.
type EngineAPI interface {
	Start(ctx context.Context, req engine.StartRequest) (*engine.Session, error)
	Get(id engine.SessionID) *engine.Session
	List() []*engine.Session
	Stop(id engine.SessionID) error
}

// MediaAPI is the download manager surface used for the downloads card.
type MediaAPI interface {
	Snapshot() interface{}
}

// Server is the web dashboard.
type Server struct {
	cfg    Config
	deps   Dependencies
	log    Logger
	secret []byte

	mu       sync.Mutex
	failures map[string][]time.Time // ip -> failed attempts
	// lastLoginGC bounds the login-path sweep (gcLoginFailuresIfDue);
	// lastEvict amortizes the hard-cap evictions in recordLoginFailure.
	lastLoginGC time.Time
	lastEvict   time.Time

	server *http.Server
}

// Logger is the logging surface.
type Logger interface {
	Infof(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

type nopLogger struct{}

func (nopLogger) Infof(string, ...interface{})  {}
func (nopLogger) Warnf(string, ...interface{})  {}
func (nopLogger) Errorf(string, ...interface{}) {}

// New builds the dashboard server.
func New(cfg Config, deps Dependencies, log Logger) (*Server, error) {
	if log == nil {
		log = nopLogger{}
	}
	if strings.TrimSpace(cfg.BotToken) == "" {
		return nil, fmt.Errorf("dashboard: bot token is required (login verification)")
	}
	if cfg.IsAdmin == nil {
		return nil, fmt.Errorf("dashboard: IsAdmin is required (fail-closed admin allow-list)")
	}
	s := &Server{
		cfg:      cfg,
		deps:     deps,
		log:      log,
		failures: map[string][]time.Time{},
	}
	secret, err := loadOrCreateSecret(cfg.SecretPath)
	if err != nil {
		return nil, err
	}
	s.secret = secret
	return s, nil
}

// loadOrCreateSecret reads (or creates) the cookie-signing secret. The
// secret is random 32 bytes; regenerating it only invalidates old cookies,
// so a missing or corrupt file is never fatal beyond that.
func loadOrCreateSecret(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return randomSecret()
	}
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	secret, err := randomSecret()
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		return nil, fmt.Errorf("persist dashboard secret: %w", err)
	}
	return secret, nil
}

func randomSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// ---- session tokens --------------------------------------------------------

// signToken returns "userid.expiry.sig" where sig = HMAC(secret, "userid.expiry").
func (s *Server) signToken(userID int64, expiry time.Time) string {
	payload := fmt.Sprintf("%d.%d", userID, expiry.Unix())
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifyToken validates a session token and returns the user id.
func (s *Server) verifyToken(token string) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, false
	}
	payload := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(parts[2])
	if err != nil || !hmac.Equal(got, want) {
		return 0, false
	}
	var userID, expiryUnix int64
	if _, err := fmt.Sscanf(payload, "%d.%d", &userID, &expiryUnix); err != nil {
		return 0, false
	}
	if time.Now().Unix() > expiryUnix || userID <= 0 {
		return 0, false
	}
	return userID, true
}

// issueCookie sets the session cookie for an authenticated admin.
func (s *Server) issueCookie(w http.ResponseWriter, userID int64) {
	ttl := s.cfg.SessionTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    s.signToken(userID, time.Now().Add(ttl)),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

// clearCookie removes the session cookie.
func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
}

// currentUser resolves the logged-in admin from the request cookie.
func (s *Server) currentUser(r *http.Request) (int64, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return 0, false
	}
	return s.verifyToken(c.Value)
}

// ---- login rate limiting ---------------------------------------------------

func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// maxLoginFailureIPs is the hard cap on tracked source addresses (see
// recordLoginFailure). loginGCEvery bounds the reclaim sweeps: both exist so
// an unauthenticated rotation of source IPs cannot grow the map without
// limit, and so the sweep is never charged to a single login attempt.
const (
	maxLoginFailureIPs = 4096
	loginGCEvery       = 5 * time.Minute
)

// loginAllowed reports whether this IP may still attempt a login.
func (s *Server) loginAllowed(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	recent := s.failures[ip][:0]
	for _, t := range s.failures[ip] {
		if now.Sub(t) < loginFailureWindow {
			recent = append(recent, t)
		}
	}
	s.failures[ip] = recent
	return len(recent) < maxLoginFailures
}

func (s *Server) recordLoginFailure(ip string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[ip] = append(s.failures[ip], now)
	// A hard cap against address rotation: an unauthenticated client can
	// manufacture distinct source IPs, and the map is the only state that
	// grows with them. The eviction is amortized (see loginGCEvery), so a
	// login storm cannot make the hot path O(n^2); the sweep that follows
	// brings the map back under the cap.
	if len(s.failures) > maxLoginFailureIPs && now.Sub(s.lastEvict) >= loginGCEvery {
		s.evictOldestFailuresLocked(now)
	}
}

// gcLoginFailuresIfDue runs the reclaims at most once per loginGCEvery, which
// is what keeps the map bounded on an unattended process without paying O(n)
// on every login attempt.
func (s *Server) gcLoginFailuresIfDue(now time.Time) {
	s.mu.Lock()
	size := len(s.failures)
	if size == 0 || now.Sub(s.lastLoginGC) < loginGCEvery {
		s.mu.Unlock()
		return
	}
	s.lastLoginGC = now
	over := size > maxLoginFailureIPs
	s.mu.Unlock()

	s.gcLoginFailures(now)
	if over {
		s.mu.Lock()
		s.evictOldestFailuresLocked(now)
		s.mu.Unlock()
	}
}

// gcLoginFailures prunes IPs with no attempt inside the window so the map
// cannot grow forever with one-off source addresses. Every entry the guard
// keeps is still limiting; entries from IPs that walked away are reclaimed.
// This mirrors internal/bot's rateLimiter.gc.
func (s *Server) gcLoginFailures(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ip, ts := range s.failures {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > loginFailureWindow {
			delete(s.failures, ip)
		}
	}
}

// evictOldestFailuresLocked drops the oldest entries past the hard cap. An
// eviction can only relieve a limit that has already expired: it removes the
// entries whose most recent attempt is the oldest, and an IP still inside its
// window is never the oldest one removed. Caller holds s.mu.
func (s *Server) evictOldestFailuresLocked(now time.Time) {
	if len(s.failures) <= maxLoginFailureIPs {
		return
	}
	s.lastEvict = now
	type entry struct {
		ip string
		at time.Time
	}
	all := make([]entry, 0, len(s.failures))
	for ip, ts := range s.failures {
		if len(ts) > 0 {
			all = append(all, entry{ip: ip, at: ts[len(ts)-1]})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, e := range all[:len(all)-maxLoginFailureIPs] {
		delete(s.failures, e.ip)
	}
}

// Start listens until ctx is cancelled, then shuts down gracefully.
func (s *Server) Start(ctx context.Context) error {
	s.server = &http.Server{Addr: s.cfg.Addr, Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(sh)
	}()
	s.log.Infof("[PIPELINE:dashboard] web dashboard listening on http://%s", s.cfg.Addr)
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
