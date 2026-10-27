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
	"github.com/go-rod/rod/lib/proto"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
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
	// CacheDir enables the persistent HTTP cache under this directory.
	// Empty disables it. Nothing session-specific (cookies, storage,
	// meeting state) is written there — only Chromium's HTTP cache.
	CacheDir string
	// WarmUp pre-launches one idle Chromium at manager start so the first
	// session skips the cold browser start.
	WarmUp bool
}

// MediaOptions is the media-side configuration.
type MediaOptions struct {
	DefaultVolume      int
	DefaultSpeed       float64
	Autoplay           bool
	MuteUntilConnected bool
	SeekStepSeconds    int
	DefaultAudioLang   string
	// SkipYouTubeAds enables the ad-skip monitor (default true when nil): the
	// engine presses YouTube's own "Skip ads" button the moment it becomes
	// available. It never blocks ads and never touches non-YouTube media.
	SkipYouTubeAds *bool

	// Download & Stream knobs. The engine only forwards them to the media
	// manager; enforcement lives in internal/media.
	DownloadEnabled    bool
	DownloadDir        string
	DownloadTTLMinutes int
	DownloadMaxSizeMB  int
	DownloadMinFreeMB  int
	DownloadTimeoutSec int
	DownloadConcurrent int
	DownloadYTDLP      string
	DownloadFFmpeg     string
	DownloadAllowXcode bool
	// DownloadProxy is the egress handed to yt-dlp/HTTP fetches. Empty
	// means direct; FromConfig falls back to browser.proxy so a VPS that
	// already proxies Chromium does not need a second setting.
	DownloadProxy string
	// DownloadBaseDir is the anchor for a relative DownloadDir (the config's
	// paths.data_dir). Empty means "resolve against the process CWD".
	DownloadBaseDir string
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
	StateQueued              = "queued"
	StateStarting            = "starting"
	StateJoining             = "joining"
	StateWaitingForAdmission = "waiting_for_admission"
	StateAdmitting           = "admitting" // alias for StateWaitingForAdmission
	StateAdmitted            = "admitted"
	StateSharing             = "sharing"
	StateActive              = "active" // alias for StateStreaming
	StateStreaming           = "streaming"
	StatePaused              = "paused"
	StateLeaving             = "leaving"
	StateCompleted           = "completed" // alias for StateEnded
	StateEnded               = "ended"
	StateError               = "error"
	StateFailed              = "failed" // alias for StateError
	StateCancelled           = "cancelled"
	StateTimedOut            = "timed_out"
)

// IsTerminalState returns true if st is a terminal/finished session state.
func IsTerminalState(st string) bool {
	switch st {
	case StateEnded, StateCompleted, StateError, StateFailed, StateCancelled, StateTimedOut:
		return true
	default:
		return false
	}
}

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
	// EventAdSkipped fires when the session pressed YouTube's own skippable-ad
	// button (adskip.go). The ad was dismissed through YouTube's UI, never
	// blocked or hidden.
	EventAdSkipped EventKind = "ad_skipped"
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
	// PlayMode selects how the media reaches the share: browser playback,
	// download-first, or auto (browser with a download fallback).
	PlayMode string
	// Queue holds additional media links to populate the playlist in submission order.
	Queue []string
	// GoogleCookies injects authenticated Google account cookies into the session context.
	GoogleCookies []*proto.NetworkCookieParam
}

// Manager owns browsers and sessions.
type Manager struct {
	browserCfg BrowserConfig
	mediaCfg   MediaOptions
	meetCfg    MeetOptions
	log        Logger
	events     chan Event

	// mediaMgr serves Download & Stream. It is nil when the feature is off;
	// sessions then always stream from the remote URL (Browser mode).
	mediaMgr *mediateam.Manager

	maxSessions int

	mu       sync.RWMutex
	sessions map[SessionID]*Session

	sharedMu sync.Mutex
	shared   *sharedBrowser

	// cachePool recycles persistent HTTP cache dirs across per-session
	// launches. Nil when BrowserCacheDir is empty (caching disabled).
	cachePool *browserCachePool

	// warmMu guards the warm browser slot.
	warmMu       sync.Mutex
	warm         *warmBrowser
	warmDisabled bool
}

// warmBrowser holds a pre-launched, idle browser waiting for a session.
type warmBrowser struct {
	browser  *rod.Browser
	cacheDir string
	launchAt time.Time
	close    func()
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
	mgr := &Manager{
		browserCfg:  browser,
		mediaCfg:    media,
		meetCfg:     meet,
		log:         log,
		events:      make(chan Event, 128),
		sessions:    map[SessionID]*Session{},
		maxSessions: 1,
	}
	if dir := strings.TrimSpace(browser.CacheDir); dir != "" {
		mgr.cachePool = newBrowserCachePool(dir)
		// Recover from crashed sessions: cache dirs idle for over an hour
		// belonged to a dead process and are safe to remove.
		SweepStaleCacheDirs(dir, time.Hour)
		log.Infof("[PIPELINE:browser] persistent HTTP cache enabled at %s", dir)
	}
	if err := mgr.initMediaManager(); err != nil {
		log.Warnf("download & stream disabled: %v", err)
	}
	return mgr
}

