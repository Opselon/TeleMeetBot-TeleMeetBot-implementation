package engine

import (
	"strings"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// PlaybackPhase — the honest, user-facing answer to "what is happening to my
// media right now?"
//
// PlayMode (browser / download / auto) is what was REQUESTED. PlaybackMethod
// (download / browser) is derived from whether a local file backs the session
// and can only ever say which transport is in use. Neither can express the
// states an end user actually cares about, and both are consequences of
// derivation rather than of observation:
//
//   - that a download is IN PROGRESS (the file does not exist yet, but one is
//     coming),
//   - that an attempted download FAILED and the session deliberately fell
//     back to the browser with a recoverable reason,
//   - that the network itself is unavailable and the pipeline is retrying or
//     recovering,
//   - that playback failed outright and the share is showing nothing.
//
// PlaybackPhase is that observation. It is written at the exact moments the
// transitions happen (prepare/fallback/recover/fail) — never recomputed from
// other fields, so it can never disagree with the event that caused it — and
// it is cleared on every new media load.
//
// The Dashboard and the Telegram card render it verbatim, so the strings the
// user sees are the enum's own documentation. The seven contract states:
//
//   - PhaseDownloading: a download is in flight (or staged and about to start)
//   - PhaseDownloaded:  a local file is the playback source right now
//   - PhaseBrowser:     the remote URL is playing directly (requested, not a fallback)
//   - PhaseFallback:    a download failed and the browser is the deliberate backup
//   - PhaseFailed:      playback failed; the share is up but the media is not playing
//   - PhaseNetworkUnavailable: the network to the source is broken / was observed broken
//   - PhaseRecovering:  the engine detected a problem and is repairing playback
//
// Never report a browser fallback as a successful local download:
// PhaseFallback is distinct from PhaseDownloaded by construction, and
// Snapshot only reports a local file when the phase is not a fallback.
type PlaybackPhase string

const (
	// PhaseNone is the absence of a recorded observation: nothing has been
	// decided about the current media yet (a session that never started, or a
	// hand-built session in tests). It is never rendered to a user.
	PhaseNone PlaybackPhase = ""
	// PhaseDownloading means a download is in flight or staged. The session is
	// fetching the media to disk before the media tab opens.
	PhaseDownloading PlaybackPhase = "downloading"
	// PhaseDownloaded means a downloaded local file is the playback source.
	PhaseDownloaded PlaybackPhase = "downloaded"
	// PhaseBrowser means the remote URL is playing directly in the media tab,
	// as requested (Browser mode, or the default when no mode was set).
	PhaseBrowser PlaybackPhase = "browser"
	// PhaseFallback means a download was requested, failed, and the browser is
	// the deliberate substitute. A fallback must never read as a download.
	PhaseFallback PlaybackPhase = "fallback"
	// PhaseFailed means playback failed and the media is not playing. The
	// share stays up; the session stays alive and recoverable.
	PhaseFailed PlaybackPhase = "failed"
	// PhaseNetworkUnavailable means the network route to the media source is
	// broken (or was just observed breaking). Retry and recovery are bounded.
	PhaseNetworkUnavailable PlaybackPhase = "network_unavailable"
	// PhaseRecovering means the engine detected unhealthy playback and is
	// repairing it without touching the screen share.
	PhaseRecovering PlaybackPhase = "recovering"
)

// phaseReason is the operator-side / user-side context carried with a phase.
// The user message is what the card and dashboard display; the detail is what
// the operator log keeps. Both are written once, at the transition.
type phaseReason struct {
	// user is the user-facing explanation (an actionable sentence, never raw
	// error text and never infrastructure detail).
	user string
	// detail is the operator-facing diagnostic that travels with the same
	// transition. It is NOT rendered to end users.
	detail string
}

// phaseState is a session's recorded playback-phase observation, guarded by
// the session mutex. It exists on the Session struct as a single field so
// every reader (Snapshot, callbacks, the watch loop) sees the same truth.
//
// lastTransition records the phases the session passed through, newest last.
// A terminal failure legitimately supersedes the phase recorded a moment
// earlier (a download fallback followed by a browser that could not start),
// but the user still must be able to see what happened first: the history is
// what lets a surface render BOTH facts instead of only the last one. It is
// bounded by phaseHistoryMax.
type phaseState struct {
	phase  PlaybackPhase
	reason phaseReason
	// failedMethod records which transport failed inside a PhaseFailed /
	// PhaseFallback ("download" or "browser"), so a fallback can report what
	// it fell back FROM without re-deriving it.
	failedMethod string
	// history is the append-only list of completed transitions, oldest first.
	history []phaseEntry
}

// phaseEntry is one recorded transition in the history.
type phaseEntry struct {
	Phase        PlaybackPhase
	UserReason   string
	DetailReason string
	FailedMethod string
}

// phaseHistoryMax bounds the per-session transition log. It exists so a
// pathological recover loop cannot grow the observation without limit; the
// interesting story is always within the most recent handful of transitions.
const phaseHistoryMax = 12

// recordPhaseLocked appends the current observation to the history before it
// is replaced. Caller must hold s.mu.
func (ps *phaseState) recordPhaseLocked() {
	if ps.phase == PhaseNone {
		return
	}
	entry := phaseEntry{Phase: ps.phase, UserReason: ps.reason.user, DetailReason: ps.reason.detail, FailedMethod: ps.failedMethod}
	ps.history = append(ps.history, entry)
	if len(ps.history) > phaseHistoryMax {
		ps.history = ps.history[len(ps.history)-phaseHistoryMax:]
	}
}

// playback phases are mutated only through setPhase, which logs the operator
// diagnostic and emits nothing to the user on its own — the user surface is
// refreshed by the card loop that reads Snapshot.

// setPhase records a playback-phase transition and the reason it happened.
// user is a user-facing sentence; detail is the operator diagnostic that
// travels with it. Either may be empty ("no reason recorded").
//
// The session logger always receives the technical detail, so nothing is lost
// for the operator: the split is strictly about who reads what.
func (s *Session) setPhase(phase PlaybackPhase, user, detail string) {
	s.mu.Lock()
	prev := s.pv.phase
	if prev != phase {
		s.pv.recordPhaseLocked()
	}
	s.pv.phase = phase
	s.pv.reason = phaseReason{user: strings.TrimSpace(user), detail: strings.TrimSpace(detail)}
	s.mu.Unlock()
	if prev != phase && s != nil && s.log != nil {
		// Operator log keeps the technical reason; the user message rides on
		// the card through Snapshot.
		if detail != "" {
			s.log.Infof("[PIPELINE:phase] session %s: playback phase %s → %s (%s)", s.ID, prev, phase, truncateForPhaseLog(detail))
		} else {
			s.log.Infof("[PIPELINE:phase] session %s: playback phase %s → %s", s.ID, prev, phase)
		}
	}
}

// setPhaseFailed records a failed transport and the reason. method is the
// transport that failed ("download" or "browser") so a later fallback can
// report what it recovered from.
func (s *Session) setPhaseFailed(method, user, detail string) {
	s.mu.Lock()
	s.pv.recordPhaseLocked()
	s.pv.phase = PhaseFailed
	s.pv.failedMethod = method
	s.pv.reason = phaseReason{user: strings.TrimSpace(user), detail: strings.TrimSpace(detail)}
	s.mu.Unlock()
	if s != nil && s.log != nil {
		s.log.Warnf("[PIPELINE:phase] session %s: playback failed via %s (%s)", s.ID, method, truncateForPhaseLog(detail))
	}
}

// setPhaseFailedWith records a failure together with the classified error
// code, choosing the phase from it: a network-shaped failure is recorded as
// network_unavailable, any other failure as failed. user is the user-facing
// sentence (already classified by the caller), detail the raw diagnostic.
// code is the media.UserError category of the underlying error ("" unknown).
func (s *Session) setPhaseFailedWith(method string, phase PlaybackPhase, code, user, detail string) {
	if phase == "" {
		if code == mediateam.UserCodeNetwork {
			phase = PhaseNetworkUnavailable
		} else {
			phase = PhaseFailed
		}
	}
	s.mu.Lock()
	s.pv.recordPhaseLocked()
	s.pv.phase = phase
	s.pv.failedMethod = method
	s.pv.reason = phaseReason{user: strings.TrimSpace(user), detail: strings.TrimSpace(detail)}
	s.mu.Unlock()
	if s != nil && s.log != nil {
		s.log.Warnf("[PIPELINE:phase] session %s: playback failed via %s (%s) classified=%s", s.ID, method, truncateForPhaseLog(detail), code)
	}
}

// fallbackUserMessage composes the card sentence for a Download mode that
// fell back to the browser. The user needs two facts, in order: the download
// did not work, and the meeting still gets the media. Everything else is
// noise; the classified reason is appended only when it is actionable.
func fallbackUserMessage(u *mediateam.UserError) string {
	base := "Download failed — playing the link directly from the browser instead."
	if u == nil {
		return base + " The meeting keeps streaming."
	}
	switch u.Code {
	case mediateam.UserCodeNetwork:
		return base + " The server couldn't reach the media source reliably, so the browser is playing it directly. The meeting keeps streaming."
	case mediateam.UserCodeBlocked:
		return base + " The media source is limiting this server right now, so the browser is playing it directly. The meeting keeps streaming."
	case mediateam.UserCodeUnsupported, mediateam.UserCodeSourceGone, mediateam.UserCodeLiveStream:
		return base + " " + u.Message
	case mediateam.UserCodeMissingTool, mediateam.UserCodeNoSpace:
		return base + " " + u.Message
	default:
		return base + " The meeting keeps streaming."
	}
}

// PhaseHistory returns the bounded, ordered list of recorded phase
// transitions (oldest first). A terminal failure legitimately supersedes the
// phase recorded a moment before it — a download fallback followed by a
// browser that could not start — and the history is what lets a surface tell
// the whole story instead of only the final entry.
func (s *Session) PhaseHistory() []phaseEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.phaseHistoryLocked()
}

