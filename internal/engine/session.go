package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"runtime/debug"
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

// Join-flow pacing: how the join loop observes the page and retries the
// prejoin click. The stage observation is event-driven (see meetStageWait):
// the classifier runs inside the page on a joinPollTick timer and the result
// arrives the moment a decisive stage appears, so a slow SPA transition is
// noticed within one tick without a CDP round trip per poll.
const (
	// joinPollWait bounds one in-page stage observation.
	joinPollWait = 1200 * time.Millisecond
	// joinPollTick is the in-page classifier cadence inside one observation.
	joinPollTick = 250 * time.Millisecond
	// joinClickPace is the short delay after a prejoin click before the next
	// observation: an open meeting answers "Join now" almost immediately.
	joinClickPace = 700 * time.Millisecond
	// openReclickWait is how long an unconfirmed open-meeting "Join now" click
	// may sit on the prejoin screen before the click is retried.
	openReclickWait = 4 * time.Second
	// knockReclickWait spaces out retried "Ask to join" knocks: the knock
	// registers server-side and Meet throttles repeated knocks.
	knockReclickWait = 12 * time.Second
	// maxPrejoinClicks bounds consecutive unconfirmed clicks before the loop
	// escalates to a full page reload instead of clicking into the same page.
	maxPrejoinClicks = 3
	// maxMeetingConfirmChecks bounds the meeting-entry self-check: how many
	// observation rounds a "meeting" stage verdict may stay uncorroborated by
	// verifyInMeeting before it is accepted anyway (the watch loop keeps
	// probing afterwards).
	maxMeetingConfirmChecks = 4
)

// clickRetryWait returns how long a prejoin click may stay unconfirmed before
// another click is justified. "Ask to join" registers a knock server-side and
// may be throttled by Meet, so knocks are retried on a wider cadence than the
// open-meeting "Join now", which must land almost instantly.
func clickRetryWait(label string) time.Duration {
	if strings.Contains(label, "ask") {
		return knockReclickWait
	}
	return openReclickWait
}

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

	// ctx is the session lifespan: the single authoritative cancellation
	// authority. Every session-owned operation (download, subprocess,
	// retry loop, media switch, queue advance) derives from it, so
	// Stop() — which cancels it — reaches all of them through one path
	// instead of a second registry that has to be kept in sync. See
	// lifecycle.go for the full rationale and the contract a worker must
	// honour (observe ctx.Done() and return promptly).
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// doneOnce guarantees cleanup() closes done exactly once even when the
	// session goroutine panics mid-cleanup.
	doneOnce sync.Once
	// workers lists the session-owned background workers that cleanup()
	// must wait for before the session reports STOPPED, and
	// runningWorkers is how many of them are still alive. A session whose
	// own goroutine exited but which still has a live download has not
	// actually stopped — that gap is exactly what these fields close.
	// Guarded by mu. See lifecycle.go.
	workers        map[string]context.CancelFunc
	runningWorkers int
	// stopOnce makes Stop idempotent: the first call records the
	// operator-visible "stopping" detail state and every later call is
	// a cheap no-op, so two simultaneous Stop requests can never race
	// the session into an inconsistent state.
	stopOnce sync.Once

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

	// prepareRun is the browser-free half of the Download & Stream
	// preparation (fetch the media to disk before the media tab opens). It
	// is nil in production, where prepareLocalMedia runs the real media
	// manager; package tests may substitute a fake so run()'s error and
	// panic handling for the download stage can be exercised without
	// Chromium and without a download toolchain.
	prepareRun func() error

	// acquireRun is the browser-acquisition half of run()'s first stage.
	// It is nil in production, where the manager launches a real Chromium;
	// package tests may substitute a fake so run()'s state machine, error
	// handling and panic recovery can be exercised without a browser.
	acquireRun func() (release func(), err error)

	// openRun is the media-tab half of run()'s second stage. It is nil in
	// production, where openMedia opens a real Chromium tab; package tests
	// may substitute a fake so the download-fallback path can be exercised
	// end to end (fallback decision → media "opened" → Meet stages) without
	// a browser. The fake must NOT set s.browser: the whole point is that
	// the session proceeds with no Chromium at all.
	openRun func() error

	// joinRun is the Meet half of run()'s [3/6]+[4/6] stage (open the Meet tab,
	// then drive the prejoin/lobby gate). It is nil in production, where
	// openMeet and joinMeeting drive real CDP. Package tests substitute a fake
	// so the two parallel legs' handoff can be exercised without Chromium —
	// in particular that a SUCCESSFUL join falls through to [5/6] instead of
	// being read as a fatal leg error.
	joinRun func() error

	// presentRun and playRun stand in for the [5/6] share and [6/6] playback
	// stages so a browser-free test can drive run() all the way past the join.
	presentRun func() error
	playRun    func() error

	// Playlist queue fields
	queueMu     sync.RWMutex
	queue       []QueueItem
	queueIdx    int
	nextQueueID int
	// repeatMode is what happens when the current media ends ("off" default).
	repeatMode string
	// qualityPolicy is the active quality policy; defaultQuality is the
	// session-level one that per-item overrides temporarily replace.
	qualityPolicy  string
	defaultQuality string
	// Caption policy and requested language. captionState records what the
	// player actually reports, sampled by the watch loop. The *Default
	// fields are the session-level values per-item overrides restore to.
	captionPolicy        string
	captionLang          string
	defaultCaptionPolicy string
	defaultCaptionLang   string
	captionState         captionState
	// observedQ is the player-reported resolution/quality telemetry, sampled
	// by the watch loop.
	observedQ observedQuality
	// downloadProg tracks mid-fetch metrics for Download & Stream mode.
	downloadProg DownloadProgress
	// pv is the recorded playback-phase observation (see phase.go): the
	// honest answer to "what is happening to the media right now?", written
	// at each transition and read by every status surface. Guarded by mu.
	pv phaseState
	// Stage tracker: which [N/6] pipeline step is running and how long each
	// completed step took. Guarded by mu; see stages.go.
	stageStep    int
	stageStarted time.Time
	stageTimes   [stageCount + 1]float64
	// shuffle picks a random upcoming item when playback auto-advances.
	shuffle bool
	// autoAdvance plays the next queue item when the current media ends.
	// It is on by default: a playlist keeps playing without operator input.
	autoAdvance bool
	// rng drives shuffle picks; injectable for deterministic tests.
	rng *rand.Rand
}