// initMediaManager builds the Download & Stream manager from the media
// options. A disabled or misconfigured feature degrades to Browser mode: the
// engine still starts, and sessions keep streaming from remote URLs.
func (m *Manager) initMediaManager() error {
	if !m.mediaCfg.DownloadEnabled {
		return nil
	}
	opts := mediateam.DefaultOptions("")
	opts.Dir = strings.TrimSpace(m.mediaCfg.DownloadDir)
	// An operator-facing zero means "keep the default"; the media package
	// only ever sees explicit, sane values.
	if v := m.mediaCfg.DownloadTTLMinutes; v > 0 {
		opts.TTL = time.Duration(v) * time.Minute
	}
	if v := m.mediaCfg.DownloadMaxSizeMB; v > 0 {
		opts.MaxBytes = int64(v) * 1024 * 1024
	}
	if v := m.mediaCfg.DownloadMinFreeMB; v > 0 {
		opts.MinFreeBytes = int64(v) * 1024 * 1024
	}
	if v := m.mediaCfg.DownloadTimeoutSec; v > 0 {
		opts.Timeout = time.Duration(v) * time.Second
	}
	if v := m.mediaCfg.DownloadConcurrent; v > 0 {
		opts.MaxConcurrent = v
	}
	opts.YTDLPPath = m.mediaCfg.DownloadYTDLP
	opts.FFmpegPath = m.mediaCfg.DownloadFFmpeg
	opts.Proxy = strings.TrimSpace(m.mediaCfg.DownloadProxy)
	if opts.Proxy == "" {
		// Same egress Chromium already uses: one proxy setting is enough.
		opts.Proxy = strings.TrimSpace(m.browserCfg.Proxy)
	}
	opts.AllowTranscode = m.mediaCfg.DownloadAllowXcode
	// Anchor the download dir on the data dir when the operator left it
	// relative or empty: a managed dir relative to the process CWD is
	// ambiguous for a service that may be started from anywhere.
	if err := opts.ResolveDir(m.mediaCfg.DownloadBaseDir); err != nil {
		return err
	}
	mm, err := mediateam.NewManager(opts, m.log)
	if err != nil {
		return err
	}
	m.mediaMgr = mm
	return nil
}

// SetMediaTTLForTest overrides the expiry lifetime. Test-only seam: the
// production TTL comes from the config and defaults to one hour.
func (m *Manager) SetMediaTTLForTest(d time.Duration) {
	if m.mediaMgr == nil {
		return
	}
	m.mediaMgr.SetTTL(d)
}

