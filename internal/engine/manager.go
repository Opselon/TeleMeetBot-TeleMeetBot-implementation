// Package engine drives Chromium to join Google Meet and screen-share a media
// link. Everything is raw Go (rod/CDP) — no Selenium, no Python, no external
// automation service.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
)

// BrowserConfig is the browser subset of the app configuration.
type BrowserConfig struct {
	Executable   string
	Headless     bool
	PerSession   bool
	Display      string
	WindowWidth  int
	WindowHeight int
	Lang         string
	ExtraArgs    []string
	UserAgent    string
	Proxy        string
	// GPU selects the compositing backend: "off" (software compositing,
	// default in headless — 6x less host CPU than SwiftShader for the same
	// frames), "swiftshader" (ANGLE/SwiftShader software GL) or "auto" (leave
	// the choice to Chromium, for real-GPU hosts).
	GPU string
}

// MediaOptions is the media-side configuration.
type MediaOptions struct {
	DefaultVolume      int
	DefaultSpeed       float64
	Autoplay           bool
	MuteUntilConnected bool
	SeekStepSeconds    int
	DefaultAudioLang   string
}

// MeetOptions is the Google Meet configuration.
type MeetOptions struct {
	GuestName             string
	PresentMode           string // "tab" | "screen"
	CaptureSource         string
	JoinTimeoutSeconds    int
	PresentTimeoutSeconds int
	AdmitWaitSeconds      int
}

// Logger is the minimal logging surface the engine needs.
type Logger interface {
	Debugf(format string, args ...interface{})
	Infof(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

type noopLogger struct{}

func (noopLogger) Debugf(string, ...interface{}) {}
func (noopLogger) Infof(string, ...interface{})  {}
func (noopLogger) Warnf(string, ...interface{})  {}
func (noopLogger) Errorf(string, ...interface{}) {}

// SessionID identifies a streaming session.
type SessionID string

// State values shown by the dashboard.
const (
	StateStarting  = "starting"
	StateJoining   = "joining"
	StateAdmitting = "admitting"
	StateSharing   = "sharing"
	StateStreaming = "streaming"
	StatePaused    = "paused"
	StateEnded     = "ended"
	StateError     = "error"
)

// EventKind classifies lifecycle events pushed to the bot.
type EventKind string

const (
	// EventState carries a progress update (Text = human message).
	EventState EventKind = "state"
	// EventJoined fires once the meeting has admitted the bot.
	EventJoined EventKind = "joined"
	// EventSharing fires once screen sharing has started.
	EventSharing EventKind = "sharing"
	// EventStreaming fires once the media is actually playing to the meeting.
	EventStreaming EventKind = "streaming"
	// EventError fires on terminal failure (Text = reason).
	EventError EventKind = "error"
	// EventEnded fires when the session stopped for any reason.
	EventEnded EventKind = "ended"
)

// Event is a session lifecycle notification.
type Event struct {
	Session SessionID
	Kind    EventKind
	Text    string
	At      time.Time
}

// StartRequest is what the bot submits for a new streaming session.
type StartRequest struct {
	MeetLink  string
	MediaLink string
	TabTitle  string
	AudioLang string
}

// Manager owns browsers and sessions.
type Manager struct {
	browserCfg BrowserConfig
	mediaCfg   MediaOptions
	meetCfg    MeetOptions
	log        Logger
	events     chan Event

	maxSessions int

	mu       sync.RWMutex
	sessions map[SessionID]*Session

	sharedMu sync.Mutex
	shared   *sharedBrowser
}

// maxFinishedSessions bounds how many ended sessions stay in the map. They are
// kept so the dashboard/history can still render them, but the map must not
// grow for the lifetime of the process.
const maxFinishedSessions = 10

type sharedBrowser struct {
	browser *rod.Browser
	close   func()
	refs    int
}

// NewManager builds an engine manager. The returned manager does not launch a
// browser until the first session starts.
func NewManager(browser BrowserConfig, media MediaOptions, meet MeetOptions, log Logger) *Manager {
	if log == nil {
		log = noopLogger{}
	}
	return &Manager{
		browserCfg:  browser,
		mediaCfg:    media,
		meetCfg:     meet,
		log:         log,
		events:      make(chan Event, 128),
		sessions:    map[SessionID]*Session{},
		maxSessions: 1,
	}
}

// SetMaxSessions caps concurrent sessions. The engine enforces the limit as a
// hard fence so a session cannot be started past it even if a caller forgets to
// check (the dashboard also checks; this is the backstop). A value <= 0 means
// "no engine-side cap" (the config validator forbids that in practice).
func (m *Manager) SetMaxSessions(n int) {
	m.mu.Lock()
	m.maxSessions = n
	m.mu.Unlock()
}

// MaxSessions reports the configured cap.
func (m *Manager) MaxSessions() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.maxSessions
}

// Events exposes lifecycle events (single consumer: the bot).
func (m *Manager) Events() <-chan Event { return m.events }

func (m *Manager) emit(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	select {
	case m.events <- ev:
	default:
		// Dashboard backpressure must never block a meeting.
	}
}

// Get returns a session by id.
func (m *Manager) Get(id SessionID) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// List returns sessions, newest last.
func (m *Manager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// Active returns sessions that are still running.
func (m *Manager) Active() []*Session {
	var out []*Session
	for _, s := range m.List() {
		if s.snapshotState() != StateEnded && s.snapshotState() != StateError {
			out = append(out, s)
		}
	}
	return out
}

// Stop cancels a session and waits (bounded) for its cleanup.
func (m *Manager) Stop(id SessionID) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("session %s not found", id)
	}
	s.Stop()
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
		return fmt.Errorf("session %s did not stop in time", id)
	}
	return nil
}

