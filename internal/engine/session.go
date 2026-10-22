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

	lastMeetLoad time.Time

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

// mediaTab returns the media page (nil until the tab exists).
func (s *Session) mediaTab() *rod.Page {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mediaPage
}

// meetTab returns the meet page (nil until the tab exists).
func (s *Session) meetTab() *rod.Page {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meetPage
}

// run is the session state machine.
func (s *Session) run() {
	defer s.cleanup()
	s.setState(StateStarting, "[1/6] Launching Chromium")
	s.log.Infof("[PIPELINE:session] [STEP 1/6] session %s: starting Chromium browser...", s.ID)

	b, release, err := s.mgr.acquireBrowser()
	if err != nil {
		s.setError(fmt.Errorf("start chromium: %w", err))
		return
	}
	defer release()
	s.mu.Lock()
	s.browser = b
	s.mu.Unlock()

	s.log.Infof("[PIPELINE:session] [STEP 1/6] session %s: Chromium ready", s.ID)

	s.setState(StateJoining, "[2/6] Loading media")
	s.log.Infof("[PIPELINE:session] [STEP 2/6] session %s: opening media tab (url=%s kind=%s)...", s.ID, s.MediaURL, s.MediaKind)
	if err := s.openMedia(); err != nil {
		if s.ctx.Err() == nil {
			s.setError(err)
		}
		return
	}
	if s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	mediaTitle, mediaDur := s.title, s.dur
	s.mu.Unlock()
	s.log.Infof("[PIPELINE:session] [STEP 2/6] session %s: media ready (title=%q dur=%.1fs)", s.ID, mediaTitle, mediaDur)

	s.setState(StateJoining, "[3/6] Opening Google Meet")
	s.log.Infof("[PIPELINE:session] [STEP 3/6] session %s: opening Meet tab (url=%s)...", s.ID, s.MeetURL)
	if err := s.openMeet(); err != nil {
		if s.ctx.Err() == nil {
			s.setError(err)
		}
		return
	}
	if s.ctx.Err() != nil {
		return
	}
	s.log.Infof("[PIPELINE:session] [STEP 3/6] session %s: Meet tab loaded", s.ID)

	s.setState(StateJoining, "[4/6] Joining meeting")
	s.log.Infof("[PIPELINE:session] [STEP 4/6] session %s: handling prejoin and host gate...", s.ID)
	if err := s.joinMeeting(); err != nil {
		if s.ctx.Err() == nil {
			s.setError(err)
		}
		return
	}
	s.mu.Lock()
	s.joined = time.Now()
	s.mu.Unlock()
	s.log.Infof("[PIPELINE:session] [STEP 4/6] session %s: joined the meeting after %s", s.ID, s.joined.Sub(s.created).Round(time.Second))
	s.emit(EventJoined, "joined the meeting")

	s.setState(StateSharing, "[5/6] Starting screen share")
	s.log.Infof("[PIPELINE:session] [STEP 5/6] session %s: presenting tab %q...", s.ID, s.TabTitle)
	if err := s.startPresenting(); err != nil {
		if s.ctx.Err() == nil {
			s.setError(err)
		}
		return
	}
	s.emit(EventSharing, "screen share is live")
	s.log.Infof("[PIPELINE:session] [STEP 5/6] session %s: screen share is active!", s.ID)

	s.setState(StateStreaming, "[6/6] Starting media playback")
	s.log.Infof("[PIPELINE:session] [STEP 6/6] session %s: unmuting & playing media...", s.ID)
	if err := s.playMedia(); err != nil {
		s.log.Warnf("[PIPELINE:session] [STEP 6/6] session %s: media play failed after share: %v", s.ID, err)
		s.setDetail("[6/6] Sharing, media start retry: " + err.Error())
	} else {
		s.setDetail("[6/6] Streaming live")
	}
	s.setState(StateStreaming, "[6/6] Streaming live")
	s.log.Infof("[PIPELINE:session] [STEP 6/6] session %s: stream is live (share up, media playing)", s.ID)
	s.emit(EventStreaming, "stream is live")

	s.watch()
}

