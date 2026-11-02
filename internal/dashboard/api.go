package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/logx"
)

// Router and handlers.
//
// Route map (everything under /api requires the session cookie):
//
//	GET  /                     login page (or the app when logged in)
//	POST /api/auth/telegram    verify the Login Widget payload, set cookie
//	POST /api/auth/logout      clear the cookie
//	GET  /api/state            one snapshot: health + sessions + users + logs stats
//	GET  /api/logs             recent entries (?level=&component=&n=)
//	GET  /api/logs/stream      SSE tail
//	POST /api/session/start    {meet_url, media_url, mode, queue[], volume, speed}
//	POST /api/session/{id}/stop
//	POST /api/session/{id}/action {action, arg}

// securityHeaders applies the same hardening the health pages use.
func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' https://telegram.org 'unsafe-inline'; style-src 'unsafe-inline'; img-src https: data:; connect-src 'self'")
}

// requireCSRF is the second half of the dashboard's request forgery
// defense, applied to every state-changing POST. The first half is the
// SameSite=Strict session cookie; this one is defense in depth for the
// day that is not enough (a relaxed SameSite, a legacy browser that
// ignores it, or a same-site injection).
//
// Two independent checks, both of which a browser-issued cross-site
// request cannot pass:
//
//  1. Origin/Referer must equal the request's own Host when either is
//     present. A cross-site form or fetch always carries a foreign Origin.
//  2. Content-Type must be application/json. The dashboard's own client
//     sends nothing else on these routes; a plain HTML form cannot set a
//     Content-Type header at all and defaults to
//     application/x-www-form-urlencoded or text/plain, so the native form
//     attack dies here.
//
// Both are deliberately stateless: no server-side token store, no second
// cookie, nothing to expire or synchronize. A SameSite=Strict cookie plus
// an origin check plus a JSON content-type gate is the standard layered
// minimum, and it costs the page nothing — api() already sends JSON.
//
// requireJSON is a separate option for routes that carry a JSON body, so
// the login route (which legitimately receives a form-encoded widget
// payload) can take the origin check alone.
func (s *Server) requireCSRF(requireJSON bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := checkCSRF(r, requireJSON); err != nil {
			s.log.Warnf("[PIPELINE:dashboard] rejected %s %s: %v", r.Method, r.URL.Path, err)
			s.writeErr(w, http.StatusForbidden, "%v", err)
			return
		}
		next(w, r)
	}
}

// errCSRF names a refusal reason without describing what a legitimate
// request looked like.
type errCSRF string

func (e errCSRF) Error() string { return string(e) }

const (
	csrfOriginMismatch  = errCSRF("origin does not match this host")
	csrfRefererMismatch = errCSRF("referer does not match this host")
	csrfContentType     = errCSRF("content-type must be application/json")
	csrfMethod          = errCSRF("method not allowed")
)

// checkCSRF reports why a state-changing request must be refused. An
// absent Origin/Referer (a non-browser client or an older one) is not
// fatal on its own — but such a client is not running the attacker's
// form, and the JSON gate still applies to the body-carrying routes.
func checkCSRF(r *http.Request, requireJSON bool) error {
	if r.Method != http.MethodPost {
		return csrfMethod
	}
	// 1. Origin when present; some older browsers send only Referer.
	if origin := r.Header.Get("Origin"); origin != "" {
		if !sameOrigin(origin, r) {
			return csrfOriginMismatch
		}
	} else if ref := r.Header.Get("Referer"); ref != "" {
		if !sameOrigin(ref, r) {
			return csrfRefererMismatch
		}
	}
	// 2. The JSON gate: a plain HTML form cannot set a Content-Type
	// header, so it lands here.
	if requireJSON {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
			return csrfContentType
		}
	}
	return nil
}