// StopAll stops every session (used on SIGTERM).
func (m *Manager) StopAll() {
	for _, s := range m.List() {
		s.Stop()
	}
	for _, s := range m.List() {
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
	}
}

// Start validates the request and launches a session in the background.
func (m *Manager) Start(ctx context.Context, req StartRequest) (*Session, error) {
	meetURL, err := NormalizeMeetURL(req.MeetLink)
	if err != nil {
		return nil, err
	}
	media, err := ClassifyMedia(req.MediaLink)
	if err != nil {
		return nil, err
	}
	tabTitle := strings.TrimSpace(req.TabTitle)
	if tabTitle == "" {
		tabTitle = m.meetCfg.CaptureSource
	}
	if tabTitle == "" {
		tabTitle = "TeleMeet Media"
	}
	if m.meetCfg.PresentMode == "screen" {
		tabTitle = m.meetCfg.CaptureSource
	}

	audioLang := NormalizeAudioLang(req.AudioLang)
	if strings.TrimSpace(req.AudioLang) == "" {
		if m.mediaCfg.DefaultAudioLang != "" {
			audioLang = NormalizeAudioLang(m.mediaCfg.DefaultAudioLang)
		} else {
			audioLang = "fa"
		}
	}

	id := newSessionID()
	s := &Session{
		ID:        id,
		MeetURL:   meetURL,
		MediaURL:  media.URL,
		MediaKind: media.Kind,
		TabTitle:  tabTitle,
		AudioLang: audioLang,
		state:     StateStarting,
		detail:    "allocated",
		created:   time.Now(),
		mgr:       m,
		log:       m.log,
		playerOpts: PlayerOptions{
			TabTitle: tabTitle,
			Autoplay: false, // starts after sharing is confirmed
			Mute:     m.mediaCfg.MuteUntilConnected,
			Loop:     true,
			Volume:   m.mediaCfg.DefaultVolume,
			Speed:    m.mediaCfg.DefaultSpeed,
		},
	}

	s.ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})

	m.mu.Lock()
	// Enforce the concurrent-session cap and keep the finished-session map
	// bounded, atomically with the insert: two simultaneous Start calls must
	// not both pass a "check then insert" fence.
	active := 0
	for _, other := range m.sessions {
		if st := other.snapshotState(); st != StateEnded && st != StateError {
			active++
		}
	}
	if m.maxSessions > 0 && active >= m.maxSessions {
		m.mu.Unlock()
		return nil, fmt.Errorf("session limit reached: %d of %d already running (limits.max_sessions)", active, m.maxSessions)
	}
	m.pruneFinishedLocked()
	m.sessions[id] = s
	m.mu.Unlock()

	go s.run()
	return s, nil
}