// cleanup releases pages and finalises the state.
func (s *Session) cleanup() {
	media, meet := s.mediaTab(), s.meetTab()
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
	lastLog := time.Now()
	deadPolls := 0
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

		// Heartbeat log every 10 seconds for real-time diagnostics
		if time.Since(lastLog) >= 10*time.Second {
			lastLog = time.Now()
			s.mu.RLock()
			s.log.Infof("[PIPELINE:stream] session %s: [heartbeat] cur=%.1fs/%.1fs paused=%v muted=%v vol=%d%% title=%q",
				s.ID, s.cur, s.dur, s.paused, s.muted, s.volume, s.title)
			s.mu.RUnlock()
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

		// A closed browser means there is nothing left to monitor. rod's Eval
		// requires function-form JS; a raw expression makes rod call
		// (1).apply and "disconnects" a perfectly healthy session. One transient
		// CDP hiccup must not kill a live stream either, so require consecutive
		// failures before giving up.
		if meet := s.meetTab(); meet != nil {
			if _, err := meet.Timeout(5 * time.Second).Eval(tabAliveJS()); err != nil {
				deadPolls++
				s.log.Warnf("[PIPELINE:stream] session %s: meet tab eval failed (%d/3): %v", s.ID, deadPolls, err)
				if deadPolls >= 3 {
					s.setDetail("browser disconnected")
					s.setError(fmt.Errorf("browser disconnected: %w", err))
					return
				}
			} else {
				deadPolls = 0
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

	// Neutralize background tab throttling & YouTube auto-pause on headless browser
	_, _ = p.EvalOnNewDocument(`
		Object.defineProperty(document, 'hidden', { value: false, writable: false });
		Object.defineProperty(document, 'visibilityState', { value: 'visible', writable: false });
		window.addEventListener('visibilitychange', e => e.stopImmediatePropagation(), true);

		window.__allowPause = false;
		const origPause = HTMLMediaElement.prototype.pause;
		HTMLMediaElement.prototype.pause = function() {
			if (!window.__allowPause) return;
			return origPause.apply(this, arguments);
		};
	`)

	s.setState(StateJoining, "[2/6] Loading media")
	s.log.Infof("[PIPELINE:media] session %s: navigating to target %s...", s.ID, target)
	if err := p.Timeout(50 * time.Second).Navigate(target); err != nil {
		return fmt.Errorf("load media %s: %w", s.MediaURL, err)
	}
	_ = p.WaitLoad()

	// Consent/interstitial walls (YouTube in the EU, etc.).
	s.log.Infof("[PIPELINE:media] session %s: clearing interstitials...", s.ID)
	if err := s.clearMediaInterstitials(p); err != nil {
		return err
	}

	// Keep the tab title stable so Chromium's auto-select capture-source finds it.
	if _, err := p.Eval(titleLockJS(s.TabTitle)); err != nil {
		s.log.Debugf("session %s: title lock failed: %v", s.ID, err)
	}

	s.setState(StateJoining, "[2/6] Waiting for playable video")
	s.log.Infof("[PIPELINE:media] session %s: waiting for playable media element...", s.ID)
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
	s.setLastMeetLoad()
	// Meet is a SPA: give it a beat to render the prejoin screen.
	time.Sleep(1200 * time.Millisecond)
	return nil
}

// joinMeeting drives the prejoin screen and lobby, and waits out Google Meet's
// host-restricted access: when the host has not joined yet, Meet refuses
// everyone ("no one can join a meeting unless invited or admitted by the
// host") — that block clears once the host starts the meeting, so it is
// handled as a waitable state rather than a terminal error.
func (s *Session) joinMeeting() error {
	timeout := time.Duration(s.mgr.meetCfg.JoinTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)

	// Lobby and host-gate both clear only when a host is in the meeting; the
	// configured admit wait (default 5 min) bounds that patience instead of
	// the plain join timeout.
	admitWait := time.Duration(s.mgr.meetCfg.AdmitWaitSeconds) * time.Second
	if admitWait <= 0 {
		admitWait = 5 * time.Minute
	}
	var hostWaitStarted time.Time
	extendWaitForHost := func() {
		if hostWaitStarted.IsZero() {
			hostWaitStarted = time.Now()
		}
		if d := hostWaitStarted.Add(admitWait); d.After(deadline) {
			deadline = d
		}
	}

	var lastStage string
	hostWaitLogged := false
	reloadCount := 0
	for time.Now().Before(deadline) {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		stage, info := s.meetStage()
		if stage != lastStage {
			lastStage = stage
			s.log.Infof("session %s: meet stage=%s %q", s.ID, stage, info)
		} else if stage == "blocked" || stage == "lobby" {
			// Diagnostics: the gate can only be debugged from the log, so
			// keep a bounded heartbeat while waiting on a host.
			if !hostWaitLogged {
				hostWaitLogged = true
				s.log.Debugf("session %s: still %s after %s (detail=%q)", s.ID, stage,
					time.Since(hostWaitStarted).Round(time.Second), info)
			}
		}
		switch stage {
		case "meeting":
			s.log.Infof("[PIPELINE:meet] session %s: joined the meeting (meeting stage detected)", s.ID)
			s.turnOffMeetingCamera()
			return nil
		case "lobby":
			extendWaitForHost()
			s.setState(StateAdmitting, "[4/6] Waiting for host to admit bot")
		case "blocked":
			if phrase := blockedFatalPhrase(info); phrase != "" {
				return fmt.Errorf("cannot join this meeting: %s", phrase)
			}
			// Host-gated: Meet only lets people in after the first host has
			// joined. The blocked page also bounces back to Meet's home screen
			// after ~60 s, so re-request entry on a bounded cadence until the
			// gate lifts.
			extendWaitForHost()
			reloadCount++
			s.setState(StateAdmitting, fmt.Sprintf("[4/6] Host-restricted meeting (attempt %d): waiting for host", reloadCount))
			if time.Since(s.lastMeetLoad) > 30*time.Second {
				s.log.Infof("[PIPELINE:meet] session %s: host-restricted meeting, re-requesting entry (attempt %d)", s.ID, reloadCount)
				if err := s.reopenMeet(); err != nil {
					return fmt.Errorf("reload the meeting page: %w", err)
				}
			}
		case "login":
			return errors.New("this meeting requires a signed-in Google account; TeleMeetBot joins as a guest only")
		case "prejoin":
			s.setState(StateJoining, "[4/6] Prejoin screen: muting camera/mic & entering")
			s.fillGuestName()
			s.mutePrejoinDevices()
			if clicked, label := s.clickJoin(); clicked {
				s.log.Infof("[PIPELINE:meet] session %s: clicked %q", s.ID, label)
				time.Sleep(1500 * time.Millisecond)
			}
		default:
			// Unknown/loading: a little patience.
		}
		time.Sleep(800 * time.Millisecond)
	}
	if !hostWaitStarted.IsZero() {
		return fmt.Errorf("waited %s for a host-restricted meeting: no host ever started it (google: no one can join unless invited or admitted by the host). if the link is wrong or expired, Meet shows this same page — verify the meeting code with the host", deadline.Sub(hostWaitStarted).Round(time.Second))
	}
	return fmt.Errorf("meet join timed out after %s (stage=%s)", timeout, lastStage)
}

// reopenMeet reloads the meeting link on the existing tab (never a new one), so
// Chromium keeps the same tab identity Meet shares from.
func (s *Session) reopenMeet() error {
	meet := s.meetTab()
	if meet == nil {
		return errors.New("meet page is not available")
	}
	if err := meet.Timeout(60 * time.Second).Navigate(s.MeetURL); err != nil {
		return err
	}
	_ = meet.WaitLoad()
	s.setState(StateJoining, "re-requesting entry to the meeting")
	s.setLastMeetLoad()
	// Meet is a SPA: give it a beat to render the prejoin or gate screen.
	time.Sleep(1200 * time.Millisecond)
	return nil
}

// setLastMeetLoad records when the meet tab was (re)loaded, used to pace the
// re-entry attempts against a host-restricted meeting.
func (s *Session) setLastMeetLoad() {
	s.mu.Lock()
	s.lastMeetLoad = time.Now()
	s.mu.Unlock()
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
		if s.meetTab() != nil {
			opened, label := s.clickPresentButton()
			if opened {
				s.log.Infof("[PIPELINE:present] session %s: opened present menu (%s)", s.ID, label)
				s.setState(StateSharing, "[5/6] Opened present menu: selecting tab")
				time.Sleep(900 * time.Millisecond)
				if clicked, item := s.clickPresentSource(); clicked {
					s.log.Infof("[PIPELINE:present] session %s: clicked share source %q", s.ID, item)
					s.setState(StateSharing, fmt.Sprintf("[5/6] Selected tab source %q", item))
					time.Sleep(1200 * time.Millisecond)
					if confirmed, clabel := s.clickShareConfirm(); confirmed {
						s.log.Infof("[PIPELINE:present] session %s: confirmed share dialog %q", s.ID, clabel)
						time.Sleep(1200 * time.Millisecond)
					}
					if ok, _ := s.presenting(); ok {
						s.log.Infof("[PIPELINE:present] session %s: presenting confirmed active", s.ID)
						return nil
					}
				}
			}
		}
		time.Sleep(900 * time.Millisecond)
	}
	return errors.New("could not start screen sharing in Google Meet (present flow timed out); the bot is in the meeting — retry from the dashboard")
}

// playMedia starts playback after the share is live, retrying a few times
// because YouTube's player sometimes swallows the first startPrompt while it
// is still wiring up its own autoplay logic.
func (s *Session) playMedia() error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := s.mediaAction("unmute", 0); err != nil {
			lastErr = err
		}
		st, err := s.mediaAction("play", 0)
		if err != nil {
			lastErr = err
		} else {
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
			if !st.Paused {
				if attempt > 1 {
					s.log.Infof("[PIPELINE:media] session %s: media playing on attempt %d", s.ID, attempt)
				}
				return nil
			}
			lastErr = fmt.Errorf("player still paused (pos=%.1fs)", st.CurrentTime)
		}
		s.log.Warnf("[PIPELINE:media] session %s: play attempt %d/3 failed: %v", s.ID, attempt, lastErr)
		if err := s.ctxSleep(2 * time.Second); err != nil {
			return err
		}
	}
	return lastErr
}