// sameOrigin reports whether the given Origin/Referer header value names
// the same host the request was actually addressed to. An unparseable or
// scheme-less value is treated as foreign rather than letting a malformed
// header bypass the check.
func sameOrigin(headerValue string, r *http.Request) bool {
	u, err := url.Parse(strings.TrimSpace(headerValue))
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// guardAPI wraps an authenticated POST route with the CSRF checks (origin
// and JSON content-type), so no route can be wired with authentication
// but without forgery protection.
func (s *Server) guardAPI(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(s.requireCSRF(true, next))
}

// guardLogin wraps the login POST with the origin check only: the
// Telegram widget delivers its signed payload as form data, so a JSON
// content-type would reject the only legitimate login flow.
func (s *Server) guardLogin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireCSRF(false, next)
}

// handler builds the router (separate for tests).
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.root)
	mux.HandleFunc("/api/auth/telegram", s.guardLogin(s.handleTelegramAuth))
	mux.HandleFunc("/api/auth/logout", s.guardAPI(s.handleLogout))
	mux.HandleFunc("/api/state", s.requireAuth(s.handleState))
	mux.HandleFunc("/api/logs", s.requireAuth(s.handleLogs))
	mux.HandleFunc("/api/logs/stream", s.requireAuth(s.handleLogStream))
	mux.HandleFunc("/api/session/start", s.guardAPI(s.handleSessionStart))
	mux.HandleFunc("/api/session/stop", s.guardAPI(s.handleSessionStop))     // ?id=
	mux.HandleFunc("/api/session/action", s.guardAPI(s.handleSessionAction)) // ?id= + body
	return s.withSecurity(mux)
}

// withSecurity wraps every response with the hardening headers.
func (s *Server) withSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		next.ServeHTTP(w, r)
	})
}

// requireAuth guards a handler with the session cookie AND the admin
// allow-list (a valid cookie for a since-removed admin is rejected too).
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := s.currentUser(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.cfg.IsAdmin(uid) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// writeJSON is the single JSON responder.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	_ = enc.Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, status int, format string, args ...interface{}) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// root serves the embedded single-page app; login state decides which view
// the page renders (the page itself is static; /api/state returns 401 until
// logged in, which the page treats as "show login").
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML(s.cfg.BotUsername, s.cfg.PublicBaseURL)))
}

// handleTelegramAuth verifies the Login Widget payload and logs the admin in.
func (s *Server) handleTelegramAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := s.clientIP(r)
	if !s.loginAllowed(ip) {
		s.log.Warnf("[PIPELINE:dashboard] login rate-limited for %s", ip)
		s.writeErr(w, http.StatusTooManyRequests, "too many failed logins; try again in a few minutes")
		return
	}
	// Accept both a JSON body of the widget fields and query parameters
	// (the widget's redirect mode delivers them as query params).
	fields := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			fields[k] = v[0]
		}
	}
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
		for k, v := range body {
			if _, ok := fields[k]; !ok {
				fields[k] = v
			}
		}
	}

	auth, err := VerifyTelegramAuth(s.cfg.BotToken, fields, time.Now())
	if err != nil {
		s.recordLoginFailure(ip)
		s.log.Warnf("[PIPELINE:dashboard] login rejected from %s: %v", ip, err)
		s.writeErr(w, http.StatusUnauthorized, "login failed: %v", err)
		return
	}
	if !s.cfg.IsAdmin(auth.ID) {
		s.recordLoginFailure(ip)
		s.log.Warnf("[PIPELINE:dashboard] login denied: telegram id %d is not an admin", auth.ID)
		s.writeErr(w, http.StatusForbidden, "this Telegram account is not an administrator of the bot")
		return
	}
	s.issueCookie(w, auth.ID)
	s.log.Infof("[PIPELINE:dashboard] admin %d (%s) logged in from %s", auth.ID, auth.DisplayName(), ip)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":   true,
		"user": map[string]interface{}{"id": auth.ID, "name": auth.DisplayName(), "username": auth.Username},
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.clearCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// sessionView is one session as the dashboard renders it.
type sessionView struct {
	ID       string                  `json:"id"`
	State    string                  `json:"state"`
	Detail   string                  `json:"detail"`
	Error    string                  `json:"error,omitempty"`
	Title    string                  `json:"title,omitempty"`
	MeetURL  string                  `json:"meet_url,omitempty"`
	MediaURL string                  `json:"media_url,omitempty"`
	Paused   bool                    `json:"paused"`
	Muted    bool                    `json:"muted"`
	Volume   int                     `json:"volume"`
	Rate     float64                 `json:"rate"`
	Cur      float64                 `json:"cur"`
	Dur      float64                 `json:"dur"`
	Created  time.Time               `json:"created"`
	Joined   time.Time               `json:"joined,omitempty"`
	PlayMode string                  `json:"play_mode,omitempty"`
	QueueLen int                     `json:"queue_len"`
	QueueIdx int                     `json:"queue_idx"`
	HasNext  bool                    `json:"has_next"`
	HasPrev  bool                    `json:"has_prev"`
	Stages   []engine.StageTiming    `json:"stages,omitempty"`
	Download engine.DownloadProgress `json:"download,omitempty"`
}

