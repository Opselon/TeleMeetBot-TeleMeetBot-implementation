package engine

import (
	"errors"
	"fmt"
	"time"
)

// Seek outcome reasons. They let the bot layer tell the operator exactly why a
// seek did nothing, instead of a generic "error".
const (
	seekReasonNotReady    = "player is not ready"
	seekReasonNoDuration  = "media has no duration (live or still loading)"
	seekReasonNonSeekable = "media is not seekable"
	seekReasonStaleTab    = "media tab is stale or gone"
)

// seekQuantum is the smallest delta worth scheduling a verify probe for.
const seekQuantum = 2 * time.Second

// SeekReport is the verified result of one seek call. Engine internals and the
// bot layer read the same fields, so the card can show the real position and
// the tests can assert against actual player state.
type SeekReport struct {
	// OK is true only when the seek was applied AND the reported position
	// reflects it (within tolerance, or bounded by duration).
	OK bool `json:"ok"`
	// Error is the failure reason when OK is false.
	Error string `json:"error,omitempty"`
	// Before is the currentTime read before the seek was attempted.
	Before float64 `json:"before"`
	// After is the currentTime read after the seek was applied.
	After float64 `json:"after"`
	// Duration returned by the player (0 when unknown/live).
	Duration float64 `json:"duration"`
	// Target is the absolute position the seek asked the player to reach.
	Target float64 `json:"target"`
	// Delta is the requested change in seconds.
	Delta int `json:"delta"`
	// ReadyState is the HTMLMediaElement.readyState at verify time, so "not
	// ready" failures carry the actual number.
	ReadyState int `json:"ready_state"`
	// Seekable is the HTMLMediaElement.seekable.length at verify time; 0 means
	// the player explicitly reported the media as non-seekable.
	Seekable int `json:"seekable"`
}

// clipTarget turns a relative delta into an absolute, duration-safe target.
// When the duration is unknown (0, NaN, Infinity — a live stream or a file
// that never finished loading metadata) forward seeks stay relative and
// backward seeks clamp at 0: seeking "back 30s" of a live edge is meaningful,
// seeking "+60s" past an unknown end is not. A non-finite current position is
// normalised first — a player that never loaded metadata reports 0, but a
// broken read must not become a NaN target.
func clipTarget(current float64, delta int, duration float64) float64 {
	if current != current || current < 0 { // NaN or negative
		current = 0
	}
	if delta == 0 {
		return clampTime(current, duration)
	}
	target := current + float64(delta)
	if !(duration > 0) { // NaN, <= 0 and Infinity all mean "unknown"
		if target < 0 {
			return 0
		}
		return target
	}
	if target < 0 {
		return 0
	}
	if target > duration {
		return duration
	}
	return target
}

// clampTime bounds a position to [0, duration] when the duration is known.
func clampTime(t, duration float64) float64 {
	if t < 0 {
		return 0
	}
	if duration > 0 && t > duration {
		return duration
	}
	return t
}

// readyForSeek reports whether the media element can accept a seek right now.
// readyState >= 1 (HAVE_METADATA) is the floor: below it there is no timeline
// to seek along. seekable.length == 0 means the source offers no seek range
// (a live stream); a seek there is a no-op at best and a stall at worst, so it
// is reported as a distinct failure instead.
func readyForSeek(st mediaStatus) (bool, string) {
	if st.Seekable == 0 {
		return false, seekReasonNonSeekable
	}
	if st.Ready < 1 {
		return false, seekReasonNotReady
	}
	return true, ""
}

// SeekTo moves playback to an absolute position and verifies it. It is the
// single seek primitive every relative control (Seek, the ±10/30/60s buttons)
// funnels through, so readiness checks, clamping, stale-tab handling and
// verification live in exactly one place.
//
// Verification is what makes the seek honest: the CDP round-trip returns the
// player's own currentTime afterwards, and the report only claims OK when the
// position actually moved (or sat at a duration boundary the clamping chose).
// A seek that silently did nothing is reported as a failure, not a success.
//
// A Go error means the media path itself is broken (stale tab, CDP failure).
// A player refusal (not ready, live source, blocked range) is NOT an error:
// the report carries the reason so the caller can show it to the operator.
func (s *Session) SeekTo(target float64) (SeekReport, error) {
	s.seekMu.Lock()
	defer s.seekMu.Unlock()
	return s.seekLocked(target, 0, false)
}

// SeekBy skips by delta seconds from the current position and returns the
// verified report. Delta 0 is a no-op that still reports the real position.
func (s *Session) SeekBy(delta int) (SeekReport, error) {
	s.seekMu.Lock()
	defer s.seekMu.Unlock()
	return s.seekLocked(0, delta, true)
}

