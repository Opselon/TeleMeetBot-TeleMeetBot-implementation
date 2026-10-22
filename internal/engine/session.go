package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// errHelpURL is a hint appended to "no media" failures.
const errHelpURL = "make sure the link is publicly reachable from this server"

// Session is one Google Meet participant that shares one media tab.
type Session struct {
	ID        SessionID
	MeetURL   string
	MediaURL  string
	MediaKind MediaKind
	TabTitle  string

	mgr        *Manager
	log        Logger
	playerOpts PlayerOptions

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.RWMutex
	state    string
	detail   string
	errMsg   string
	paused   bool
	muted    bool
	volume   int
	rate     float64
	title    string
	cur      float64
	dur      float64
	created  time.Time
	joined   time.Time
	ended    time.Time
	mediaErr string

	browser   *rod.Browser
	mediaPage *rod.Page
	meetPage  *rod.Page
}

// Status is a dashboard-friendly snapshot.
type Status struct {
	ID       SessionID
	State    string
	Detail   string
	Error    string
	Title    string
	MeetURL  string
	MediaURL string
	Paused   bool
	Muted    bool
	Volume   int
	Rate     float64
	Cur      float64
	Dur      float64
	Created  time.Time
	Joined   time.Time
	Ended    time.Time
}

// Elapsed is how long the session has been running.
func (st Status) Elapsed() time.Duration {
	if st.Created.IsZero() {
		return 0
	}
	end := time.Now()
	if !st.Ended.IsZero() {
		end = st.Ended
	}
	return end.Sub(st.Created)
}

// Snapshot returns a copy safe for concurrent readers.
func (s *Session) Snapshot() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Status{
		ID:       s.ID,
		State:    s.state,
		Detail:   s.detail,
		Error:    s.errMsg,
		Title:    s.title,
		MeetURL:  s.MeetURL,
		MediaURL: s.MediaURL,
		Paused:   s.paused,
		Muted:    s.muted,
		Volume:   s.volume,
		Rate:     s.rate,
		Cur:      s.cur,
		Dur:      s.dur,
		Created:  s.created,
		Joined:   s.joined,
		Ended:    s.ended,
	}
}

func (s *Session) snapshotState() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Stop asks the session to leave and release Chromium.
func (s *Session) Stop() { s.cancel() }

// Done is closed when the session has fully cleaned up.
func (s *Session) Done() <-chan struct{} { return s.done }

// setState records a new state and emits a state event.
func (s *Session) setState(state, detail string) {
	s.mu.Lock()
	changed := s.state != state || s.detail != detail
	s.state = state
	if detail != "" {
		s.detail = detail
	}
	s.mu.Unlock()
	if changed {
		s.log.Infof("session %s: %s (%s)", s.ID, state, detail)
		s.mgr.emit(Event{Session: s.ID, Kind: EventState, Text: detail})
	}
}

func (s *Session) setDetail(detail string) {
	s.mu.Lock()
	s.detail = detail
	s.mu.Unlock()
}

func (s *Session) setError(err error) {
	s.mu.Lock()
	s.errMsg = err.Error()
	s.state = StateError
	s.detail = err.Error()
	s.ended = time.Now()
	s.mu.Unlock()
	s.log.Errorf("session %s failed: %v", s.ID, err)
	s.mgr.emit(Event{Session: s.ID, Kind: EventError, Text: err.Error()})
}

func (s *Session) emit(kind EventKind, text string) {
	s.mgr.emit(Event{Session: s.ID, Kind: kind, Text: text})
}

func (s *Session) pages() (media, meet *rod.Page) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mediaPage, s.meetPage
}

// run is the session state machine.
func (s *Session) run() {
	defer close(s.done)
	defer s.cleanup()

	b, release, err := s.mgr.acquireBrowser()
	if err != nil {
		s.setError(fmt.Errorf("cannot launch chromium: %w", err))
		return
	}
	defer release()
	s.mu.Lock()
	s.browser = b
	s.mu.Unlock()

	s.setState(StateStarting, "chromium ready")

	if err := s.openMedia(); err != nil {
		if s.ctx.Err() == nil {
			s.setError(err)
		}
		return
	}
	if s.ctx.Err() != nil {
		return
	}

	if err := s.openMeet(); err != nil {
		if s.ctx.Err() == nil {
			s.setError(err)
		}
		return
	}
	if s.ctx.Err() != nil {
		return
	}

	if err := s.joinMeeting(); err != nil {
		if s.ctx.Err() == nil {
			s.setError(err)
		}
		return
	}
	s.mu.Lock()
	s.joined = time.Now()
	s.mu.Unlock()
	s.emit(EventJoined, "joined the meeting")

	s.setState(StateSharing, "starting screen share")
	if err := s.startPresenting(); err != nil {
		if s.ctx.Err() == nil {
			s.setError(err)
		}
		return
	}
	s.emit(EventSharing, "screen share is live")

	if err := s.playMedia(); err != nil {
		s.log.Warnf("session %s: media play failed after share: %v", s.ID, err)
		s.setDetail("sharing, media did not start: " + err.Error())
	}
	s.setState(StateStreaming, "streaming")
	s.emit(EventStreaming, "stream is live")

	s.watch()
}