// DownloadProgress reports mid-fetch metrics for Download & Stream mode.
type DownloadProgress struct {
	Active  bool
	Bytes   int64
	Total   int64
	Speed   int64
	Percent int
	// Stage is the media pipeline stage (media.StageValidate ... StageDone);
	// empty on legacy paths means "downloading".
	Stage string
	// ETASeconds is seconds remaining, -1 when unknown.
	ETASeconds int64
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
	// PlaybackMethod reports how the CURRENT media is actually being played:
	// "download" (a local file fetched by the media manager), "browser"
	// (the remote URL loaded directly in Chromium) or "" before playback
	// starts. It is the honest answer to "did the download work?" — the
	// mode says what was REQUESTED, the method says what happened. When a
	// Download-mode session falls back to browser playback after an
	// extraction failure, PlayMode still reads "download" while
	// PlaybackMethod reads "browser", so the card can never mistake a
	// fallback for a successful download.
	PlaybackMethod string
	// Phase is the observed playback phase (see phase.go): the user-facing
	// answer to what is happening to the media right now — downloading,
	// downloaded, browser (requested), fallback (download failed, browser
	// is the substitute), failed, network_unavailable or recovering. It is
	// recorded at the moment of each transition, never derived, so it can
	// never disagree with the event that caused it.
	Phase string `json:"phase,omitempty"`
	// PhaseUserReason is the actionable, user-facing sentence recorded with
	// the phase. It never contains infrastructure detail or raw error text.
	PhaseUserReason string `json:"phase_user_reason,omitempty"`
	// PhaseDetailReason is the operator diagnostic for the same transition.
	// Rendered only by operator surfaces (logs, management console) — never
	// to end users. It is excluded from JSON on purpose.
	PhaseDetailReason string `json:"-"`
	// PhaseFailedMethod records which transport failed for the failed/fallback
	// phases ("download" | "browser"); empty elsewhere.
	PhaseFailedMethod string `json:"phase_failed_method,omitempty"`
	// PhaseHistory is the bounded, ordered story of the media's playback
	// phases (oldest first). It exists so a terminal failure cannot hide the
	// transition that came before it: "download failed, fell back to the
	// browser, the browser could not start either" reads as three entries,
	// not as only the last one. Diagnostics in each entry are operator-only.
	PhaseHistory []PhaseTransition `json:"phase_history,omitempty"`

	// LocalFile reports the downloaded media backing the session, if any.
	LocalFile localFileStatus
	// DownloadProgress reports real-time metrics while media is being fetched.
	DownloadProgress DownloadProgress
	// Stages is the [1/6]..[6/6] pipeline timeline (nil before the first step).
	Stages []StageTiming
	// Queue fields
	QueueLength int
	QueueIndex  int
	HasNext     bool
	HasPrevious bool
	// RepeatMode is the end-of-media behaviour ("off", "one", "all").
	RepeatMode string
	// QualityPolicy is the active requested policy.
	QualityPolicy string
	// Observed is what the live player reports (videoWidth/Height and
	// YouTube's own quality label when present). Zero values mean the
	// player has not reported a resolution.
	Observed observedQuality
	// SelectedFormat describes what the extraction pipeline chose for the
	// currently loaded file (download mode only; browser mode has no ladder).
	SelectedFormat string
	// Caption telemetry: requested policy + the states the player reports.
	CaptionPolicy string
	CaptionLang   string
	Caption       captionState
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
		ID:               s.ID,
		State:            s.state,
		Detail:           s.detail,
		Error:            s.errMsg,
		Title:            s.title,
		MeetURL:          s.MeetURL,
		MediaURL:         s.MediaURL,
		Paused:           s.paused,
		Muted:            s.muted,
		Volume:           s.volume,
		Rate:             s.rate,
		Cur:              s.cur,
		Dur:              s.dur,
		Created:          s.created,
		Joined:           s.joined,
		Ended:            s.ended,
		AudioLang:        s.AudioLang,
		AudioTrack:       s.audioRep.SelectedTrackLabel(),
		FallbackReason:   s.audioRep.FallbackReason,
		PlayMode:         string(s.playMode),
		DownloadProgress: s.downloadProg,
	}
	s.mu.RUnlock()

	s.queueMu.RLock()
	st.QueueLength = len(s.queue)
	st.QueueIndex = s.queueIdx
	st.RepeatMode = NormalizeRepeatMode(s.repeatMode)
	st.QualityPolicy = NormalizeQualityPolicy(s.qualityPolicy)
	st.Shuffle = s.shuffle
	st.AutoAdvance = s.autoAdvance
	st.Observed = s.observedQ
	// captionPolicy/captionLang/qualityPolicy are written under queueMu
	// (applyItemCaptionLocked/applyItemQualityLocked from Next/Previous),
	// so they must be read inside the same critical section — reading them
	// after the unlock above raced those writes (race detector: queue_test
	// TestSessionQueueConcurrency).
	st.CaptionPolicy = NormalizeCaptionPolicy(s.captionPolicy)
	st.CaptionLang = s.captionLang
	// One s.mu read hold for the whole phase/local-file section. Two entered
	// reads (the historical shape) leaked a hold: RWMutex is not reentrant, so
	// the first RLock was never paired with an RUnlock and every Snapshot
	// permanently incremented the reader count — a writer could then never
	// acquire the write lock and the whole Session froze.
	s.mu.RLock()
	if s.localFile != nil {
		st.SelectedFormat = s.localFile.selected.Describe()
		st.PlaybackMethod = "download"
	} else if s.playMode == mediateam.ModeDownload || s.playMode == mediateam.ModeAuto {
		// A Download/Auto session with no local file is playing the remote URL
		// directly — i.e. it fell back to the browser after the download
		// failed, or the download is still pending. The method must say
		// "browser" either way: there is no local file.
		st.PlaybackMethod = "browser"
	}
	// The observed phase is a recorded event, not a derivation. It wins over
	// the implied method whenever the two could disagree: a fallback records
	// itself as a fallback, a download in flight records "downloading", and
	// only a completed local playback records "downloaded".
	pv := s.phaseViewOfLocked()
	hist := s.phaseHistoryLocked()
	s.mu.RUnlock()
	st.Phase = pv.Phase
	st.PhaseUserReason = pv.UserReason
	st.PhaseDetailReason = pv.Detail
	// The failed-method field rides along so surfaces can say what a fallback
	// recovered from; it is empty outside failed/fallback phases.
	if pv.FailedMethod != "" {
		st.PhaseFailedMethod = pv.FailedMethod
	}
	// The history is the whole story: a terminal phase must not erase the
	// transitions that led to it.
	if len(hist) > 0 {
		st.PhaseHistory = make([]PhaseTransition, len(hist))
		for i, e := range hist {
			st.PhaseHistory[i] = PhaseTransition{
				Phase: e.Phase, UserReason: e.UserReason,
				DetailReason: e.DetailReason, FailedMethod: e.FailedMethod,
			}
		}
	}
	// Honesty contract: a browser fallback must never render as a successful
	// local download. The phase is observation, the derived method is
	// inference — whenever the observed phase is a fallback (or an in-flight
	// download that has not produced a file yet), the method must say what
	// is ACTUALLY playing: the browser.
	switch PlaybackPhase(pv.Phase) {
	case PhaseFallback, PhaseDownloading:
		if st.PlaybackMethod == PlaybackMethodDownload {
			st.PlaybackMethod = PlaybackMethodBrowser
		}
	case PhaseDownloaded:
		st.PlaybackMethod = PlaybackMethodDownload
	}
	s.queueMu.RUnlock()
	st.Stages = s.StageTimings()
	st.HasNext = st.QueueIndex >= 0 && st.QueueIndex+1 < st.QueueLength
	st.HasPrevious = st.QueueIndex > 0

	st.LocalFile = s.localFileStatus()
	st.Caption = s.captionState
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

// Stop terminates the session: every operation it owns is cancelled
// along the single authoritative path (the session lifespan context)
// and the resources it held are released.
//
// Stop is idempotent and race-safe. The first call records an
// operator-visible "stopping" detail state so the card never claims
// the session is still live while it is tearing down; every later
// call is a cheap no-op, so two simultaneous Stop requests (a double
// tap on the button, a Telegram press racing the dashboard) can never
// race the session into an inconsistent state.
//
// Stop is session-isolated by construction: cancelling this session's
// context cannot touch another session, whose workers derive from
// ITS OWN lifespan. Stop never blocks indefinitely: the verification
// wait lives in Manager.Stop (bounded by stopGracePeriod), and
// cancelLifespan's own wait is bounded per worker class.
//
// After Stop, the session's done channel closes once cleanup() has
// released the pages, the leases and the temp files — that is the
// STOPPED confirmation every entry point must wait for before
// reporting success to the user.
func (s *Session) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		if s.state != StateError && !IsTerminalState(s.state) {
			s.detail = "stopping"
		}
		s.mu.Unlock()
		// Sweep the partial files a cancelled download would otherwise
		// leave behind, then cancel the lifespan. The sweep is
		// best-effort and never blocks the cancellation itself.
		s.stopOwnedDownload()
		s.cancelLifespan()
		// Guarantee the termination signal no matter who called Stop.
		// cleanup() — the deferred release in run() — is what normally
		// closes done, but a session stopped before its goroutine ever
		// got there (Stop immediately after creation, Stop while the
		// download stage was still holding it) would otherwise wait for
		// a cleanup that run()'s error paths skip: several of run()'s
		// early-return branches return BEFORE the deferred cleanup is
		// reachable, and a hand-built session may have no run() at all.
		// doneOnce makes this a no-op when cleanup() already ran, so
		// the two paths can never double-close.
		s.markStopped()
	})
}

// done is closed when the session has fully cleaned up.
func (s *Session) Done() <-chan struct{} { return s.done }

