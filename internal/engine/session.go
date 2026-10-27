package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// errHelpURL is a hint appended to "no media" failures.
const errHelpURL = "make sure the link is publicly reachable from this server"

// Pacing constants for the stage poll loop.
//
// When a meeting is host-restricted ("no one can join unless invited or
// admitted"), polling the DOM every sub-second is wasted CDP churn: the page
// cannot change until we re-request entry (reload), so a relaxed poll bounded by
// the re-entry deadline cuts CPU down to near zero during long host waits.
const (
	meetReloadInterval  = 30 * time.Second
	blockedPollInterval = 5 * time.Second
	lobbyPollInterval   = 2 * time.Second
	hostWaitHeartbeat   = 30 * time.Second
)

// Session is one Google Meet participant that shares one media tab.
type Session struct {
	ID        SessionID
	MeetURL   string
	MediaURL  string
	MediaKind MediaKind
	TabTitle  string
	AudioLang string

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
	audioRep AudioTrackReport

	// seekMu serializes seek calls on one session: rapid button presses
	// (and a seek racing a switch) must not interleave their
	// read-position → seekTo → verify round-trips, or the second call can
	// compute its target from a position the first one already left.
	seekMu sync.Mutex

	lastMeetLoad time.Time

	// adSkipEnabled gates the ad-skip monitor. A nil config value (the default)
	// means "on": pressing YouTube's own skip button is what a human viewer
	// does, so it takes an explicit opt-out to disable it.
	adSkipEnabled *bool

	// playMode selects how the media reaches the share (Browser default).
	playMode mediateam.Mode
	// localFile is the Download & Stream artifact, when the session plays
	// from a downloaded file instead of the remote URL.
	localFile *localMedia
	// expiryWarned avoids repeating the "media expired" notice on the card.
	expiryWarned bool

	// googleCookies holds authenticated Google account cookies injected for this session.
	googleCookies []*proto.NetworkCookieParam
	// guestNameOverride optionally replaces the default Meet guest name.
	guestNameOverride string

	browser   *rod.Browser
	mediaPage *rod.Page
	meetPage  *rod.Page

	// pub tracks Meet's own publication counters between samples.
	pub publishTracker

	// switchMu serializes SwitchMedia calls: only one navigate → prepare →
	// play cycle may drive the media tab at a time. switchGen is the generation
	// counter that lets an in-flight switch notice a newer request and abort.
	switchMu  sync.Mutex
	switchGen atomic.Uint64
	// switchRun is the browser-facing half of a switch (navigate → prepare →
	// play). It is nil in production, where the real rod-based implementation
	// runs; package tests may substitute a fake so the orchestration — the
	// latest-wins handoff, the state transitions and the rollback — can be
	// exercised without Chromium.
	switchRun func(target MediaTarget, stale func() bool) error

	// mediaRun is the browser-facing half of a media action (seek, pause,
	// volume...). It is nil in production, where the real CDP page is driven
	// directly; package tests may substitute a fake player so the seek state
	// machine — readiness, clamping, verification, rapid presses — can be
	// exercised without Chromium.
	mediaRun func(action string, arg float64) (mediaStatus, error)

	// Playlist queue fields
	queueMu     sync.RWMutex
	queue       []QueueItem
	queueIdx    int
	nextQueueID int
	// repeatMode is what happens when the current media ends ("off" default).
	repeatMode string
	// shuffle picks a random upcoming item when playback auto-advances.
	shuffle bool
	// autoAdvance plays the next queue item when the current media ends.
	// It is on by default: a playlist keeps playing without operator input.
	autoAdvance bool
	// rng drives shuffle picks; injectable for deterministic tests.
	rng *rand.Rand
}