// cleanup releases pages and finalises the state.
func (s *Session) cleanup() {
	media, meet := s.pages()
	if media != nil {
		_ = media.Close()
	}
	if meet != nil {
		_ = meet.Close()
	}
	s.mu.Lock()
	if s.state != StateError {
		s.state = StateEnded
		if s.detail == "" || s.detail == "streaming" {
			s.detail = "left the meeting"
		}
	}
	if s.ended.IsZero() {
		s.ended = time.Now()
	}
	s.mu.Unlock()
	s.emit(EventEnded, "session closed")
}

// watch keeps cached media stats fresh and detects a dead share/page.
func (s *Session) watch() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	presentTries := 0
	lastPresentCheck := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		st, err := s.queryMedia()
		if err != nil {
			s.setDetail("cannot read media status: " + err.Error())
		} else {
			s.mu.Lock()
			s.cur = st.CurrentTime
			s.dur = st.Duration
			s.paused = st.Paused
			s.muted = st.Muted
			s.volume = int(st.Volume*100 + 0.5)
			s.rate = st.PlaybackRate
			if st.Title != "" {
				s.title = st.Title
			}
			s.mediaErr = st.Error
			s.mu.Unlock()
			if st.Error != "" {
				s.setDetail("player error: " + st.Error)
			}
		}

		// Verify the presentation is still up; re-present a bounded number of
		// times (Meet sometimes drops a share on network hiccups).
		if time.Since(lastPresentCheck) > 15*time.Second {
			lastPresentCheck = time.Now()
			if ok, _ := s.presenting(); !ok {
				if presentTries >= 3 {
					s.setDetail("sharing stopped and could not be resumed; leaving")
					s.setError(errors.New("screen share was dropped by Google Meet"))
					return
				}
				presentTries++
				s.log.Warnf("session %s: share not detected, re-presenting (%d/3)", s.ID, presentTries)
				if err := s.startPresenting(); err != nil {
					s.log.Warnf("session %s: re-present failed: %v", s.ID, err)
				} else {
					_ = s.playMedia()
				}
			}
		}

		// A closed browser means there is nothing left to monitor.
		if _, meet := s.pages(); meet != nil {
			if _, err := meet.Eval("1"); err != nil {
				s.setDetail("browser disconnected")
				s.setError(fmt.Errorf("browser disconnected: %w", err))
				return
			}
		}
	}
}

// openMedia opens the media tab (player document or the raw link).
func (s *Session) openMedia() error {
	b := s.browser
	p, err := newPage(b, s.mgr.browserCfg)
	if err != nil {
		return fmt.Errorf("open media tab: %w", err)
	}
	s.mu.Lock()
	s.mediaPage = p
	s.mu.Unlock()

	target := s.MediaURL
	if s.MediaKind == MediaDirect {
		doc, err := PlayerHTML(MediaTarget{URL: s.MediaURL, Kind: s.MediaKind}, s.playerOpts)
		if err != nil {
			return fmt.Errorf("build player: %w", err)
		}
		// Loopback HTTP (not data:) so Chrome always allows the media fetch.
		target, err = players.playerURL(doc)
		if err != nil {
			return fmt.Errorf("serve player: %w", err)
		}
	}
	// The display surface headless Chrome captures is sized by the media tab's
	// viewport; set it before loading so the share matches browser.window_*.
	if err := p.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: widthOf(s.mgr.browserCfg), Height: heightOf(s.mgr.browserCfg), DeviceScaleFactor: 1,
	}); err != nil {
		s.log.Warnf("session %s: set viewport: %v", s.ID, err)
	}
	s.setState(StateJoining, "loading media")
	if err := p.Timeout(50 * time.Second).Navigate(target); err != nil {
		return fmt.Errorf("load media %s: %w", s.MediaURL, err)
	}
	_ = p.WaitLoad()

	// Consent/interstitial walls (YouTube in the EU, etc.).
	if err := s.clearMediaInterstitials(p); err != nil {
		return err
	}

	// Keep the tab title stable so Chromium's auto-select capture-source finds it.
	if _, err := p.Eval(titleLockJS(s.TabTitle)); err != nil {
		s.log.Debugf("session %s: title lock failed: %v", s.ID, err)
	}

	if err := s.waitMedia(35 * time.Second); err != nil {
		if prob := s.playerProblem(); prob != "" {
			return fmt.Errorf("media unavailable: %s", prob)
		}
		return fmt.Errorf("no playable media found on %s (%s)", s.MediaURL, errHelpURL)
	}
	if s.playerOpts.Mute {
		_, _ = s.mediaAction("mute", 0)
		_, _ = s.mediaAction("pause", 0)
	}
	return nil
}