// phaseHistoryLocked copies the transition history. Caller must hold s.mu.
func (s *Session) phaseHistoryLocked() []phaseEntry {
	if len(s.pv.history) == 0 {
		return nil
	}
	out := make([]phaseEntry, len(s.pv.history))
	copy(out, s.pv.history)
	return out
}

// clearPhase resets the recorded observation — used when a new media load
// begins, so a stale phase can never describe the new media. The history of
// the previous media is dropped with it: a switch is a new story.
func (s *Session) clearPhase() {
	s.mu.Lock()
	s.pv = phaseState{}
	s.mu.Unlock()
}

// prevPhaseFailedSummary returns the failed-method summary of the phase the
// session recorded before the current one, so a terminal failure can report
// what it is failing AFTER ("download+browser") without re-deriving the past.
func (s *Session) prevPhaseFailedSummary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch s.pv.phase {
	case PhaseFallback:
		return s.pv.failedMethod + "+browser"
	case PhaseNetworkUnavailable:
		if s.pv.failedMethod == "" {
			return "network+browser"
		}
		return s.pv.failedMethod + "+browser"
	}
	return "browser"
}

// classifyHealthCode maps a media-health reason string (produced by
// mediaHealth/recoverMedia: "the player is not ready", "the media tab has no
// video element", …) onto the user-error category driving the card message.
// It keeps the recovery surface honest without leaking the raw diagnostic.
func classifyHealthCode(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "no video element"), strings.Contains(r, "not ready"):
		return mediateam.UserCodeSourceGone
	case strings.Contains(r, "consent"):
		return mediateam.UserCodeInternal
	case strings.Contains(r, "gone"), strings.Contains(r, "cannot read"):
		return mediateam.UserCodeNetwork
	}
	return mediateam.UserCodeInternal
}