// ctxSleep sleeps for d, aborting early if the session is cancelled.
func (s *Session) ctxSleep(d time.Duration) error {
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-time.After(d):
		return nil
	}
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
	media := s.mediaTab()
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
	media := s.mediaTab()
	if media == nil {
		return errors.New("media page is not available")
	}
	target := s.MediaURL
	if s.MediaKind == MediaDirect {
		// Loopback HTTP (not data:) so Chrome always allows the media fetch.
		doc, err := PlayerHTML(MediaTarget{URL: s.MediaURL, Kind: s.MediaKind}, s.playerOpts)
		if err != nil {
			return err
		}
		target, err = players.playerURL(doc)
		if err != nil {
			return fmt.Errorf("serve player: %w", err)
		}
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
	found := false
	for time.Now().Before(deadline) {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		st, err := s.queryMedia()
		if err == nil && st.Count > 0 {
			if !found {
				found = true
				s.log.Infof("[PIPELINE:media] session %s: media element found (dur=%.1fs paused=%t src=%s)",
					s.ID, st.Duration, st.Paused, truncateStr(st.Src, 60))
			}
			// A <video> tag shows up before the stream is buffered. Waiting for a
			// known duration (or an element already playing) means the first
			// play() lands on real media instead of an empty frame — the
			// difference between a green share and a real one.
			if st.Duration > 0 || !st.Paused {
				return nil
			}
		}
		if err != nil {
			last = err.Error()
		}
		time.Sleep(700 * time.Millisecond)
	}
	if found {
		s.log.Warnf("[PIPELINE:media] session %s: media element present but never buffered (last=%q); continuing", s.ID, last)
		return nil
	}
	if last == "" {
		last = "timeout"
	}
	return errors.New(last)
}