// setState records a new state and emits a state event.
func (s *Session) setState(state, detail string) {
	s.mu.Lock()
	changed := s.state != state || s.detail != detail
	s.state = state
	if detail != "" {
		s.detail = detail
		s.trackStageLocked(stageStepOf(detail))
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
	s.trackStageLocked(stageStepOf(detail))
	s.mu.Unlock()
}

func (s *Session) setError(err error) {
	s.mu.Lock()
	s.errMsg = err.Error()
	s.state = StateError
	s.detail = err.Error()
	s.ended = time.Now()
	s.closeStageLocked(time.Now())
	s.mu.Unlock()
	s.log.Errorf("session %s failed: %v", s.ID, err)
	s.mgr.emit(Event{Session: s.ID, Kind: EventError, Text: err.Error()})
}

// failWithState records a terminal error under a specific state. It produces
// exactly the surface setError does (message, log entry, EventError) without
// forcing the generic StateError, so classified failures such as join
// timeouts keep their own terminal state.
func (s *Session) failWithState(state string, err error) {
	s.mu.Lock()
	s.errMsg = err.Error()
	s.state = state
	s.detail = err.Error()
	s.ended = time.Now()
	s.closeStageLocked(time.Now())
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

// bindPageCtx returns a page whose every CDP call is bound to the session's
// lifetime. rod's Page.Context returns a bound copy of the page, so a stop
// issued while the page is inside a Navigate/Eval cancels that call through
// the context instead of blocking until the page's own Timeout expires.
//
// Without this, s.cancel() was a no-op for every rod call in openMedia,
// openMeet and prepareMedia: a stop mid-join blocked in the dashboard HTTP
// handler for its full 15 s budget and then reported failure even though the
// session died a moment later. The probe files (mediaprobe.go:173,
// watchprobe.go:88) already use this pattern; the session pages are the ones
// that lacked it.
//
// A page that is not a real rod page (the codebase's hand-built sessions use
// a bare &rod.Page{} as an "open but never evaluated" sentinel, which has no
// helpersLock and panics inside rod's Context) is returned unchanged, as is
// a nil page or a session without a context.
func (s *Session) bindPageCtx(p *rod.Page) *rod.Page {
	if p == nil || !s.pageBindable(p) {
		return p
	}
	s.mu.RLock()
	ctx := s.ctx
	s.mu.RUnlock()
	if ctx == nil {
		return p
	}
	return p.Context(ctx) // rod returns a bound copy; the original is untouched
}

// pageBindable reports whether p is a real rod page that Context() can be
// called on. The minimal check is the helpersLock rod itself takes in
// Page.Context: a bare zero-value Page (the hand-built sentinel) has none.
func (s *Session) pageBindable(p *rod.Page) bool {
	// A zero-value Page cannot be bound: rod's Context dereferences its
	// helpersLock. Detect it by the same field rod depends on, via the page's
	// String() representation (a real page renders "<page:id>", the zero
	// value renders "<page:>").
	return p.String() != "<page:>"
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
//
// One panic anywhere in the engine goroutine must not silently kill the
// session: the deferred cleanup() still releases the browser, the pages and
// the leases, but without a recovery the panic would be swallowed by the
// goroutine, the session would stay in whatever non-terminal state it was
// in, its meeting's dedup slot would stay occupied, and nothing would ever
// tell the operator, the bot card or the dashboard what happened. The
// recovery turns the panic into an ordinary terminal error (the engine's
// own event and log surfaces carry the digest), so the meeting can be
// restarted and the diagnostics are discoverable instead of vanishing.
func (s *Session) run() {
	defer s.cleanup()
	// recoveredByLeg reports whether a parallel leg (below) already turned a
	// panic into an error on its channel. The run-level recovery stays armed
	// for every sequential step ([1/6], the joins after the legs, [5/6],
	// [6/6], watch); a panic inside a LEG is handled by the leg's own recover,
	// and double-handling it here would overwrite the leg's error with a
	// "session crashed" message and, when the cancel raced it, downgrade the
	// session to cancelled — hiding the crash the leg already reported.
	recoveredByLeg := false
	defer func() {
		if r := recover(); r != nil {
			if !recoveredByLeg {
				err := fmt.Errorf("session crashed: %v", r)
				if s.ctx != nil && s.ctx.Err() != nil {
					// A panic during cancellation is usually the cancellation
					// itself racing a rod call; report it as cancelled, the
					// honest terminal state for a stop in flight.
					s.setError(errors.New("session stopped during teardown"))
				} else {
					s.setError(err)
				}
				// The stack digest goes to the log; stderr still gets the full
				// trace from the runtime's own panic reporting path.
				s.log.Errorf("[PIPELINE:session] session %s: %v\n%s", s.ID, r, debug.Stack())
			}
		}
	}()
	s.setState(StateStarting, "[1/6] Launching Chromium")
	s.log.Infof("[PIPELINE:session] [STEP 1/6] session %s: starting Chromium browser...", s.ID)

	// Prefer a pre-warmed browser (WarmUp enabled): same isolation guarantees
	// (fresh profile dir), skips the cold process start. On any warm-path
	// failure fall back to a cold launch.
	if s.acquireRun != nil {
		// Test seam: the fake stands in for browser acquisition, so run()'s
		// state machine, error handling and panic recovery can be exercised
		// without launching Chromium.
		release, err := s.acquireRun()
		if err != nil {
			if s.ctx.Err() == nil {
				s.setError(fmt.Errorf("start chromium: %w", err))
			}
			return
		}
		defer release()
	} else if b, release, ok := s.mgr.acquireWarmBrowser(); ok {
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

	// ---- [2/6]+[3/6]+[4/6]: media leg and meet leg, in parallel -----------------
	//
	// The user's contract: "browser open + Meet join must overlap the download"
	// (باید باز شدن مرورگر و تلاش برای ورود همزمان با دانلود اتفاق بیوفته).
	// Both legs are pure CDP on DIFFERENT tabs and share nothing but the
	// s.mu-guarded fields, so they can run concurrently: the meet leg opens the
	// Meet tab and drives the prejoin/lobby gate while the media leg is still
	// fetching the file or loading the media tab.
	//
	// Error semantics are preserved exactly: each leg returns the error it
	// observed and handles it here (after the join) exactly as the sequential
	// version did, including the download-fallback phase bookkeeping and the
	// timed-out classification. On the FIRST fatal error from either leg the
	// session lifespan is cancelled so the other leg unwinds promptly instead
	// of hanging in a CDP call; both legs observe s.ctx through bindPageCtx and
	// the explicit checks below.
	//
	// Concurrency safety: setState/setDetail/setError/setPhase all take s.mu
	// internally, and each leg writes only its own page field (s.mediaPage /
	// s.meetPage) under s.mu, so no new shared mutable state exists. The
	// players.playerURL loopback server is process-global and mutex-guarded
	// inside playerServer, which both legs may register documents on when
	// playing local/direct media — it is safe to call concurrently.
	s.log.Infof("[PIPELINE:session] session %s: media and meet legs spawning concurrently", s.ID)

	mediaErrCh := make(chan error, 1)
	meetErrCh := make(chan error, 1)
	// meetQuiet is closed when the meet leg has finished calling setState —
	// i.e. it has either reported its error or observed the cancelled lifespan
	// at the gate between openMeet and joinMeeting. The media leg's fatal
	// branch waits on it before recording the terminal state, so a straggler
	// setState from the meet goroutine can never overwrite StateError (the
	// state machine stays single-writer even across the two parallel legs).
	meetQuiet := make(chan struct{})
	var meetQuietOnce sync.Once
	quietMeet := func() { meetQuietOnce.Do(func() { close(meetQuiet) }) }

	// The meet leg: [3/6] open the Meet tab, then [4/6] join the meeting.
	// These two are pure CDP on the meet tab and do not depend on the media
	// tab or the download, so they run in their own goroutine.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// Convert the panic into an error so run()'s panic contract
				// still holds: the leg reports a terminal state instead of
				// vanishing. recoveredByLeg tells run()'s own deferred
				// recovery the crash was already handled, so it does not
				// overwrite this error (or downgrade the session to
				// cancelled when the cancel raced the panic).
				s.mu.Lock()
				recoveredByLeg = true
				s.mu.Unlock()
				quietMeet()
				meetErrCh <- fmt.Errorf("meet leg crashed: %v", r)
			}
		}()
		s.setState(StateJoining, "[3/6] Opening Google Meet")
		s.log.Infof("[PIPELINE:session] [STEP 3/6] session %s: opening Meet tab (url=%s)...", s.ID, s.MeetURL)
		if s.joinRun != nil {
			// Test seam: the fake covers BOTH meet stages (open + join) so the
			// leg's success/failure handoff is exercisable without Chromium.
			quietMeet()
			meetErrCh <- s.joinRun()
			return
		}
		if err := s.openMeet(); err != nil {
			quietMeet()
			meetErrCh <- err
			return
		}
		// A media-leg failure cancels the lifespan; do not start the join on a
		// dead session — the observation point between the two meet stages.
		if s.ctx.Err() != nil {
			quietMeet()
			meetErrCh <- s.ctx.Err()
			return
		}
		s.log.Infof("[PIPELINE:session] [STEP 3/6] session %s: Meet tab loaded", s.ID)

		s.setState(StateJoining, "[4/6] Joining meeting")
		s.log.Infof("[PIPELINE:session] [STEP 4/6] session %s: handling prejoin and host gate...", s.ID)
		// joinMeeting owns the last stretch of setState calls. Quiet the meet
		// leg only AFTER it returns: a reader of meetErrCh then knows every
		// setState from this leg has already happened, so the terminal state
		// it records can never be overwritten by a straggler poll from this
		// loop. Quieting before the call made that guarantee void — the poll
		// loop kept calling setState for the whole join, and a recorded
		// terminal "error" was observed to regress to "waiting_for_admission"
		// and finally degrade to "cancelled" via markStopped.
		joinErr := s.joinMeeting()
		quietMeet()
		meetErrCh <- joinErr
	}()

	// The media leg: the Download & Stream preparation (BLOCKING), then [2/6]
	// open the media tab. prepareLocalMedia's phase recording and fallback
	// handling are kept EXACTLY as is; the [2/6] openMedia call honours the
	// openRun test seam.
	func() {
		defer func() {
			if r := recover(); r != nil {
				// Same contract as the meet leg: report the crash as an error
				// and keep run()'s recovery from double-handling it.
				s.mu.Lock()
				recoveredByLeg = true
				s.mu.Unlock()
				mediaErrCh <- fmt.Errorf("media leg crashed: %v", r)
			}
		}()
		// Download & Stream: fetch the media to disk BEFORE the tab is opened so
		// the media tab loads a local, always-available file. The download honours
		// its own size/time/concurrency limits and starts the expiry clock on
		// completion. If download extraction fails (e.g. YouTube bot-blocks yt-dlp
		// or drops TLS connections on datacenter/proxy egress), fall back to
		// direct browser playback in Chromium rather than killing the live session.
		// Chromium in the media tab plays YouTube and web links natively.
		if mode := s.playMode; mode == mediateam.ModeDownload || mode == mediateam.ModeAuto {
			// The phase records the download intent BEFORE the fetch runs, so a
			// card rendered while it is in flight says "downloading" instead of
			// silently implying a finished file.
			if mediateam.IsDownloadable(string(s.MediaKind)) {
				s.setPhase(PhaseDownloading, "", "download stage starting")
			}
			prepareErr := s.prepareLocalMedia()
			if s.prepareRun != nil {
				// Test seam: the fake stands in for the whole preparation, so
				// run()'s handling of a failing or panicking download stage can
				// be exercised without Chromium or a download toolchain.
				if perr := s.prepareRun(); perr != nil && prepareErr == nil {
					prepareErr = perr
				}
			}
			if prepareErr != nil {
				// Classify once: the user message is what the card shows, the raw
				// error is what the operator log keeps. A network-shaped failure
				// is recorded as such so the user sees "network unavailable"
				// rather than a generic failure.
				uerr := mediateam.ClassifyUserError(prepareErr)
				userMsg := fallbackUserMessage(uerr)
				detail := prepareErr.Error()
				code := ""
				if uerr != nil {
					code = uerr.Code
				}
				phase := PhaseFallback
				if mediateam.IsUserCode(prepareErr, mediateam.UserCodeNetwork) {
					phase = PhaseNetworkUnavailable
				}
				s.log.Warnf("[PIPELINE:session] session %s: download extraction failed (%v), falling back to browser playback", s.ID, prepareErr)
				// The detail line is the user-facing diagnostic for the fallback.
				// It names the failure honestly and states the outcome, so an
				// operator can tell "the download failed and we recovered" apart
				// from "the download worked". Never claim success here.
				s.setDetail("download failed, playing via browser: " + truncateStr(prepareErr.Error(), 80))
				s.setPhaseFailedWith("download", phase, code, userMsg, detail)
				s.emit(EventState, "download unavailable, streaming via browser")
			}
		}

		s.setState(StateJoining, "[2/6] Loading media")
		s.log.Infof("[PIPELINE:session] [STEP 2/6] session %s: opening media tab (url=%s kind=%s)...", s.ID, s.MediaURL, s.MediaKind)
		// A session that never entered the download stage plays the remote URL
		// directly — record the honest phase ("browser", not a fallback) so the
		// card can distinguish "played from the browser as requested" from
		// "download failed, fell back to the browser".
		if s.Phase() == PhaseNone || s.Phase() == PhaseDownloading {
			if s.playMode == mediateam.ModeBrowser {
				s.setPhase(PhaseBrowser, "", "browser mode: playing the remote URL directly")
			} else if s.localFile == nil {
				// Download/Auto whose download was skipped (unsupported kind) or
				// already failed: whatever plays is the browser, and the phase
				// must not imply a download that never happened.
				s.setPhase(PhaseBrowser, "", "download not applicable; playing the remote URL directly")
			}
		}
		if s.openRun != nil {
			// Test seam: the fake stands in for the whole media-tab open, so the
			// fallback path runs past stage 2 without Chromium.
			mediaErrCh <- s.openRun()
			return
		}
		mediaErrCh <- s.openMedia()
	}()

	// Wait for both legs. The media leg's error handling is the big block at
	// [2/6]: it is fatal EXCEPT when the download already recorded a fallback
	// the browser leg could still recover from — preserved here exactly.
	var err error
	// meetErr/metDone record the meet leg's verdict so the [2/6] block below
	// can join it without reading a channel that already delivered (a second
	// receive on meetErrCh would block forever after the meet leg reported
	// first).
	var meetErr error
	meetDone := false
	// mediaFirst records which leg settled first: only a media error that
	// arrives while the meet leg is still in flight must wait out the join
	// verdict (the meet-first paths below already hold their own verdict).
	mediaFirst := false
	select {
	case err = <-mediaErrCh:
		mediaFirst = true
		// The media leg settled first; the meet leg keeps running for now and
		// is joined (or drained) below.
	case meetErr = <-meetErrCh:
		meetDone = true
		// The meet leg reported. A nil error is SUCCESS — the bot is in the
		// meeting — and must fall through to [5/6], not be treated as a
		// failure. This branch used to abort on every value the channel
		// delivered, so a perfectly successful join cancelled the session
		// mid-media-load: the media tab's buffered-video wait was killed by
		// the lifespan cancel and the session died reporting
		// "video element never buffered within 35s" for a video that had
		// simply not finished loading, or with a nil-pointer panic in
		// handleMeetLegError(nil). See the nil guard there.
		if meetErr == nil {
			break
		}
		// Prefer an already-settled media verdict: when both legs failed
		// essentially together, the media story (download → fallback →
		// failure) is the honest terminal phase and its classification block
		// below owns the recorded verdict — the meet error is kept in the log.
		// When the media leg is still in flight its verdict is moot, and the
		// MEET error is what the session reports, exactly as before (a meet
		// failure such as an invalid meeting code must not wait out the media
		// leg to be recorded).
		select {
		case err = <-mediaErrCh:
			s.log.Warnf("[PIPELINE:session] session %s: meet leg also failed (%v); recording the media verdict", s.ID, meetErr)
			// Fall through to the media classification block below. The meet
			// verdict was already consumed here, so the block's drain must
			// not read meetErrCh again (it would block for its full timeout).
		default:
			// First fatal error from the meet leg ([3/6] or [4/6]): abort the
			// session so the media leg unwinds instead of loading a tab for a
			// meeting that is already lost. Wait for the meet leg to go quiet
			// first — after handleMeetLegError records the terminal state a
			// straggler setState from the still-unwinding meet goroutine could
			// overwrite it, exactly the race the media branch guards against.
			select {
			case <-meetQuiet:
			case <-time.After(5 * time.Second):
				s.log.Warnf("[PIPELINE:session] session %s: meet leg still running after its own failure; recording the terminal state anyway", s.ID)
			}
			if s.cancel != nil {
				s.cancel()
			}
			// Drain the media leg so it cannot outlive the session goroutine:
			// it observes the cancelled lifespan and returns promptly. Its
			// (now moot) error is dropped on purpose — the MEET error is what
			// the session must report, exactly as the sequential version did.
			select {
			case <-mediaErrCh:
			case <-time.After(5 * time.Second):
			}
			s.handleMeetLegError(meetErr)
			return
		}
	}
	// The media leg settled first with an error. A media failure must not
	// kill a join that is still in flight: YouTube bot-checks and slow
	// downloads are routine while Google's admission gate takes minutes, and
	// the previous behaviour cancelled the session mid-admission — the card
	// froze on "waiting_for_admission" while the terminal verdict degraded
	// into "cancelled". The join's own verdict decides the outcome:
	// admitted → the meeting survives with the standard media-failure
	// handling; failed → the media story stays the terminal verdict, exactly
	// as the meet-first branch prefers it when both legs fail.
	if err != nil {
		joinFailed := false
		if mediaFirst && !meetDone && s.ctx.Err() == nil {
			// Bounded wait: joinMeeting is deadline-bounded by the join
			// timeout plus the admit wait, and (after the quietMeet reorder
			// above) the verdict is sent only after the leg's last setState,
			// so consuming it here makes the terminal decision race-free.
			select {
			case meetErr = <-meetErrCh:
				meetDone = true
				joinFailed = meetErr != nil
			case <-s.ctx.Done():
				// Stopped while waiting: cleanup owns the outcome.
				return
			}
			if !joinFailed {
				// Admitted despite the media failure: the meeting stays live.
				// Record the media loss honestly, try the same single bounded
				// download fallback the after-share path uses, and fall
				// through to the share/play flow below — its [6/6] play path
				// treats a failing playback as non-fatal, so a dead media tab
				// can never end an admitted meeting again.
				s.log.Warnf("[PIPELINE:session] session %s: media failed during the join (%v); the meeting continues, media recovery follows admission", s.ID, err)
				uerr := mediateam.ClassifyUserError(err)
				user := mediateam.FriendlyUserMessage(err)
				if u := uerr; u != nil && u.Code == mediateam.UserCodeNetwork {
					user = "The server can't reach the media source right now."
				}
				if s.playMode == mediateam.ModeAuto && s.localFile == nil {
					s.setPhase(PhaseDownloading, "Downloading media after the browser failed…", "auto fallback after a media failure during the join")
					if ferr := s.fallbackToDownload(); ferr != nil {
						s.log.Warnf("[PIPELINE:session] session %s: auto fallback after media failure failed: %v", s.ID, ferr)
						s.setPhaseFailedWith("download", PhaseFailed, mediateam.UserCodeOf(ferr),
							"The media failed and the download fallback couldn't recover it. Use 🔀 Switch media to try another link — the meeting is still live.",
							"auto fallback after media failure during join: "+ferr.Error())
					}
				} else {
					s.setPhaseFailedWith("browser", PhaseFailed, mediateam.UserCodeOf(err),
						user+" Use 🔀 Switch media to try another link — the meeting is still live.",
						err.Error())
				}
			} else {
				// Both legs failed; the richer media story owns the terminal
				// verdict (symmetric with the meet-first branch below) — the
				// join error is already logged by the leg itself.
				s.log.Warnf("[PIPELINE:session] session %s: media leg failed (%v) and the join failed too (%v); recording the media verdict", s.ID, err, meetErr)
			}
		} else {
			joinFailed = true
		}
		if joinFailed {
			// Terminal media classification, shared by both both-failed
			// orderings and the dying-lifespan case.
			//
			// Do NOT erase a download-fallback recorded moments ago: when the
			// download failed AND the browser leg could not start either, the
			// user must learn both facts, in order — the download failed, the
			// browser could not recover it, the meeting ends.
			select {
			case <-meetQuiet:
			case <-time.After(5 * time.Second):
				s.log.Warnf("[PIPELINE:session] session %s: meet leg still running after media failure; recording the terminal state anyway", s.ID)
			}
			if s.ctx.Err() == nil {
				uerr := mediateam.ClassifyUserError(err)
				user := mediateam.FriendlyUserMessage(err)
				if u := uerr; u != nil && u.Code == mediateam.UserCodeNetwork {
					user = "The server can't reach the media source right now. Please check the link and try again."
				}
				prevPhase := s.Phase()
				prevReason := s.PhaseUserReason()
				if prevPhase == PhaseFallback || prevPhase == PhaseNetworkUnavailable {
					user = prevReason + " The browser playback could not start either, so the stream cannot continue."
					s.setPhaseFailedWith(s.prevPhaseFailedSummary(), PhaseFailed, mediateam.UserCodeOf(err), user, "media tab failed after download fallback: "+err.Error())
				} else {
					s.setPhaseFailedWith("browser", "", mediateam.UserCodeOf(err), user, err.Error())
				}
				s.setError(err)
			}
			// A verdict consumed by the meet-first branch above (meetDone)
			// must not be read again — the channel delivered it once.
			if !meetDone {
				select {
				case <-meetErrCh:
				case <-time.After(5 * time.Second):
				}
			}
			return
		}
	}
	if s.ctx.Err() != nil {
		// The lifespan died mid-media-leg: wait for the meet leg to go quiet
		// and finish unwinding so it cannot outlive the session goroutine.
		select {
		case <-meetQuiet:
		case <-time.After(5 * time.Second):
		}
		select {
		case <-meetErrCh:
		case <-time.After(5 * time.Second):
		}
		return
	}
	s.mu.Lock()
	mediaTitle, mediaDur := s.title, s.dur
	s.mu.Unlock()
	s.log.Infof("[PIPELINE:session] [STEP 2/6] session %s: media ready (title=%q dur=%.1fs)", s.ID, mediaTitle, mediaDur)

	// The media leg is done; the meet leg is still driving [3/6]/[4/6] on its
	// own tab — unless it already reported above, in which case its verdict is
	// in hand and receiving from the channel again would block forever.
	if !meetDone {
		meetErr = <-meetErrCh
	}
	if meetErr != nil {
		s.handleMeetLegError(meetErr)
		return
	}
	// rearmRecovery guards the remaining sequential steps ([5/6], [6/6],
	// watch) after the parallel legs' own recovers have gone out of scope.
	// It must NOT re-run when the meet leg already reported a crash
	// (recoveredByLeg): double-handling would overwrite the leg's error and,
	// when the cancel raced the crash, downgrade the session to cancelled.
	if !recoveredByLeg {
		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("session crashed: %v", r)
				if s.ctx != nil && s.ctx.Err() != nil {
					// A panic during cancellation is usually the cancellation
					// itself racing a rod call; report it as cancelled, the
					// honest terminal state for a stop in flight.
					s.setError(errors.New("session stopped during teardown"))
				} else {
					s.setError(err)
				}
				s.log.Errorf("[PIPELINE:session] session %s: %v\n%s", s.ID, r, debug.Stack())
			}
		}()
	}
	if s.ctx.Err() != nil {
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
	shareErr := func() error {
		if s.presentRun != nil {
			return s.presentRun()
		}
		return s.startPresenting()
	}()
	if shareErr != nil {
		if s.ctx.Err() == nil {
			s.setError(shareErr)
		}
		return
	}
	s.emit(EventSharing, "screen share is live")
	s.log.Infof("[PIPELINE:session] [STEP 5/6] session %s: screen share is active!", s.ID)

	s.setState(StateStreaming, "[6/6] Starting media playback")
	s.log.Infof("[PIPELINE:session] [STEP 6/6] session %s: unmuting & playing media...", s.ID)
	playErr := func() error {
		if s.playRun != nil {
			return s.playRun()
		}
		return s.playMedia()
	}()
	if playErr != nil {
		s.log.Warnf("[PIPELINE:session] [STEP 6/6] session %s: media play failed after share: %v", s.ID, playErr)
		// Auto fallback: the browser pipeline could not play the remote
		// source, so try the downloaded file — exactly once, bounded, and
		// only when nothing was already downloaded (Download mode already
		// tried, and Auto never retries a failed fallback).
		if s.playMode == mediateam.ModeAuto && s.localFile == nil {
			s.log.Infof("[PIPELINE:session] session %s: auto fallback — downloading media after browser playback failed", s.ID)
			s.setPhase(PhaseDownloading, "The browser couldn't play this link; downloading it instead…", "auto fallback after browser playback failure")
			if ferr := s.fallbackToDownload(); ferr != nil {
				s.log.Warnf("[PIPELINE:session] session %s: auto fallback download failed: %v", s.ID, ferr)
				// Both legs lost: record the honest terminal phase with a
				// user message that names what to do next. The share stays
				// up and the session stays alive and recoverable.
				uerr := mediateam.ClassifyUserError(ferr)
				user := "Playback failed and the download fallback couldn't recover it. Use 🔀 Switch media to try another link — the meeting is still live."
				if uerr != nil && uerr.Code == mediateam.UserCodeNetwork {
					user = "The network to the media source is unavailable right now. Try again in a moment, or use 🔀 Switch media — the meeting is still live."
				}
				s.setPhaseFailedWith("download", PhaseFailed, mediateam.UserCodeOf(ferr), user, "auto fallback download failed: "+ferr.Error())
			}
		} else {
			// record the browser-side play failure (terminal unless recovery
			// repairs it: recoverMedia overwrites the phase when it succeeds).
			uerr := mediateam.ClassifyUserError(playErr)
			user := "The media couldn't start playing. Use 🔀 Switch media to try another link — the meeting is still live."
			if uerr != nil && uerr.Code == mediateam.UserCodeNetwork {
				user = "The network to the media source is unavailable right now. Playback will retry automatically; the meeting is still live."
			}
			s.setPhaseFailedWith("browser", PhaseFailed, mediateam.UserCodeOf(playErr), user, playErr.Error())
		}
		s.setDetail("[6/6] Sharing, media start retry: " + playErr.Error())
	} else {
		s.setDetail("[6/6] Streaming live")
		// Playback is verified. Keep "downloaded"/"browser" as recorded by
		// the download stage (or by the browser-mode entry above); a phase
		// that says "failed" here would contradict a healthy stream.
		if s.Phase() == PhaseFailed {
			s.setPhase(PhaseBrowser, "", "playback recovered")
		}
	}
	s.setState(StateStreaming, "[6/6] Streaming live")
	s.log.Infof("[PIPELINE:session] [STEP 6/6] session %s: stream is live (share up, media playing)", s.ID)
	s.emit(EventStreaming, "stream is live")

	s.watch()
}