// pruneFinishedLocked drops the oldest finished sessions once more than
// maxFinishedSessions are retained. Caller holds m.mu.
func (m *Manager) pruneFinishedLocked() {
	if len(m.sessions) <= maxFinishedSessions {
		return
	}
	type finished struct {
		id      SessionID
		created time.Time
	}
	var done []finished
	for id, s := range m.sessions {
		st := s.snapshotState()
		if st == StateEnded || st == StateError {
			done = append(done, finished{id: id, created: s.created})
		}
	}
	if len(done) <= maxFinishedSessions {
		return
	}
	sort.Slice(done, func(i, j int) bool { return done[i].created.Before(done[j].created) })
	for _, f := range done[:len(done)-maxFinishedSessions] {
		delete(m.sessions, f.id)
	}
}

// acquireBrowser returns a browser for a session plus a release function.
func (m *Manager) acquireBrowser() (*rod.Browser, func(), error) {
	opts := launchOptions{browser: m.browserCfg, meet: m.meetCfg, forMeet: true}

	if !m.browserCfg.PerSession {
		m.sharedMu.Lock()
		defer m.sharedMu.Unlock()
		if m.shared == nil {
			b, closeFn, err := launchBrowser(opts, m.log)
			if err != nil {
				return nil, nil, err
			}
			m.shared = &sharedBrowser{browser: b, close: closeFn}
		}
		m.shared.refs++
		refs := m.shared.refs
		var once sync.Once
		return m.shared.browser, func() {
			once.Do(func() {
				m.sharedMu.Lock()
				defer m.sharedMu.Unlock()
				refs--
				if refs <= 0 && m.shared != nil {
					m.shared.close()
					m.shared = nil
				}
			})
		}, nil
	}

	dir, err := tempProfileDir()
	if err != nil {
		return nil, nil, err
	}
	opts.userData = dir
	b, closeFn, err := launchBrowser(opts, m.log)
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	return b, func() { once.Do(closeFn) }, nil
}

// newSessionID returns a short random id like "s-1a2b3c4d".
func newSessionID() SessionID {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return SessionID(fmt.Sprintf("s-%d", time.Now().UnixNano()%1e8))
	}
	return SessionID("s-" + hex.EncodeToString(buf))
}

// meetCodeRe matches the modern Meet code shape (3-4-3).
var meetCodeRe = regexp.MustCompile(`[a-z0-9]{3}-[a-z0-9]{4}-[a-z0-9]{3}`)

// Shortcode extracts a Meet code from a URL, or "" when there is none.
func Shortcode(meetURL string) string {
	return meetCodeRe.FindString(strings.ToLower(meetURL))
}

// NormalizeMeetURL accepts a bare code, a "meet.google.com/xxx" string or a full
// URL and returns a canonical https URL with hl=en-US.
func NormalizeMeetURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("meet link is empty")
	}
	if !strings.Contains(s, "://") {
		if strings.HasPrefix(strings.ToLower(s), "meet.google.com") {
			s = "https://" + s
		} else {
			s = "https://meet.google.com/" + strings.TrimPrefix(s, "/")
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid meet link: %w", err)
	}
	host := strings.ToLower(u.Host)
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	if host != "meet.google.com" {
		return "", fmt.Errorf("not a Google Meet link (expected meet.google.com): %s", raw)
	}
	out := "https://meet.google.com" + u.Path
	if code := Shortcode(u.Path); code != "" {
		out = "https://meet.google.com/" + code
	}
	if out == "https://meet.google.com/" || strings.TrimSpace(u.Path) == "" {
		return "", fmt.Errorf("meet link has no meeting code: %s", raw)
	}
	q := ""
	if hl := u.Query().Get("hl"); hl != "" {
		q = "?hl=" + hl
	} else {
		q = "?hl=en-US"
	}
	if au := u.Query().Get("authuser"); au != "" {
		if q == "" {
			q = "?"
		} else {
			q += "&"
		}
		q += "authuser=" + au
	}
	return out + q, nil
}

// tempProfileDir creates a per-session browser profile directory.
func tempProfileDir() (string, error) {
	base := strings.TrimSpace(os.Getenv("TELEMEETBOT_PROFILE_DIR"))
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(filepath.Join(base, "telemeetbot"), 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(filepath.Join(base, "telemeetbot"), "profile-*")
}