// PhaseTransition is one recorded playback-phase transition, exposed for
// diagnostics (history). The detail reason is operator-only and is excluded
// from JSON so an API consumer cannot accidentally render it to an end user.
type PhaseTransition struct {
	Phase        PlaybackPhase `json:"phase"`
	UserReason   string        `json:"user_reason,omitempty"`
	DetailReason string        `json:"-"`
	FailedMethod string        `json:"failed_method,omitempty"`
}

// truncateForPhaseLog bounds the operator diagnostic in the phase log line.
func truncateForPhaseLog(s string) string {
	if len(s) <= 140 {
		return s
	}
	return s[:140] + "…"
}

// Phase returns the recorded playback phase.
func (s *Session) Phase() PlaybackPhase {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pv.phase
}

// PhaseUserReason returns the user-facing sentence recorded with the current
// phase ("" when none was recorded).
func (s *Session) PhaseUserReason() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pv.reason.user
}

// PhaseDetailReason returns the operator diagnostic recorded with the current
// phase. Misuse guard: this is for logs and the management console only —
// never hand it to an end user.
func (s *Session) PhaseDetailReason() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pv.reason.detail
}

// phaseFailedMethod reports which transport failed for PhaseFailed/PhaseFallback.
func (s *Session) phaseFailedMethod() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pv.failedMethod
}

// phaseView is the read-only view embedded in Status. It carries the phase
// itself plus both reasons, so every surface (Telegram card, dashboard,
// healthz) renders the SAME recorded observation instead of re-deriving one.
type phaseView struct {
	// Phase is the machine value ("" when nothing was recorded).
	Phase string `json:"phase,omitempty"`
	// UserReason is the user-facing sentence ("" when none).
	UserReason string `json:"user_reason,omitempty"`
	// Detail is the operator diagnostic, rendered only by the management
	// console and logs. Dashboard/Templates must not echo it to end users.
	Detail string `json:"-"`
	// FailedMethod records the transport that failed for failed/fallback
	// phases ("download" | "browser").
	FailedMethod string `json:"failed_method,omitempty"`
}

// phaseViewOf snapshots the phase state for a Status. Convenience wrapper
// around phaseViewOfLocked that takes s.mu itself; the caller must NOT hold
// it (Snapshot takes the lock once for both reads).
func (s *Session) phaseViewOf() phaseView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.phaseViewOfLocked()
}

// phaseViewOfLocked returns the phase view. Caller must hold s.mu (at least
// a read hold).
func (s *Session) phaseViewOfLocked() phaseView {
	return phaseView{
		Phase:        string(s.pv.phase),
		UserReason:   s.pv.reason.user,
		Detail:       s.pv.reason.detail,
		FailedMethod: s.pv.failedMethod,
	}
}