// Status is a dashboard-friendly snapshot.
type Status struct {
	ID             SessionID
	State          string
	Detail         string
	Error          string
	Title          string
	MeetURL        string
	MediaURL       string
	Paused         bool
	Muted          bool
	Volume         int
	Rate           float64
	Cur            float64
	Dur            float64
	Created        time.Time
	Joined         time.Time
	Ended          time.Time
	AudioLang      string
	AudioTrack     string
	FallbackReason string
	// PlayMode is the Download & Stream mode of the session.
	PlayMode string
	// LocalFile reports the downloaded media backing the session, if any.
	LocalFile localFileStatus
	// Queue fields
	QueueLength int
	QueueIndex  int
	HasNext     bool
	HasPrevious bool
	// RepeatMode is the end-of-media behaviour ("off", "one", "all").
	RepeatMode string
	// Shuffle reports whether auto-advance picks random upcoming items.
	Shuffle bool
	// AutoAdvance reports whether the queue advances when media ends.
	AutoAdvance bool
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
//
// The mu section must stay flat: it captures the plain fields and releases
// the lock before localFileStatus() takes its own read lock. Calling it
// under the outer RLock is a recursive read that deadlocks whenever a
// writer queues in between (the RWMutex writer-preference rule) — with a
// Next worker waiting on switchMu that is a whole-session stall.
func (s *Session) Snapshot() Status {
	s.mu.RLock()
	st := Status{
		ID:             s.ID,
		State:          s.state,
		Detail:         s.detail,
		Error:          s.errMsg,
		Title:          s.title,
		MeetURL:        s.MeetURL,
		MediaURL:       s.MediaURL,
		Paused:         s.paused,
		Muted:          s.muted,
		Volume:         s.volume,
		Rate:           s.rate,
		Cur:            s.cur,
		Dur:            s.dur,
		Created:        s.created,
		Joined:         s.joined,
		Ended:          s.ended,
		AudioLang:      s.AudioLang,
		AudioTrack:     s.audioRep.SelectedTrackLabel(),
		FallbackReason: s.audioRep.FallbackReason,
		PlayMode:       string(s.playMode),
	}
	s.mu.RUnlock()

	s.queueMu.RLock()
	st.QueueLength = len(s.queue)
	st.QueueIndex = s.queueIdx
	st.RepeatMode = NormalizeRepeatMode(s.repeatMode)
	st.Shuffle = s.shuffle
	st.AutoAdvance = s.autoAdvance
	s.queueMu.RUnlock()
	st.HasNext = st.QueueIndex >= 0 && st.QueueIndex+1 < st.QueueLength
	st.HasPrevious = st.QueueIndex > 0

	st.LocalFile = s.localFileStatus()
	return st
}

// AudioReport returns a copy of the audio track report.
func (s *Session) AudioReport() AudioTrackReport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.audioRep
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
		s.logger().Infof("session %s: %s (%s)", s.ID, state, detail)
		s.emit(EventState, detail)
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
	if s.mgr != nil {
		s.mgr.emit(Event{Session: s.ID, Kind: kind, Text: text})
	}
}

// logger returns the session logger, or a silent one when the session was built
// by hand (tests) instead of the manager.
func (s *Session) logger() Logger {
	if s.log != nil {
		return s.log
	}
	return noopLogger{}
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

	// Prefer a pre-warmed browser (WarmUp enabled): same isolation guarantees
	// (fresh profile dir), skips the cold process start. On any warm-path
	// failure fall back to a cold launch.
	if b, release, ok := s.mgr.acquireWarmBrowser(); ok {
		s.log.Infof("[PIPELINE:session] [STEP 1/6] session %s: using warm Chromium (cache warm)", s.ID)
		defer release()
		s.mu.Lock()
		s.browser = b
		s.mu.Unlock()
	} else {
		b, release, err := s.mgr.acquireBrowser()
		if err != nil {
			s.setError(fmt.Errorf("start chromium: %w", err))
			return
		}
		defer release()
		s.mu.Lock()
		s.browser = b
		s.mu.Unlock()
	}

	s.log.Infof("[PIPELINE:session] [STEP 1/6] session %s: Chromium ready", s.ID)
	s.mu.Lock()
	br := s.browser
	s.mu.Unlock()
	if len(s.googleCookies) > 0 && br != nil {
		s.log.Infof("[PIPELINE:googleauth] session %s: injecting %d Google session cookies into browser context", s.ID, len(s.googleCookies))
		if err := (proto.NetworkSetCookies{Cookies: s.googleCookies}).Call(br); err != nil {
			s.log.Warnf("[PIPELINE:googleauth] session %s: inject cookies into browser: %v", s.ID, err)
		} else {
			s.log.Infof("[PIPELINE:googleauth] session %s: successfully injected %d Google cookies", s.ID, len(s.googleCookies))
		}
	}

	// Skippable ads: the monitor watches the media tab from the moment it
	// exists, so a pre-roll is dismissed before it ever reaches the share.
	// Non-YouTube media makes the probe a no-op on the first sample. An
	// explicit media.skip_youtube_ads: false disables it.
	if !s.adSkipDisabled() {
		adSkipStop := make(chan struct{})
		go s.MonitorSkipAds(adSkipStop)
		defer close(adSkipStop)
	}

	// Download & Stream: fetch the media to disk BEFORE the tab is opened so
	// the media tab loads a local, always-available file. The download honours
	// its own size/time/concurrency limits and starts the expiry clock on
	// completion; a failure here is terminal for Download mode, a fallback
	// signal for Auto, and a no-op for Browser.
	if mode := s.playMode; mode == mediateam.ModeDownload || mode == mediateam.ModeAuto {
		if err := s.prepareLocalMedia(); err != nil {
			if mode == mediateam.ModeDownload {
				if s.ctx.Err() == nil {
					s.setError(err)
				}
				return
			}
			// Auto: keep the browser pipeline, record why. No retry loop —
			// the browser path is the primary, this was the fallback.
			s.log.Warnf("[PIPELINE:session] session %s: download fallback failed, continuing with browser playback: %v", s.ID, err)
			s.setDetail("download unavailable: " + truncateStr(err.Error(), 80))
		}
	}

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
		if s.ctx.Err() != nil {
			s.setState(StateCancelled, "cancelled")
			return
		}
		if strings.Contains(err.Error(), "timed out") {
			s.setState(StateTimedOut, err.Error())
		}
		s.setError(err)
		return
	}
	s.mu.Lock()
	s.joined = time.Now()
	s.mu.Unlock()
	s.setState(StateAdmitted, "[4/6] Admitted to meeting")
	s.log.Infof("[PIPELINE:session] [STEP 4/6] session %s: admitted to meeting after %s", s.ID, s.joined.Sub(s.created).Round(time.Second))
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
		// Auto fallback: the browser pipeline could not play the remote
		// source, so try the downloaded file — exactly once, bounded, and
		// only when nothing was already downloaded (Download mode already
		// tried, and Auto never retries a failed fallback).
		if s.playMode == mediateam.ModeAuto && s.localFile == nil {
			s.log.Infof("[PIPELINE:session] session %s: auto fallback — downloading media after browser playback failed", s.ID)
			if ferr := s.fallbackToDownload(); ferr != nil {
				s.log.Warnf("[PIPELINE:session] session %s: auto fallback download failed: %v", s.ID, ferr)
			}
		}
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
	s.releaseLocalMedia("session stop or close")
	s.cleanupQueue()
	s.mu.Lock()
	if !IsTerminalState(s.state) {
		if s.ctx != nil && s.ctx.Err() != nil {
			s.state = StateCancelled
			s.detail = "cancelled"
		} else {
			s.state = StateCompleted
			if s.detail == "" || s.detail == "streaming" || s.detail == "[6/6] Streaming live" {
				s.detail = "left the meeting"
			}
		}
	}
	if s.ended.IsZero() {
		s.ended = time.Now()
	}
	s.mu.Unlock()
	s.emit(EventEnded, "session closed")
}