func viewOf(s *engine.Session) sessionView {
	if s == nil {
		return sessionView{}
	}
	st := s.Snapshot()
	return sessionView{
		ID: string(st.ID), State: st.State, Detail: st.Detail, Error: st.Error,
		Title: st.Title, MeetURL: st.MeetURL, MediaURL: st.MediaURL,
		Paused: st.Paused, Muted: st.Muted, Volume: st.Volume, Rate: st.Rate,
		Cur: st.Cur, Dur: st.Dur, Created: st.Created, Joined: st.Joined,
		PlayMode: st.PlayMode, QueueLen: st.QueueLength, QueueIdx: st.QueueIndex,
		HasNext: st.HasNext, HasPrev: st.HasPrevious,
		Stages: st.Stages, Download: st.DownloadProgress,
	}
}

// handleState is the dashboard's one polling endpoint.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	out := map[string]interface{}{
		"now": time.Now(),
	}
	if s.deps.Health != nil {
		out["health"] = s.deps.Health()
	}
	if s.deps.Engine != nil {
		sessions := s.deps.Engine.List()
		views := make([]sessionView, 0, len(sessions))
		active := 0
		for _, sess := range sessions {
			views = append(views, viewOf(sess))
			if !engine.IsTerminalState(sess.Snapshot().State) {
				active++
			}
		}
		out["sessions"] = views
		out["active_sessions"] = active
	}
	if s.deps.Users != nil {
		out["users"] = s.deps.Users.List()
	}
	if s.deps.Logs != nil {
		out["log_stats"] = s.deps.Logs.Stats()
	}
	if s.deps.Media != nil {
		out["media"] = s.deps.Media.Snapshot()
	}
	writeJSON(w, http.StatusOK, out)
}

// handleLogs returns recent log entries with optional filters.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.deps.Logs == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"entries": []interface{}{}})
		return
	}
	q := r.URL.Query()
	n := atoiDefault(q.Get("n"), 200)
	if n <= 0 || n > 2000 {
		n = 200
	}
	minLevel := atoiDefault(q.Get("level"), 0)
	component := strings.TrimSpace(q.Get("component"))
	entries := s.deps.Logs.RecentFiltered(n, logx.Level(minLevel))
	if component != "" {
		filtered := entries[:0:0]
		for _, e := range entries {
			if strings.EqualFold(e.ComponentName(), component) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"entries": entries})
}

// handleLogStream is the SSE tail of the central log.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if s.deps.Logs == nil {
		http.Error(w, "log capture unavailable", http.StatusServiceUnavailable)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusHTTPVersionNotSupported)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Bootstrap with the entries since the page requested the stream.
	for _, e := range s.deps.Logs.Since(time.Now().Add(-30 * time.Second)) {
		writeSSE(w, e)
	}
	fl.Flush()

	ch, cancel := s.deps.Logs.Subscribe(128)
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if !writeSSE(w, e) {
				return
			}
			fl.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, e interface{}) bool {
	b, err := json.Marshal(e)
	if err != nil {
		return true
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err == nil
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}