// truncateStr shortens s for log lines.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// mediaAction evaluates a media-control script and decodes its result.
func (s *Session) mediaAction(action string, arg float64) (mediaStatus, error) {
	media := s.mediaTab()
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
	media := s.mediaTab()
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
	url := strings.ToLower(p.MustInfo().URL)
	if strings.Contains(url, "consent.") {
		for i := 0; i < 3; i++ {
			if clicked, _ := clickByText(p, []string{"accept all", "i agree", "agree to all"}, 2*time.Second); clicked {
				time.Sleep(1500 * time.Millisecond)
			}
			if !strings.Contains(strings.ToLower(p.MustInfo().URL), "consent.") {
				break
			}
		}
	}
	// YouTube in-page consent / cookie buttons
	_, _ = p.Timeout(3 * time.Second).Eval(`() => {
		const norm = x => (x || "").toLowerCase().trim();
		const btns = Array.from(document.querySelectorAll('button, [role=button]'));
		for (const b of btns) {
			const t = norm((b.getAttribute('aria-label') || "") + " " + (b.innerText || ""));
			if (t === "accept all" || t === "reject all" || t === "i agree" || t === "agree to all" || t.includes("accept the use of cookies") || t === "reject") {
				try { b.click(); return t; } catch (e) {}
			}
		}
		return "";
	}`)
	return nil
}