// localexpiresAtText renders the deadline for a log line.
func localexpiresAtText(l *localMedia) string {
	if l == nil {
		return "?"
	}
	return l.expiresAt.Format(time.RFC3339)
}

// maxMediaRecoveries bounds self-healing so a genuinely broken link cannot spin
// forever; the share stays up either way and the log carries the diagnostics.
const maxMediaRecoveries = 5

// watch keeps cached media stats fresh, self-heals the media tab and detects a
// dead share/page.
//
// Media recovery NEVER re-presents: getDisplayMedia is bound to the media tab, so
// dismissing a consent wall, re-fitting the surface or replaying the same tab is
// invisible to the meeting. Only a share Meet itself dropped is re-presented, and
// only after two consecutive misses so a single slow poll cannot cost a re-click.
func (s *Session) watch() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	presentTries := 0
	presentMisses := 0
	lastPresentCheck := time.Now()
	lastLog := time.Now()
	lastMediaCheck := time.Now()
	lastRecovery := time.Time{}
	recoveries := 0
	deadPolls := 0
	stalled := 0
	lastCur := -1.0
	exhaustedLogged := false
	// Publishing watchdog: Meet's DOM can claim a share while its outbound
	// video channel is dead. Two consecutive named problems (≥ 16s apart) with
	// a recoverable class cost one bounded re-present (max 2), so a dropped
	// share heals itself instead of streaming to an audience that sees nothing.
	publishMisses := 0
	publishFixes := 0
	lastPublishFix := time.Time{}
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		// The TTL is an orphan-cleanup deadline, not a playback deadline.
		// A leased file survives past ExpiresAt for the whole owning
		// session (any duration, any pause), so an expired-but-leased file
		// is healthy and must NOT stop playback. Only an expired file the
		// janitor actually deleted surfaces as a player error below, which
		// the media status reports as a decode/source failure.
		local := s.localFile
		if local != nil && !local.expiresAt.IsZero() && time.Now().After(local.expiresAt) {
			if !s.expiryWarned {
				s.expiryWarned = true
				if mgr := s.mediaManager(); mgr != nil && mgr.Enabled() && mgr.LeaseOwners(local.id) != nil {
					s.log.Infof("[PIPELINE:session] session %s: media is past its TTL and held by an active lease (%s) — playback continues", s.ID, local.id)
				} else {
					s.log.Warnf("[PIPELINE:session] session %s: downloaded media is past its TTL (%s) — the janitor will reclaim it when unused", s.ID, localexpiresAtText(local))
				}
			}
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
			if st.Ended && !st.Paused {
				if s.snapshotState() == StateStreaming {
					switch s.handleQueueEnded() {
					case queueEndNext, queueEndWrap:
						s.log.Infof("[PIPELINE:queue] session %s: media playback ended, advancing queue", s.ID)
					case queueEndRestart:
						s.log.Infof("[PIPELINE:queue] session %s: media playback ended, repeat-one restart", s.ID)
					}
				}
			} else if st.Count > 0 && !st.Paused {
				// A frozen position with a "playing" player is a stall (buffering,
				// a rotated consent wall, a dead decoder) even when paused=false.
				if lastCur >= 0 && st.CurrentTime-lastCur < 0.05 {
					stalled++
				} else {
					stalled = 0
				}
			}
			lastCur = st.CurrentTime
		}

		// Heartbeat log every 10 seconds for real-time diagnostics
		if time.Since(lastLog) >= 10*time.Second {
			lastLog = time.Now()
			s.mu.RLock()
			s.log.Infof("[PIPELINE:stream] session %s: [heartbeat] cur=%.1fs/%.1fs paused=%v muted=%v vol=%d%% title=%q",
				s.ID, s.cur, s.dur, s.paused, s.muted, s.volume, s.title)
			s.mu.RUnlock()
		}

		// Self-heal the media tab: a consent wall can appear mid-stream, and the
		// player can stall. Repair the media tab only — never the share.
		if s.snapshotState() == StateStreaming && time.Since(lastMediaCheck) >= 10*time.Second {
			lastMediaCheck = time.Now()
			reason := ""
			if stalled >= 3 {
				reason = fmt.Sprintf("playback position frozen for %ds", stalled*2)
			}
			if h, st2, _ := s.mediaHealth(); h != "" {
				reason = h
				if st2.Consent {
					s.log.Warnf("[PIPELINE:recovery] session %s: consent wall mid-stream (%s): %s", s.ID, st2.Surface, st2.describe())
				}
			}
			switch {
			case reason == "":
				recoveries = 0
				exhaustedLogged = false
			case recoveries >= maxMediaRecoveries:
				if !exhaustedLogged {
					exhaustedLogged = true
					s.log.Errorf("[PIPELINE:recovery] session %s: %s; %d recovery attempts already failed — share left up, run `--media-probe <link>` for the media diagnostics", s.ID, reason, recoveries)
					s.setDetail("[6/6] Media unhealthy: " + reason)
				}
			case time.Since(lastRecovery) >= 20*time.Second:
				recoveries++
				lastRecovery = time.Now()
				stalled = 0
				if rerr := s.recoverMedia(reason); rerr != nil {
					s.log.Errorf("[PIPELINE:recovery] session %s: attempt %d failed: %v", s.ID, recoveries, rerr)
					s.setDetail("[6/6] Media recovery failed: " + rerr.Error())
				} else {
					lastCur = -1
				}
			}
		}

		// Verify the presentation is still up; re-present a bounded number of
		// times (Meet sometimes drops a share on network hiccups). Two
		// consecutive misses are required: one slow eval must not cost a
		// needless Share Screen click.
		if time.Since(lastPresentCheck) > 8*time.Second {
			lastPresentCheck = time.Now()
			// Meet's own encoder first: "presenting" is a DOM claim, the
			// outbound-rtp counters are what participants actually receive.
			problem := s.logMeetPublish()
			class := publishProblemClass(problem)
			if problem != "" {
				s.log.Warnf("[PIPELINE:publish] session %s: %s", s.ID, problem)
				s.setDetail("[6/6] Share problem: " + problem)
				// A recoverable drop (no senders / no capture stream) is healed
				// by clicking Share again — same treatment as presenting()
				// going false, and bounded the same way.
				if class == "share-dropped" {
					publishMisses++
					s.log.Warnf("[PIPELINE:publish] session %s: publication dropped (%d/2 consecutive)", s.ID, publishMisses)
					if publishMisses >= 2 && publishFixes < 2 && time.Since(lastPublishFix) >= 30*time.Second {
						publishMisses = 0
						publishFixes++
						lastPublishFix = time.Now()
						s.log.Warnf("[PIPELINE:publish] session %s: re-presenting to restore the video track (%d/2)", s.ID, publishFixes)
						if err := s.startPresenting(); err != nil {
							s.log.Warnf("[PIPELINE:publish] session %s: re-present failed: %v", s.ID, err)
						} else {
							_ = s.playMedia()
						}
					}
				}
			} else {
				publishMisses = 0
			}
			if ok, _ := s.presenting(); ok {
				presentMisses = 0
			} else {
				presentMisses++
				s.log.Warnf("[PIPELINE:present] session %s: presenting() false (%d/2 consecutive)", s.ID, presentMisses)
				if presentMisses >= 2 {
					presentMisses = 0
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
	kind := s.MediaKind
	sourceURL := s.MediaURL
	// Download & Stream: when a local file backs the session, the media tab
	// loads the loopback URL of that file instead of the remote one. The
	// player document is the same, only the src changes.
	s.mu.RLock()
	local := s.localFile
	s.mu.RUnlock()
	if local != nil && strings.TrimSpace(local.url) != "" {
		target = local.url
		kind = MediaDirect
		sourceURL = local.path
	}
	if kind == MediaDirect {
		doc, err := PlayerHTML(MediaTarget{URL: target, Kind: kind}, s.playerOpts)
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
		return fmt.Errorf("load media %s: %w", sourceURL, err)
	}
	_ = p.WaitLoad()

	// Keep the tab title stable so Chromium's auto-select capture-source finds it.
	if _, err := p.Eval(titleLockJS(s.TabTitle)); err != nil {
		s.log.Debugf("session %s: title lock failed: %v", s.ID, err)
	}

	// Double-check tab title after lock.
	if curTitle, err := p.Eval(`() => document.title`); err == nil {
		s.log.Infof("[PIPELINE:media] session %s: media tab title locked to %q (current=%v)", s.ID, s.TabTitle, curTitle.Value)
	}

	// Consent walls, surface fitting and the buffered-video wait. The wall is
	// cleared BEFORE the cosmetic CSS is installed: a hidden dialog cannot be
	// clicked, and the video never paints while the lightbox owns the page.
	s.setState(StateJoining, "[2/6] Waiting for playable video")
	s.log.Infof("[PIPELINE:media] session %s: preparing the media surface (consent → fit → ready)...", s.ID)
	if err := s.prepareMedia(p); err != nil {
		return err
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
	// Record Meet's own peer connections and capture stream before the app
	// boots: without that, a share that is "presenting" but encoding nothing is
	// indistinguishable from a healthy one from inside the page.
	if _, err := p.EvalOnNewDocument(meetPeerHookJS()); err != nil {
		s.log.Warnf("[PIPELINE:publish] session %s: install publisher hook: %v", s.ID, err)
	}
	if len(s.googleCookies) > 0 {
		if err := (proto.NetworkSetCookies{Cookies: s.googleCookies}).Call(p); err != nil {
			s.log.Warnf("[PIPELINE:googleauth] session %s: inject cookies into meet tab: %v", s.ID, err)
		}
	}
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
	lastWaitNote := time.Time{}
	reloadCount := 0
	joinClicked := false
	joinClickedAt := time.Time{}

	computeReloadInterval := func(attempts int) time.Duration {
		var base time.Duration
		switch {
		case attempts == 0:
			base = 2500 * time.Millisecond
		case attempts == 1:
			base = 4000 * time.Millisecond
		case attempts == 2:
			base = 6000 * time.Millisecond
		case attempts < 5:
			base = 10 * time.Second
		default:
			base = 15 * time.Second
		}
		jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
		return base + jitter
	}
	currentReloadWait := computeReloadInterval(0)

	for time.Now().Before(deadline) {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		// pace is the delay before the next stage poll. States that can change
		// on their own (loading, prejoin, lobby) need a short poll; a host-gated
		// page cannot change until Meet is re-requested, so polling it every
		// sub-second is pure CDP churn (it used to re-render the dashboard card
		// and rewrite the state file ~1.2x/second for minutes on end).
		pace := 900 * time.Millisecond
		stage, info := s.meetStage()
		if stage != lastStage {
			lastStage = stage
			s.log.Infof("session %s: meet stage=%s %q", s.ID, stage, info)
		}
		switch stage {
		case "meeting":
			s.log.Infof("[PIPELINE:meet] session %s: joined the meeting (meeting stage detected)", s.ID)
			s.setState(StateAdmitted, "[4/6] Admitted to meeting")
			s.turnOffMeetingCamera()
			return nil
		case "lobby":
			extendWaitForHost()
			s.setState(StateWaitingForAdmission, "[4/6] Waiting for host to admit bot")
			pace = lobbyPollInterval
			s.logHostWait(&lastWaitNote, "lobby", "waiting to be admitted", 0, hostWaitStarted)
		case "blocked":
			if phrase := blockedFatalPhrase(info); phrase != "" {
				return fmt.Errorf("cannot join this meeting: %s", phrase)
			}
			// Host-gated: Meet only lets people in after the first host has
			// joined. The blocked page also bounces back to Meet's home screen
			// after ~60 s, so re-request entry on a bounded cadence until the
			// gate lifts. The card keeps a stable detail string (no attempt
			// counter): the counter belongs to the log, where it costs nothing,
			// not to the dashboard text where every increment forces a message
			// edit and a state-file rewrite.
			extendWaitForHost()
			s.setState(StateWaitingForAdmission, "[4/6] Host-restricted meeting: waiting for host")
			interval := currentReloadWait
			if time.Since(s.lastMeetLoad) >= interval {
				reloadCount++
				currentReloadWait = computeReloadInterval(reloadCount)
				joinClicked = false
				s.log.Infof("[PIPELINE:meet] session %s: host-restricted meeting, re-requesting entry (attempt %d, waited %s)",
					s.ID, reloadCount, time.Since(hostWaitStarted).Round(time.Second))
				if err := s.reopenMeet(); err != nil {
					return fmt.Errorf("reload the meeting page: %w", err)
				}
				pace = 2 * time.Second
			} else {
				// Nothing can change until the next re-request: wait it out
				// (bounded, so admission is still noticed quickly).
				pace = blockedPollInterval
				if remaining := interval - time.Since(s.lastMeetLoad); remaining < pace {
					// Never trim the wait to nothing: a sub-tick pace would
					// turn the last moments before a reload into a poll storm.
					if remaining < 250*time.Millisecond {
						remaining = 250 * time.Millisecond
					}
					pace = remaining
				}
				s.logHostWait(&lastWaitNote, "blocked", "host-restricted meeting", reloadCount, hostWaitStarted)
			}
		case "login":
			if len(s.googleCookies) > 0 {
				return errors.New("Google account cookies were rejected or expired; please re-export and update via /account")
			}
			return errors.New("this meeting requires a signed-in Google account; TeleMeetBot joins as a guest only (add your Google account in /account)")
		case "prejoin":
			s.setState(StateJoining, "[4/6] Prejoin screen: muting camera/mic & entering")
			s.fillGuestName()
			s.mutePrejoinDevices()
			if !joinClicked || time.Since(joinClickedAt) > 12*time.Second {
				if clicked, label := s.clickJoin(); clicked {
					joinClicked = true
					joinClickedAt = time.Now()
					s.log.Infof("[PIPELINE:meet] session %s: clicked %q", s.ID, label)
					s.setState(StateWaitingForAdmission, "[4/6] Waiting for host to admit bot")
					pace = 1500 * time.Millisecond
				}
			} else {
				pace = 1000 * time.Millisecond
			}
		default:
			// Unknown/loading: a little patience.
		}
		if err := s.ctxSleep(pace); err != nil {
			return err
		}
	}
	if !hostWaitStarted.IsZero() {
		return fmt.Errorf("waited %s for a host-restricted meeting: no host ever started it (google: no one can join unless invited or admitted by the host). if the link is wrong or expired, Meet shows this same page — verify the meeting code with the host", deadline.Sub(hostWaitStarted).Round(time.Second))
	}
	return fmt.Errorf("meet join timed out after %s (stage=%s)", timeout, lastStage)
}

// logHostWait emits a bounded heartbeat while the bot waits for a host, so a
// long host-restricted wait stays diagnosable without a log line per poll.
func (s *Session) logHostWait(last *time.Time, stage, what string, attempts int, since time.Time) {
	if !last.IsZero() && time.Since(*last) < hostWaitHeartbeat {
		return
	}
	*last = time.Now()
	waited := "unknown"
	if !since.IsZero() {
		waited = time.Since(since).Round(time.Second).String()
	}
	if attempts > 0 {
		s.log.Infof("[PIPELINE:meet] session %s: %s (%s), waited %s, re-entry attempts=%d", s.ID, what, stage, waited, attempts)
		return
	}
	s.log.Infof("[PIPELINE:meet] session %s: %s (%s), waited %s", s.ID, what, stage, waited)
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

// startPresenting drives Meet's share flow. Two shapes exist: builds that open a
// source menu ("A tab" / "A window" / "Entire screen") and builds that call
// getDisplayMedia straight from the toolbar button and show "Presentation is
// starting..." until the capture stream is live. Either way Chromium answers the
// picker through --auto-select-tab-capture-source-by-title, which only matches a
// tab whose browser-side title is the configured capture source.
func (s *Session) startPresenting() error {
	timeout := time.Duration(s.mgr.meetCfg.PresentTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	// The capture matcher works on Chromium's own tab titles; prove the media tab
	// is visible there before clicking, otherwise getDisplayMedia waits for a
	// picker that headless cannot show.
	if err := s.ensureCaptureTitle(s.TabTitle); err != nil {
		s.log.Warnf("[PIPELINE:present] session %s: capture tab title check: %v", s.ID, err)
	} else {
		s.log.Infof("[PIPELINE:present] session %s: chromium sees capture tab %q", s.ID, s.TabTitle)
	}
	deadline := time.Now().Add(timeout)
	startingSince := time.Time{}
	lastState := ""
	lastNote := time.Time{}
	lastWarn := time.Time{}
	for time.Now().Before(deadline) {
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		if ok, _ := s.presenting(); ok {
			return nil
		}
		// Meet is already starting the capture (getDisplayMedia in flight): wait
		// for the stream instead of hammering the toolbar again.
		if state := s.presentControlState(); strings.Contains(state, "starting") {
			if startingSince.IsZero() {
				startingSince = time.Now()
				s.log.Infof("[PIPELINE:present] session %s: Meet is starting the capture (%q), waiting for the stream", s.ID, state)
			}
			lastState = state
			if time.Since(lastNote) >= 10*time.Second {
				lastNote = time.Now()
				s.log.Infof("[PIPELINE:present] session %s: still starting the capture after %s (control=%q, chromium tabs=%v)",
					s.ID, time.Since(startingSince).Round(time.Second), state, s.captureTabTitles())
			}
			s.setState(StateSharing, "[5/6] Waiting for Meet to start the capture")
			time.Sleep(700 * time.Millisecond)
			continue
		}
		startingSince = time.Time{}
		if s.meetTab() != nil {
			opened, label := s.clickPresentButton()
			if opened {
				if label == "opened more options" {
					time.Sleep(350 * time.Millisecond)
					if ok, _ := s.presenting(); ok {
						return nil
					}
					// Pick the share item from the now-open More options menu.
					if itemPicked, itemLabel := s.clickMoreOptionsShareItem(); itemPicked {
						label = itemLabel
						s.log.Infof("[PIPELINE:present] session %s: picked share item from more-options menu (%s)", s.ID, itemLabel)
					} else {
						s.log.Warnf("[PIPELINE:present] session %s: more-options menu did not reveal a share item; dumping menu DOM", s.ID)
						s.dumpPresentMenu()
						continue
					}
				}
				lastState = label
				s.log.Infof("[PIPELINE:present] session %s: opened present menu (%s)", s.ID, label)
				s.setState(StateSharing, "[5/6] Opened present menu: selecting tab")
				time.Sleep(900 * time.Millisecond)
				// The click may already have started the capture (Meet builds
				// without a source menu): re-check before hunting for a menu.
				if ok, _ := s.presenting(); ok {
					s.log.Infof("[PIPELINE:present] session %s: presenting confirmed active", s.ID)
					return nil
				}
				clicked, item := s.clickPresentSource()
				if !clicked {
					// No source menu is normal on builds that start the capture
					// straight from the toolbar button; only dump the DOM when
					// nothing is starting either.
					st := s.presentControlState()
					if strings.Contains(st, "starting") || strings.Contains(st, "stop") {
						s.log.Infof("[PIPELINE:present] session %s: no source menu (build starts the capture directly)", s.ID)
					} else {
						s.log.Warnf("[PIPELINE:present] session %s: present menu item not found (label=%q); dumping menu DOM", s.ID, item)
						s.dumpPresentMenu()
					}
				}
				if clicked {
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
			} else {
				if time.Since(lastWarn) >= 10*time.Second {
					lastWarn = time.Now()
					s.log.Warnf("[PIPELINE:present] session %s: clickPresentButton found no button; visible buttons: %s", s.ID, s.dumpToolbarButtons())
				}
			}
		}
		time.Sleep(900 * time.Millisecond)
	}
	return fmt.Errorf("could not start screen sharing in Google Meet (present flow timed out after %s; present control=%q; chromium tabs=%v); the bot is in the meeting — retry from the dashboard",
		timeout, lastState, s.captureTabTitles())
}

// playMedia starts playback after the share is live and proves it: the position
// must advance and the frames must be real pixels (not black, not decoder-green).
// A consent wall that lands between opening the tab and the share going live is
// cleared here without touching the Meet presentation.
func (s *Session) playMedia() error {
	media := s.mediaTab()
	if media == nil {
		return errors.New("media page is not available")
	}
	if res, err := dismissConsent(media, s.log, 15*time.Second); err != nil {
		s.log.Warnf("[PIPELINE:playback] session %s: consent check before play: %v", s.ID, err)
	} else if res.Present {
		s.log.Infof("[PIPELINE:playback] session %s: consent %s", s.ID, res.describe())
	}
	if st, err := s.mediaAction("unmute", 0); err == nil {
		s.mu.Lock()
		s.muted = st.Muted
		s.volume = int(st.Volume*100 + 0.5)
		if st.Title != "" {
			s.title = st.Title
		}
		s.mu.Unlock()
	}
	rep, err := ensurePlayback(media, s.log, 1.0, 20*time.Second)
	s.mu.Lock()
	s.cur, s.dur, s.paused = rep.Current, rep.Duration, rep.Paused
	s.mediaErr = rep.DrawErr
	if rep.Title != "" {
		s.title = rep.Title
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.log.Infof("[PIPELINE:playback] session %s: %s", s.ID, rep.describe())
	return nil
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

// Seek skips by delta seconds. The implementation lives in seek.go: it reads
// the real player position, clamps to the duration, verifies the result and
// reports failures honestly.

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
	if err := s.navigateMediaTab(media, s.MediaURL, s.MediaKind); err != nil {
		return err
	}
	return s.prepareMedia(media)
}

// navigateMediaTab points the existing media tab at target. The share follows
// the tab (Chromium's getDisplayMedia is bound to the tab, not the URL), so
// navigating the same tab is what switches what participants see. Direct media
// files are wrapped in the loopback player document, exactly like openMedia.
func (s *Session) navigateMediaTab(media *rod.Page, url string, kind MediaKind) error {
	target := url
	if kind == MediaDirect {
		// Loopback HTTP (not data:) so Chrome always allows the media fetch.
		doc, err := PlayerHTML(MediaTarget{URL: url, Kind: kind}, s.playerOpts)
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
	return nil
}

// queryMedia reads the media status JSON from the page.
func (s *Session) queryMedia() (mediaStatus, error) {
	return s.mediaAction("status", 0)
}

// truncateStr shortens s for log lines.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// mediaAction evaluates a media-control script and decodes its result. A CDP
// round-trip can fail transiently (a tab reload mid-press, a navigation, the
// browser reclaiming the target) — those look identical to a genuinely dead
// tab, so one retry after re-reading the tab pointer keeps rapid seek presses
// working through a reload without hiding a real failure: the retry only runs
// when the first attempt errored, and the last error is what the caller sees.
func (s *Session) mediaAction(action string, arg float64) (mediaStatus, error) {
	st, err := s.mediaActionOnce(action, arg)
	if err == nil {
		return st, nil
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		return mediaStatus{}, err
	}
	// Re-read the pointer: a navigate/reload may have replaced the page object
	// the first attempt used. Only retry once, and only for round-trip errors
	// (a decode failure would just fail the same way twice).
	if second, err2 := s.mediaActionOnce(action, arg); err2 == nil {
		s.log.Debugf("[PIPELINE:media] session %s: media %q action recovered after retry: %v", s.ID, action, err)
		return second, nil
	}
	return mediaStatus{}, err
}

// mediaActionOnce performs a single CDP round-trip for a media action.
func (s *Session) mediaActionOnce(action string, arg float64) (mediaStatus, error) {
	// mediaRun is the browser-facing half of a media action. Tests substitute
	// a fake player; production always goes through the real rod page.
	if s.mediaRun != nil {
		return s.mediaRun(action, arg)
	}
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

// Consent walls are handled by dismissConsent (consent.go).

// localMedia is a downloaded file backing a session's media tab.
type localMedia struct {
	id        string
	url       string // loopback URL the tab loads
	path      string // absolute path inside the managed download dir
	expiresAt time.Time
	via       string
	bytes     int64
}

// localFileStatus is the card-friendly view of a downloaded file.
type localFileStatus struct {
	URL       string    `json:"url,omitempty"`
	Path      string    `json:"path,omitempty"`
	Bytes     int64     `json:"bytes,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Via       string    `json:"via,omitempty"`
}

func (s *Session) localFileStatus() localFileStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.localFile == nil {
		return localFileStatus{}
	}
	return localFileStatus{
		URL:       s.localFile.url,
		Path:      s.localFile.path,
		Bytes:     s.localFile.bytes,
		ExpiresAt: s.localFile.expiresAt,
		Via:       s.localFile.via,
	}
}

// PlaybackMode returns the session's Download & Stream mode. An unset mode is
// Browser so every session built before the feature existed keeps the original
// behaviour.
func (s *Session) PlaybackMode() mediateam.Mode {
	s.mu.RLock()
	mode := s.playMode
	s.mu.RUnlock()
	if string(mode) == "" {
		return mediateam.ModeBrowser
	}
	return mode
}

// SetPlaybackMode changes how the session sources its media. Browser restores
// the remote URL; Download/Auto fetch through the media manager.
func (s *Session) SetPlaybackMode(mode mediateam.Mode) error {
	norm := mediateam.NormalizeMode(string(mode))
	s.mu.Lock()
	oldMode := s.playMode
	s.playMode = norm
	s.mu.Unlock()
	if norm == mediateam.ModeBrowser && oldMode != mediateam.ModeBrowser {
		s.releaseLocalMedia("playback mode set to browser")
	}
	return nil
}

// MediaExpiresAt returns the orphan-cleanup deadline of the session's
// downloaded media (zero when streaming from the remote URL). It is NOT a
// playback deadline: while the session holds an active lease, the file stays
// playable past this moment for the whole session.
func (s *Session) MediaExpiresAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.localFile == nil {
		return time.Time{}
	}
	return s.localFile.expiresAt
}

// SetSwitchRunForTesting overrides the switch runner for deterministic tests.
func (s *Session) SetSwitchRunForTesting(fn func(target MediaTarget, stale func() bool) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.switchRun = fn
}

// MediaStatus is the exported alias of the media probe struct, so test
// seams outside the engine package can express the mediaRun signature.
type MediaStatus = mediaStatus

// SetMediaRunForTesting overrides the media-action runner (seek, pause,
// volume…) for deterministic tests, mirroring SetSwitchRunForTesting.
func (s *Session) SetMediaRunForTesting(fn func(action string, arg float64) (MediaStatus, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mediaRun = fn
}

// SetMediaPageForTesting overrides the media page for deterministic tests.
func (s *Session) SetMediaPageForTesting(p *rod.Page) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mediaPage = p
}