// seekLocked is the shared core, called with seekMu held so rapid presses
// queue instead of interleaving their read → seek → verify round-trips.
// relative=true means delta drives (0 = stay put); relative=false means the
// absolute target drives.
func (s *Session) seekLocked(target float64, delta int, relative bool) (SeekReport, error) {
	var rep SeekReport
	rep.Delta = delta
	seekStart := time.Now()
	log := s.logger()
	// Structured seek trace: every ±10/±30/±60s press is diagnosable from
	// the logs alone — the requested delta, the landing position, whether
	// the player refused and why, and how long the round-trip took. This is
	// the record that turns the next "it crashed after I pressed +60s" report
	// into a root cause instead of a guess.
	defer func() {
		log.Infof("[PIPELINE:seek] session %s: delta=%ds target=%.1fs before=%.1fs after=%.1fs duration=%.1fs ready=%d seekable=%d ok=%t err=%q took=%s",
			s.ID, delta, rep.Target, rep.Before, rep.After, rep.Duration, rep.ReadyState, rep.Seekable, rep.OK, rep.Error, time.Since(seekStart).Round(time.Millisecond))
	}()
	if s.mediaTab() == nil && s.mediaRun == nil {
		rep.Error = seekReasonStaleTab
		return rep, errors.New(seekReasonStaleTab)
	}

	// Read the state first: a stale CDP reference must surface as an error
	// before the seek is attempted, never as a false success.
	cur, err := s.mediaAction("seekStatus", 0)
	if err != nil {
		rep.Error = seekReasonStaleTab
		return rep, fmt.Errorf("read media state: %w", err)
	}
	rep.Before = cur.CurrentTime
	rep.Duration = cur.Duration
	rep.ReadyState = cur.Ready
	rep.Seekable = cur.Seekable

	if relative && delta == 0 {
		// Explicit no-op: report the position, touch nothing. The player is
		// not even asked, so there is nothing to refuse.
		rep.Target = clampTime(cur.CurrentTime, cur.Duration)
		rep.After = cur.CurrentTime
		rep.OK = true
		return rep, nil
	}

	if ok, why := readyForSeek(cur); !ok {
		rep.Error = why
		s.log.Warnf("[PIPELINE:seek] session %s: refusing seek: %s (readyState=%d seekable=%d)",
			s.ID, why, cur.Ready, cur.Seekable)
		return rep, nil
	}

	if relative {
		target = clipTarget(cur.CurrentTime, delta, cur.Duration)
	}
	rep.Target = clampTime(target, cur.Duration)

	// The absolute position is what the player understands; the relative
	// controls convert deltas to absolutes before calling in.
	res, err := s.mediaAction("seekTo", rep.Target)
	if err != nil {
		rep.Error = truncateStr(err.Error(), 120)
		return rep, fmt.Errorf("seek to %0.1fs: %w", rep.Target, err)
	}
	rep.After = res.CurrentTime
	if res.Duration > 0 {
		rep.Duration = res.Duration
	}

	// Verify against the position the player reports, not the one we asked
	// for: a blocked seek (live edge, DRM, a still-buffering range) lands
	// somewhere else and must be reported as such.
	if !seekLanded(rep.Target, rep.After, cur.Duration) {
		rep.Error = fmt.Sprintf("seek did not reach target: asked %0.1fs, player at %0.1fs",
			rep.Target, rep.After)
		s.log.Warnf("[PIPELINE:seek] session %s: %s", s.ID, rep.Error)
		return rep, nil
	}
	rep.OK = true

	s.mu.Lock()
	s.cur = rep.After
	if rep.Duration > 0 {
		s.dur = rep.Duration
	}
	s.mu.Unlock()

	if abs := rep.Target - rep.Before; abs > 0 && time.Duration(abs)*time.Second >= seekQuantum {
		s.log.Infof("[PIPELINE:seek] session %s: %0.1fs → %0.1fs (duration %0.1fs)", s.ID, rep.Before, rep.After, rep.Duration)
	}
	return rep, nil
}

// seekLanded decides whether the reported position confirms the seek. The
// tolerance covers the player's own rounding and a frame still in flight; a
// clamp to a duration boundary is a success even though the exact target is
// unreachable.
func seekLanded(target, after, duration float64) bool {
	const tol = 1.5
	if !(duration > 0) {
		return true // unknown duration: the player's word is all we have
	}
	if after >= duration-tol && target >= duration-tol {
		return true // clamped at the end
	}
	if after <= tol && target <= tol {
		return true // clamped at the start
	}
	return after >= target-tol
}

// Seek skips by delta seconds and returns an error when the seek could not be
// applied at all. A player refusal (live source, not ready) surfaces as an
// error here for the legacy callers that only want a bool; the bot layer uses
// SeekBy to keep the reason for the toast.
func (s *Session) Seek(delta int) error {
	rep, err := s.SeekBy(delta)
	if err != nil {
		return err
	}
	if !rep.OK {
		return errors.New(rep.Error)
	}
	return nil
}

// peekMedia reads the current position/duration without touching the player.
// It is separated from SeekTo so SeekBy can compute a target in one CDP read
// instead of two seek round-trips.
func (s *Session) peekMedia() (mediaStatus, error) {
	if s.mediaTab() == nil && s.mediaRun == nil {
		return mediaStatus{}, errors.New(seekReasonStaleTab)
	}
	st, err := s.mediaAction("seekStatus", 0)
	if err != nil {
		return mediaStatus{}, fmt.Errorf("read media state: %w", err)
	}
	return st, nil
}

// SeekableDuration returns the length of the seekable range, or 0 when the
// source is live/non-seekable. The bot uses it to decide whether the seek
// buttons should be offered at all.
func (s *Session) SeekableDuration() float64 {
	st, err := s.peekMedia()
	if err != nil {
		return 0
	}
	if st.Seekable == 0 {
		return 0
	}
	return st.Duration
}