// handleMeetLegError is the meet leg's error path, lifted unchanged from the
// sequential run(): the [3/6]/[4/6] failures were fatal (except for the
// cancelled case), with the joinMeeting-specific timed-out classification.
// The error passed in is the first fatal error from EITHER leg when it came
// from the meet side, or the meet leg's own error observed after the media
// leg already succeeded; either way the handler records it exactly as before.
func (s *Session) handleMeetLegError(err error) {
	// A nil verdict is success, not a failure: the meet leg sends nil after a
	// completed join, and this handler used to dereference it (nil pointer
	// panic) — masking a perfectly healthy join as a crashed session.
	if err == nil {
		return
	}
	if s.ctx.Err() != nil && errors.Is(err, context.Canceled) {
		s.setState(StateCancelled, "cancelled")
		return
	}
	if strings.Contains(err.Error(), "timed out") {
		// Record the timeout under its own terminal state. setError forces
		// StateError unconditionally, which used to make every join timeout
		// surface as a generic "error" and left StateTimedOut unreachable —
		// the operator could not tell "Google's gate never lifted" apart
		// from any other crash. failWithState keeps the same surface (message,
		// log, EventError) while preserving the honest classification.
		s.failWithState(StateTimedOut, err)
		return
	}
	s.setError(err)
}

