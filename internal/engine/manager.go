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
}

// MediaOptions is the media-side configuration.
type MediaOptions struct {
	DefaultVolume      int
	DefaultSpeed       float64
	Autoplay           bool
	MuteUntilConnected bool
	SeekStepSeconds    int
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
}

// Manager owns browsers and sessions.
type Manager struct {
	browserCfg BrowserConfig
	mediaCfg   MediaOptions
	meetCfg    MeetOptions
	log        Logger
	events     chan Event

	mu       sync.RWMutex
	sessions map[SessionID]*Session

	sharedMu sync.Mutex
	shared   *sharedBrowser
}

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
		browserCfg: browser,
		mediaCfg:   media,
		meetCfg:    meet,
		log:        log,
		events:     make(chan Event, 128),
		sessions:   map[SessionID]*Session{},
	}
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

	id := newSessionID()
	s := &Session{
		ID:        id,
		MeetURL:   meetURL,
		MediaURL:  media.URL,
		MediaKind: media.Kind,
		TabTitle:  tabTitle,
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
	m.sessions[id] = s
	m.mu.Unlock()

	go s.run()
	return s, nil
}

// acquireBrowser returns a browser for a session plus a release function.
func (m *Manager) acquireBrowser() (*rod.Browser, func(), error) {
	opts := launchOptions{browser: m.browserCfg, meet: m.meetCfg}

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