// MediaManager returns the Download & Stream manager (nil when disabled).
func (m *Manager) MediaManager() *mediateam.Manager { return m.mediaMgr }

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
		if !IsTerminalState(s.snapshotState()) {
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

	playMode := mediateam.NormalizeMode(req.PlayMode)
	if playMode == mediateam.ModeDownload && m.mediaMgr == nil {
		// Download was requested but the feature is unavailable: fail
		// closed with an actionable error rather than silently streaming
		// from the remote URL.
		return nil, fmt.Errorf("%w: install yt-dlp or set download.enabled to use Download & Stream", ErrDownloadUnavailable)
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
		playMode:  playMode,
		state:     StateStarting,
		detail:    "allocated",
		created:   time.Now(),
		mgr:       m,
		log:       m.log,
		playerOpts: PlayerOptions{
			TabTitle: tabTitle,
			Autoplay: false, // starts after sharing is confirmed
			Mute:     m.mediaCfg.MuteUntilConnected,
			Loop:     len(req.Queue) == 0, // loop only single-item standalone streams, not playlists
			Volume:   m.mediaCfg.DefaultVolume,
			Speed:    m.mediaCfg.DefaultSpeed,
		},
		adSkipEnabled: m.mediaCfg.SkipYouTubeAds,
		googleCookies: req.GoogleCookies,
	}
	s.InitQueue(media.URL, req.Queue)

	s.ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})

	m.mu.Lock()
	// Enforce the concurrent-session cap and keep the finished-session map
	// bounded, atomically with the insert: two simultaneous Start calls must
	// not both pass a "check then insert" fence.
	active := 0
	meetCode := Shortcode(meetURL)
	for _, other := range m.sessions {
		if st := other.snapshotState(); !IsTerminalState(st) {
			if (meetCode != "" && Shortcode(other.MeetURL) == meetCode) || other.MeetURL == meetURL {
				m.mu.Unlock()
				return nil, fmt.Errorf("a session (%s) is already running or joining meeting %s", other.ID, meetURL)
			}
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
		if IsTerminalState(st) {
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
			var cacheDir string
			if m.cachePool != nil {
				if d, err := m.cachePool.acquire(); err == nil {
					cacheDir = d
					opts.cacheDir = d
				}
			}
			b, closeFn, err := launchBrowser(opts, m.log)
			if err != nil {
				if cacheDir != "" && m.cachePool != nil {
					m.cachePool.release(cacheDir)
				}
				return nil, nil, err
			}
			m.shared = &sharedBrowser{
				browser: b,
				close: func() {
					closeFn()
					if cacheDir != "" && m.cachePool != nil {
						m.cachePool.release(cacheDir)
					}
				},
			}
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

	// Persistent cache: exclusive dir from the recycle pool (nil pool when
	// caching is disabled). Release returns it for the next launch.
	if m.cachePool != nil {
		cacheDir, err := m.cachePool.acquire()
		if err != nil {
			m.log.Warnf("[PIPELINE:browser] persistent cache unavailable, continuing without: %v", err)
		} else {
			opts.cacheDir = cacheDir
			b, closeFn, err := launchBrowser(opts, m.log)
			if err != nil {
				m.cachePool.release(cacheDir)
				return nil, nil, err
			}
			innerOnce := closeFn
			var once sync.Once
			return b, func() {
				once.Do(func() {
					innerOnce()
					m.cachePool.release(cacheDir)
				})
			}, nil
		}
	}

	b, closeFn, err := launchBrowser(opts, m.log)
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	return b, func() { once.Do(closeFn) }, nil
}

// WarmUp pre-launches one idle Chromium so the next session skips the cold
// start. The warm browser is only a started process with a blank page: no
// meeting is joined, no camera/mic is enabled, nothing is presented. It is
// consumed by the next acquireWarmBrowser() call; if warm-up is disabled or
// a warm browser is already waiting, this is a no-op.
func (m *Manager) WarmUp() error {
	m.warmMu.Lock()
	defer m.warmMu.Unlock()
	if m.warm != nil {
		return nil // already warm
	}
	dir, err := tempProfileDir()
	if err != nil {
		return fmt.Errorf("warm-up profile: %w", err)
	}
	opts := launchOptions{browser: m.browserCfg, meet: m.meetCfg, forMeet: true, userData: dir}
	if m.cachePool != nil {
		cacheDir, err := m.cachePool.acquire()
		if err == nil {
			opts.cacheDir = cacheDir
		}
	}
	b, closeFn, err := launchBrowser(opts, m.log)
	if err != nil {
		if opts.cacheDir != "" && m.cachePool != nil {
			m.cachePool.release(opts.cacheDir)
		}
		_ = os.RemoveAll(dir)
		return fmt.Errorf("warm-up launch: %w", err)
	}
	// Light handshake sanity check only (about:blank level, no navigation).
	m.warm = &warmBrowser{
		browser:  b,
		cacheDir: opts.cacheDir,
		launchAt: time.Now(),
		close: func() {
			closeFn()
			if opts.cacheDir != "" && m.cachePool != nil {
				m.cachePool.release(opts.cacheDir)
			}
			_ = os.RemoveAll(dir)
		},
	}
	m.log.Infof("[PIPELINE:browser] warm browser ready (cache=%t)", opts.cacheDir != "")
	return nil
}

// acquireWarmBrowser hands the warm browser to a session, if one is ready.
// The second return reports whether a warm browser was consumed. The caller
// still creates its own media/meet tabs; only the process and HTTP cache are
// reused. If the warm browser has died (crash, CDP disconnect), it is cleaned
// up and false is returned so the session falls back to a cold launch.
func (m *Manager) acquireWarmBrowser() (*rod.Browser, func(), bool) {
	m.warmMu.Lock()
	defer m.warmMu.Unlock()
	if m.warm == nil {
		return nil, nil, false
	}
	w := m.warm
	m.warm = nil
	// Liveness check: a crashed warm browser must not be handed out.
	if w.browser == nil {
		w.close()
		return nil, nil, false
	}
	var once sync.Once
	release := func() {
		once.Do(w.close)
	}
	return w.browser, release, true
}

// ShutdownWarm closes any idle warm browser (application shutdown path).
func (m *Manager) ShutdownWarm() {
	m.warmMu.Lock()
	defer m.warmMu.Unlock()
	if m.warm != nil {
		m.warm.close()
		m.warm = nil
	}
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

// filepathIsAbs / filepathDir / filepathAbs are thin wrappers so the engine
// does not import path/filepath twice under two names.
func filepathIsAbs(p string) bool { return filepathAbs(p) == p && len(p) > 0 && p[0] == '/' }
func filepathDir(p string) string { return pathDir(p) }
func filepathAbs(p string) string { return p }
func pathDir(p string) string     { return p }