// cleanup releases pages and finalises the state.
//
// It is the single exit path for run(): every return (error, cancel, watch
// loop exit) and every panic inside the session goroutine reaches it through
// the deferred call. It therefore owns closing s.done exactly once — the
// engine's own Manager.Stop, Manager.StopAll, the dashboard's stop endpoint
// and the bot's shutdown path all wait on Done(), so a session that never
// closes it makes every stop time out at its bounded deadline and reports a
// spurious "did not stop in time" failure.
func (s *Session) cleanup() {
	// Wait for every session-owned worker to actually terminate before
	// the session reports STOPPED. The lifespan context is cancelled by
	// the time we get here (Stop) or is about to be (run() returning),
	// so a cooperative worker is already unwinding; the wait is bounded
	// so a stuck worker cannot hold the session's terminal status
	// hostage, and any that outlive the deadline are named in the log
	// instead of vanishing silently. See lifecycle.go.
	if stuck := s.cancelLifespan(); len(stuck) > 0 {
		s.log.Warnf("[PIPELINE:session] session %s: worker(s) did not terminate within %s: %v", s.ID, stopGracePeriod, stuck)
	}
	defer func() {
		// Close done exactly once, after every resource release so a caller
		// waiting on Done() observes a fully released session. The channel
		// is nil for hand-built sessions that never went through Start;
		// closing a nil channel would panic.
		s.doneOnce.Do(func() {
			if s.done != nil {
				close(s.done)
			}
		})
	}()
	media, meet := s.mediaTab(), s.meetTab()
	// Each release is independently panic-safe: a page whose CDP connection
	// is already gone (browser crashed, or a teardown racing this cleanup)
	// raises through rod instead of returning an error, and one broken page
	// must not prevent the other page, the queue leases and the done signal
	// from being released too.
	closePage := func(p *rod.Page) {
		if p == nil {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				s.log.Warnf("[PIPELINE:session] session %s: page close raised (already gone?): %v", s.ID, r)
			}
		}()
		_ = p.Close()
	}
	closePage(media)
	closePage(meet)
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
			s.observedQ = observedQuality{Width: st.VideoWidth, Height: st.VideoHeight, YtQuality: st.YtQuality}
			s.captionState = captionState{
				Source:    st.CaptionSource,
				Lang:      st.CaptionLang,
				Loaded:    st.CaptionLoaded,
				Enabled:   st.CaptionEnabled,
				Available: st.CaptionAvailable,
				Tracks:    st.CaptionTracks,
				Note:      st.CaptionNote,
			}
			s.mu.Unlock()
			if st.Error != "" {
				s.setDetail("player error: " + st.Error)
			}
			// End-of-media: HTMLMediaElement sets paused=true when playback
			// ends (the element is parked at its last frame), so gating on
			// !st.Paused suppressed the advance for every native or
			// downloaded file — auto-advance and repeat-one silently never
			// fired on the direct path, only on YouTube, whose probe forces
			// ended=true from getPlayerState()===0 while paused stays false.
			// st.Ended alone is the correct trigger; the state guard below
			// keeps the dispatch idempotent while the advance is in flight.
			if st.Ended {
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
				// Healthy again: whatever phase a previous failure recorded, a
				// verified-healthy stream is not "failed" anymore. Restore the
				// transport-truthful phase. (The phase was preserved across the
				// recovery window on purpose: a recovering stream must not
				// pretend nothing happened, and it stops recovering the moment
				// it is healthy.)
				if cur := s.Phase(); cur == PhaseRecovering || cur == PhaseFailed || cur == PhaseNetworkUnavailable {
					if s.localFile != nil {
						s.setPhase(PhaseDownloaded, "", "playback recovered, playing the local file")
					} else {
						s.setPhase(PhaseBrowser, "", "playback recovered, playing the remote URL")
					}
				}
			case recoveries >= maxMediaRecoveries:
				if !exhaustedLogged {
					exhaustedLogged = true
					s.log.Errorf("[PIPELINE:recovery] session %s: %s; %d recovery attempts already failed — share left up, run `--media-probe <link>` for the media diagnostics", s.ID, reason, recoveries)
					s.setDetail("[6/6] Media unhealthy: " + reason)
					// Recovery is exhausted: the media is not playing and will
					// not repair itself. Record the failure honestly, with a
					// user message that says the meeting survives and what the
					// user can still do.
					s.setPhaseFailedWith("browser", PhaseFailed, classifyHealthCode(reason),
						"The media stopped playing and automatic recovery couldn't fix it. The meeting is still live — use 🔀 Switch media to try another link.",
						"recovery exhausted: "+reason)
				}
			case time.Since(lastRecovery) >= 20*time.Second:
				recoveries++
				lastRecovery = time.Now()
				stalled = 0
				// A recovery attempt is in flight: record it so the card can
				// say "recovering" instead of hiding the outage, and so a
				// user who sees a frozen stream knows the engine is on it.
				// The previous phase's reason is kept in the detail so the
				// operator log retains what is being recovered from.
				prevDetail := s.PhaseDetailReason()
				s.setPhase(PhaseRecovering, "The media hiccuped — repairing playback without leaving the meeting…", "recovering: "+reason+"; was: "+prevDetail)
				if rerr := s.recoverMedia(reason); rerr != nil {
					s.log.Errorf("[PIPELINE:recovery] session %s: attempt %d failed: %v", s.ID, recoveries, rerr)
					s.setDetail("[6/6] Media recovery failed: " + rerr.Error())
					// This attempt failed but more are allowed: keep the
					// recovering phase (the engine is still trying) and give
					// the user the reason this attempt lost.
					s.setPhase(PhaseRecovering, "The media hiccuped — still trying to repair playback…", "recovery attempt "+fmt.Sprint(recoveries)+" failed: "+rerr.Error())
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
					// Out-of-meeting detection first: when the tab was kicked
					// to a landing page, re-presenting cannot ever succeed —
					// fail fast with the honest reason instead of three 45 s
					// present-flow timeouts (production incident 2026-10-27).
					if err := s.checkMeetingPresence(); err != nil {
						s.setError(err)
						return
					}
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
	// Bound every CDP call on this page to the session's lifetime: a stop
	// issued while the media tab is mid-Navigate must cancel the navigation
	// instead of blocking on the page's own timeout (mediaprobe.go:173 shows
	// the Context-bound pattern; the session pages historically had none).
	p = s.bindPageCtx(p)
	s.mu.Lock()
	s.mediaPage = p
	s.mu.Unlock()

	// The media tab faces the SAME YouTube account gate as yt-dlp: a
	// bot-checked video presents LOGIN_REQUIRED in the player, which is
	// exactly the error the extractor already told us about. Inject the
	// configured cookie jar so the watch page carries the same
	// authenticated session the downloader uses. Nothing happens when no
	// jar is configured (the overwhelmingly common case).
	if err := s.injectYouTubeCookies(p); err != nil {
		// A broken jar is an operator problem, not a reason to kill the
		// session: browser playback may still work for un-flagged videos.
		s.log.Warnf("[PIPELINE:googleauth] session %s: cookie jar not injected: %v", s.ID, err)
	}

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
		doc, err := PlayerHTML(MediaTarget{URL: target, Kind: kind}, s.playerOptsForSubtitle())
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

// injectYouTubeCookies loads the configured download.cookies_file jar and
// sets it on p. The watch page faces the same YouTube account gate as
// yt-dlp, so the tab must carry the same authenticated session the
// extractor uses. Nil cookies (no jar configured) is a no-op.
func (s *Session) injectYouTubeCookies(p *rod.Page) error {
	if s.mgr == nil {
		return nil
	}
	cookies, err := s.mgr.YouTubeCookies()
	if err != nil {
		return err
	}
	if len(cookies) == 0 {
		return nil
	}
	params := make([]*proto.NetworkCookieParam, 0, len(cookies))
	for _, c := range cookies {
		domain := c.Domain
		if domain != "" && !strings.HasPrefix(domain, ".") {
			// CDP treats a leading dot as "include subdomains"; the
			// Netscape column already said which it is, and a bare host
			// here would limit the cookie to exactly that host.
			domain = "." + domain
		}
		params = append(params, &proto.NetworkCookieParam{
			Name:   c.Name,
			Value:  c.Value,
			Domain: domain,
			Path:   c.Path,
			Secure: c.Secure,
		})
	}
	if err := (proto.NetworkSetCookies{Cookies: params}).Call(p); err != nil {
		return fmt.Errorf("set cookies: %w", err)
	}
	s.log.Infof("[PIPELINE:googleauth] session %s: injected %d YouTube session cookies into media tab", s.ID, len(params))
	return nil
}

// openMeet loads the Meet page.
func (s *Session) openMeet() error {
	p, err := newPage(s.browser, s.mgr.browserCfg)
	if err != nil {
		return fmt.Errorf("open meet tab: %w", err)
	}
	p = s.bindPageCtx(p)
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
	// Meet is a SPA: give it a beat to start rendering. The join loop's
	// event-driven observer (meetStageWait) notices the prejoin screen the
	// moment it appears, so this only needs to cover the blank page.
	time.Sleep(400 * time.Millisecond)
	return nil
}

// joinMeeting drives the prejoin screen and lobby, and waits out Google Meet's
// host-restricted access: when the meeting's opener is not inside the call,
// Meet refuses everyone ("no one can join a meeting unless invited or admitted
// by the host") — that block clears once the opener starts the meeting, so it
// is handled as a waitable state rather than a terminal error.
//
// The state messages are honest by construction: "Waiting for host to admit
// bot" is reserved for pages that actually demand admission (the knock lobby);
// an open meeting's "Join now" click reports "Joining the meeting…" instead of
// mislabeling every join as host-gated.
func (s *Session) joinMeeting() error {
	timeout := time.Duration(s.mgr.meetCfg.JoinTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)

	// Lobby and host-gate both clear only when the meeting's opener is in the
	// call; the configured admit wait (default 5 min) bounds that patience
	// instead of the plain join timeout.
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

	// Prejoin click bookkeeping. A click is confirmed the moment the page
	// leaves the prejoin stage — ANY decisive stage (meeting, lobby, blocked)
	// means Google processed the click, so the bookkeeping resets there. While
	// the page stays prejoin, the click is retried on a label-aware cadence
	// (clickRetryWait), and a page that swallows maxPrejoinClicks clicks in a
	// row is reloaded instead of being clicked further.
	clickLabel := ""
	clickAt := time.Time{}
	unlandedClicks := 0
	resetClick := func() {
		clickLabel = ""
		clickAt = time.Time{}
		unlandedClicks = 0
	}
	confirmClick := func(label string) {
		clickLabel = strings.ToLower(label)
		clickAt = time.Now()
	}

	// Meeting-entry self-check: the "meeting" stage must be corroborated by a
	// participant-grade signal (verifyInMeeting) before the bot reports
	// Admitted. A page that keeps claiming the meeting without any
	// corroboration is accepted after maxMeetingConfirmChecks — Meet's DOM
	// varies between builds, and the watch loop owns the ground truth from
	// there (its kicked-out detector probes the same signals).
	meetingConfirmed := false
	confirmChecks := 0

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
		stage, info := s.meetStageWait(joinPollWait)
		if stage != lastStage {
			lastStage = stage
			s.log.Infof("session %s: meet stage=%s %q", s.ID, stage, info)
		}
		switch stage {
		case "meeting":
			if !meetingConfirmed {
				ok, why := s.verifyInMeeting()
				switch {
				case ok:
					meetingConfirmed = true
					s.log.Infof("[PIPELINE:meet] session %s: meeting entry verified (%s)", s.ID, why)
				case confirmChecks < maxMeetingConfirmChecks:
					// The page claims the meeting but no participant-grade
					// signal agrees yet (the media transport takes a beat to
					// come up right after admission): confirm before claiming.
					confirmChecks++
					s.log.Infof("[PIPELINE:meet] session %s: meeting stage unverified (%s), confirming (%d/%d)",
						s.ID, why, confirmChecks, maxMeetingConfirmChecks)
					s.setState(StateJoining, "[4/6] Confirming meeting entry…")
					pace = 900 * time.Millisecond
				default:
					// The page keeps claiming the meeting without
					// corroboration: accept it and let the watch loop own the
					// ground truth from here on.
					meetingConfirmed = true
					s.log.Warnf("[PIPELINE:meet] session %s: meeting stage never verified (%s); accepting, the watch loop keeps checking", s.ID, why)
				}
			}
			if !meetingConfirmed {
				break
			}
			s.log.Infof("[PIPELINE:meet] session %s: joined the meeting (meeting stage detected)", s.ID)
			s.setState(StateAdmitted, "[4/6] Admitted to meeting")
			s.turnOffMeetingCamera()
			return nil
		case "lobby":
			// The knock registered: the click bookkeeping resets.
			resetClick()
			extendWaitForHost()
			s.setState(StateWaitingForAdmission, "[4/6] Waiting for host to admit bot")
			pace = lobbyPollInterval
			s.logHostWait(&lastWaitNote, "lobby", "waiting to be admitted", 0, hostWaitStarted)
		case "blocked":
			if phrase := blockedFatalPhrase(info); phrase != "" {
				return fmt.Errorf("cannot join this meeting: %s", phrase)
			}
			// Host-gated: Meet only lets people in after the meeting's opener
			// has joined. The blocked page also bounces back to Meet's home
			// screen after ~60 s, so re-request entry on a bounded cadence
			// until the gate lifts. The card keeps a stable detail string (no
			// attempt counter): the counter belongs to the log, where it costs
			// nothing, not to the dashboard text where every increment forces
			// a message edit and a state-file rewrite.
			resetClick()
			extendWaitForHost()
			s.setState(StateWaitingForAdmission, "[4/6] Meeting not open yet: waiting for host to start it")
			interval := currentReloadWait
			if time.Since(s.lastMeetLoad) >= interval {
				reloadCount++
				currentReloadWait = computeReloadInterval(reloadCount)
				s.log.Infof("[PIPELINE:meet] session %s: meeting not accepting guests, re-requesting entry (attempt %d, waited %s)",
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
				s.logHostWait(&lastWaitNote, "blocked", "meeting not accepting guests yet", reloadCount, hostWaitStarted)
			}
		case "login":
			if len(s.googleCookies) > 0 {
				return errors.New("Google account cookies were rejected or expired; please re-export and update via /account")
			}
			return errors.New("this meeting requires a signed-in Google account; TeleMeetBot joins as a guest only (add your Google account in /account)")
		case "prejoin":
			s.setState(StateJoining, "[4/6] Prejoin screen: preparing and joining")
			s.fillGuestName()
			s.mutePrejoinDevices()
			if !clickAt.IsZero() && unlandedClicks >= maxPrejoinClicks {
				// The page swallowed click after click; only a full reload can
				// move it forward.
				s.log.Infof("[PIPELINE:meet] session %s: %d join clicks never left the prejoin screen, reloading the page", s.ID, unlandedClicks)
				resetClick()
				if err := s.reopenMeet(); err != nil {
					return fmt.Errorf("reload the meeting page: %w", err)
				}
				pace = 2 * time.Second
			} else if clickAt.IsZero() || time.Since(clickAt) >= clickRetryWait(clickLabel) {
				// First click on this page, or the previous click stayed
				// unconfirmed past its label-aware budget: click again.
				if !clickAt.IsZero() {
					unlandedClicks++
				}
				clicked, label := s.clickJoin()
				if clicked {
					confirmClick(label)
					s.log.Infof("[PIPELINE:meet] session %s: clicked %q", s.ID, label)
					// An open meeting answers "Join now" within a second or
					// two; a genuine admission need moves to the knock lobby,
					// where the "Waiting for host to admit bot" message comes
					// from. Saying "waiting for host" HERE would mislabel
					// every open meeting as host-gated.
					s.setState(StateJoining, "[4/6] Joining the meeting…")
					pace = joinClickPace
				} else {
					pace = 900 * time.Millisecond
				}
			} else {
				pace = 900 * time.Millisecond
			}
		default:
			// Unknown/loading: a little patience.
		}
		if err := s.ctxSleep(pace); err != nil {
			return err
		}
	}
	if !hostWaitStarted.IsZero() {
		return fmt.Errorf("timed out waiting %s for the meeting to accept guests: Google's host gate refused every entry attempt (no one can join a meeting unless invited or admitted by the host). the meeting opener must be inside the call with the account that created the link — the bot joins automatically the moment the room opens. if the room is already open, connect a Google account via /account: Meet sometimes refuses guests that are not signed in", deadline.Sub(hostWaitStarted).Round(time.Second))
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
	// Meet is a SPA: give it a beat to start rendering the prejoin or gate
	// screen; the join loop's observer picks the rest up as it appears.
	time.Sleep(400 * time.Millisecond)
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
	presentMisses := 0
	lastDump := time.Time{}
	presentStart := time.Now()
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
					presentMisses++
					// Dump the toolbar once per burst, not on every miss: a
					// stuck loop produced 25 ~1KB dumps in one production
					// incident. Later misses in the same burst only count.
					if presentMisses == 1 || time.Since(lastDump) >= 90*time.Second {
						lastDump = time.Now()
						s.log.Warnf("[PIPELINE:present] session %s: clickPresentButton found no button (miss #%d); visible buttons: %s", s.ID, presentMisses, s.dumpToolbarButtons())
					} else {
						s.log.Warnf("[PIPELINE:present] session %s: clickPresentButton found no button (miss #%d, %ds since first)", s.ID, presentMisses, int(time.Since(presentStart).Seconds()))
					}
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
	// The player is now hydrated and playing (ensurePlayback pressed play and
	// verified advancing frames). Only now does getAvailableAudioTracks() report
	// real tracks — calling ensureAudioTrack earlier (in prepareMedia, during the
	// ad/pre-roll) finds 0 tracks and gives up, which left YouTube's default
	// auto-dubbed English track playing instead of the requested Persian original.
	if s.MediaKind == MediaYouTube {
		if atRep, atErr := ensureAudioTrack(media, s.MediaKind, s.AudioLang, s.log); atErr != nil {
			s.log.Warnf("[PIPELINE:audio-track] session %s: requested_lang=%s: %v", s.ID, s.AudioLang, atErr)
		} else {
			s.mu.Lock()
			s.audioRep = atRep
			s.mu.Unlock()
			s.log.Infof("[PIPELINE:audiotrack] session %s: %s", s.ID, atRep.describe())
		}
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
		doc, err := PlayerHTML(MediaTarget{URL: url, Kind: kind}, s.playerOptsForSubtitle())
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
	// selected is the format the extractor chose (yt-dlp sources); zero
	// when not reported (direct fetches, Telegram ingest).
	selected mediateam.SelectedFormat
	// subtitle is the fetched caption track (nil when none was requested or
	// the source had none).
	subtitle *mediateam.Subtitle
}

// localFileStatus is the card-friendly view of a downloaded file.
type localFileStatus struct {
	URL       string    `json:"url,omitempty"`
	Path      string    `json:"path,omitempty"`
	Bytes     int64     `json:"bytes,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Via       string    `json:"via,omitempty"`
	// Subtitle describes the attached caption track, when one was fetched.
	Subtitle string `json:"subtitle,omitempty"`
}

func (s *Session) localFileStatus() localFileStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.localFile == nil {
		return localFileStatus{}
	}
	lf := localFileStatus{
		URL:       s.localFile.url,
		Path:      s.localFile.path,
		Bytes:     s.localFile.bytes,
		ExpiresAt: s.localFile.expiresAt,
		Via:       s.localFile.via,
	}
	if s.localFile.subtitle != nil {
		lf.Subtitle = s.localFile.subtitle.Lang + " (vtt)"
	}
	return lf
}

// playerOptsForSubtitle returns the player options with the current media's
// caption track attached, when a subtitle artifact is served for it. The
// base options are immutable after Start; the subtitle rides per media load.
func (s *Session) playerOptsForSubtitle() PlayerOptions {
	s.mu.RLock()
	defer s.mu.RUnlock()
	opts := s.playerOpts
	if s.localFile != nil && s.localFile.subtitle != nil {
		opts.SubtitleURL = s.localFile.subtitle.URL
		opts.SubtitleLang = s.localFile.subtitle.Lang
	}
	return opts
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

// SetStopForTesting gives a directly-constructed session (bot tests build
// sessions without Manager.Start) a working Stop/Done pair: cancelling the
// context closes the done channel exactly like the real run loop's cleanup.
//
// It routes through markStopped (the same doneOnce-guarded closer the real
// Stop uses) so a test that calls BOTH Stop() and the context cancel can
// never double-close done — the exact race the guard exists for.
func (s *Session) SetStopForTesting() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.ctx, s.cancel, s.done = ctx, cancel, done
	go func() {
		<-ctx.Done()
		s.markStopped()
	}()
}

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