// openMeet loads the Meet page.
func (s *Session) openMeet() error {
	p, err := newPage(s.browser, s.mgr.browserCfg)
	if err != nil {
		return fmt.Errorf("open meet tab: %w", err)
	}
	s.mu.Lock()
	s.meetPage = p
	s.mu.Unlock()
	s.setState(StateJoining, "loading meet")
	if err := p.Timeout(60 * time.Second).Navigate(s.MeetURL); err != nil {
		return fmt.Errorf("load meet link: %w", err)
	}
	_ = p.WaitLoad()
	// Meet is a SPA: give it a beat to render the prejoin screen.
	time.Sleep(1200 * time.Millisecond)
	return nil
}

// joinMeeting drives the prejoin screen and lobby.
func (s *Session) joinMeeting() error {
	timeout := time.Duration(s.mgr.meetCfg.JoinTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	var lastStage string
	for time.Now().Before(deadline) {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		stage, info := s.meetStage()
		if stage != lastStage {
			lastStage = stage
			s.log.Infof("session %s: meet stage=%s %s", s.ID, stage, info)
		}
		switch stage {
		case "meeting":
			return nil
		case "lobby":
			s.setState(StateAdmitting, "waiting for the host to admit the bot")
		case "blocked":
			return fmt.Errorf("cannot join this meeting: %s", info)
		case "login":
			return errors.New("this meeting requires a signed-in Google account; TeleMeetBot joins as a guest only")
		case "prejoin":
			s.setState(StateJoining, "accepting the prejoin screen")
			s.fillGuestName()
			if clicked, label := s.clickJoin(); clicked {
				s.log.Infof("session %s: clicked %q", s.ID, label)
				time.Sleep(1500 * time.Millisecond)
			}
		default:
			// Unknown/loading: a little patience.
		}
		time.Sleep(800 * time.Millisecond)
	}
	return fmt.Errorf("meet join timed out after %s (stage=%s)", timeout, lastStage)
}

// startPresenting clicks through Meet's "Present now" flow. Chromium's
// auto-select-desktop-capture-source answers the picker synchronously, so a
// successful click ends with an active share.
func (s *Session) startPresenting() error {
	timeout := time.Duration(s.mgr.meetCfg.PresentTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		if ok, _ := s.presenting(); ok {
			return nil
		}
		if _, p := s.pages(); p != nil { // meet tab
			opened, label := s.clickPresentButton()
			if opened {
				s.log.Infof("session %s: opened present menu (%s)", s.ID, label)
				time.Sleep(900 * time.Millisecond)
				if clicked, item := s.clickPresentSource(); clicked {
					s.log.Infof("session %s: clicked share source %q", s.ID, item)
					time.Sleep(1500 * time.Millisecond)
					if ok, _ := s.presenting(); ok {
						return nil
					}
				}
			}
		}
		time.Sleep(900 * time.Millisecond)
	}
	return errors.New("could not start screen sharing in Google Meet (present flow timed out); the bot is in the meeting — retry from the dashboard")
}

// playMedia starts playback after the share is live.
func (s *Session) playMedia() error {
	if _, err := s.mediaAction("unmute", 0); err != nil {
		return err
	}
	st, err := s.mediaAction("play", 0)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.paused = st.Paused
	s.muted = st.Muted
	s.volume = int(st.Volume*100 + 0.5)
	s.rate = st.PlaybackRate
	if st.Title != "" {
		s.title = st.Title
	}
	s.cur = st.CurrentTime
	s.dur = st.Duration
	s.mu.Unlock()
	return nil
}

// Pause pauses playback.
func (s *Session) Pause() error {
	st, err := s.mediaAction("pause", 0)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.paused = st.Paused
	s.mu.Unlock()
	if s.snapshotState() == StateStreaming {
		s.setState(StatePaused, "paused by operator")
	}
	return nil
}

// Resume continues playback.
func (s *Session) Resume() error {
	st, err := s.mediaAction("play", 0)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.paused = st.Paused
	s.mu.Unlock()
	if s.snapshotState() == StatePaused {
		s.setState(StateStreaming, "resumed by operator")
	}
	return nil
}

// SetMuted toggles the media tab audio.
func (s *Session) SetMuted(mute bool) error {
	action := "unmute"
	if mute {
		action = "mute"
	}
	st, err := s.mediaAction(action, 0)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.muted = st.Muted
	s.mu.Unlock()
	return nil
}

// SetVolume sets volume 0..100.
func (s *Session) SetVolume(v int) error {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	st, err := s.mediaAction("volume", float64(v))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.volume = int(st.Volume*100 + 0.5)
	s.mu.Unlock()
	return nil
}

// SetRate sets playback speed (0.25..4).
func (s *Session) SetRate(r float64) error {
	if r < 0.25 {
		r = 0.25
	}
	if r > 4 {
		r = 4
	}
	st, err := s.mediaAction("rate", r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.rate = st.PlaybackRate
	s.mu.Unlock()
	return nil
}

// Seek skips by delta seconds.
func (s *Session) Seek(delta int) error {
	st, err := s.mediaAction("seek", float64(delta))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cur = st.CurrentTime
	s.dur = st.Duration
	s.mu.Unlock()
	return nil
}

// RestartMedia reloads the media tab from the beginning (keeps the share up,
// Chromium's tab capture follows the same tab).
func (s *Session) RestartMedia() error {
	media, _ := s.pages()
	if media == nil {
		return errors.New("media page is not available")
	}
	s.setState(StateJoining, "restarting media")
	if err := s.reopenMedia(); err != nil {
		return err
	}
	if err := s.playMedia(); err != nil {
		return err
	}
	s.setState(StateStreaming, "media restarted")
	return nil
}

// reopenMedia navigates the media tab again.
func (s *Session) reopenMedia() error {
	media, _ := s.pages()
	if media == nil {
		return errors.New("media page is not available")
	}
	target := s.MediaURL
	if s.MediaKind == MediaDirect {
		doc, err := PlayerDataURL(MediaTarget{URL: s.MediaURL, Kind: s.MediaKind}, s.playerOpts)
		if err != nil {
			return err
		}
		target = doc
	}
	if err := media.Timeout(45 * time.Second).Navigate(target); err != nil {
		return err
	}
	_ = media.WaitLoad()
	_, _ = media.Eval(titleLockJS(s.TabTitle))
	return s.waitMedia(25 * time.Second)
}

// queryMedia reads the media status JSON from the page.
func (s *Session) queryMedia() (mediaStatus, error) {
	return s.mediaAction("status", 0)
}

// waitMedia waits until a media element with metadata exists.
func (s *Session) waitMedia(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		st, err := s.queryMedia()
		if err == nil && st.Count > 0 {
			return nil
		}
		if err != nil {
			last = err.Error()
		}
		time.Sleep(700 * time.Millisecond)
	}
	if last == "" {
		last = "timeout"
	}
	return errors.New(last)
}

// mediaAction evaluates a media-control script and decodes its result.
func (s *Session) mediaAction(action string, arg float64) (mediaStatus, error) {
	media, _ := s.pages()
	if media == nil {
		return mediaStatus{}, errors.New("media page is not available")
	}
	res, err := media.Timeout(6 * time.Second).Eval(mediaControlJS(action, arg))
	if err != nil {
		return mediaStatus{}, err
	}
	var st mediaStatus
	if err := json.Unmarshal([]byte(res.Value.Str()), &st); err != nil {
		return mediaStatus{}, fmt.Errorf("decode media status: %w", err)
	}
	return st, nil
}

// playerProblem reports a visible playback blocker, if any.
func (s *Session) playerProblem() string {
	media, _ := s.pages()
	if media == nil {
		return ""
	}
	res, err := media.Eval(playerProblemJS())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Value.Str())
}

// clearMediaInterstitials dismisses consent dialogs for YouTube/Google.
func (s *Session) clearMediaInterstitials(p *rod.Page) error {
	if !strings.Contains(strings.ToLower(p.MustInfo().URL), "consent.") {
		return nil
	}
	for i := 0; i < 3; i++ {
		if clicked, _ := clickByText(p, []string{"accept all", "i agree", "agree to all"}, 2*time.Second); clicked {
			time.Sleep(1500 * time.Millisecond)
		}
		if !strings.Contains(strings.ToLower(p.MustInfo().URL), "consent.") {
			return nil
		}
	}
	return nil
}
