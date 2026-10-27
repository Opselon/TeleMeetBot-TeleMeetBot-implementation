package engine

// Video-quality policies on the session.
//
// Three distinct notions exist and are never conflated:
//
//   - Requested: the active quality policy (user default, session override
//     or per-queue-item override, in that precedence).
//   - Selected: what the extraction pipeline actually chose (yt-dlp reports
//     it; browser playback has no ladder and reports nothing).
//   - Observed: what the player itself reports while playing (the media
//     element's videoWidth/videoHeight, plus YouTube's own quality label
//     when the page exposes it). This is real telemetry, read from the live
//     player, not a restatement of the selection.
//
// Direct YouTube honesty: the IFrame player API cannot reliably force an
// exact resolution, so in browser mode a fixed policy is recorded and
// applied to extraction-backed paths (download mode), while the dashboard
// states that YouTube controls adaptive quality for direct playback. The
// engine never claims a fixed resolution was enforced when it was not.

import (
	"fmt"
)

// Quality policies (canonical strings, shared with internal/media).
const (
	QualityAuto     = "auto"
	QualitySaver    = "saver"
	QualityBalanced = "balanced"
	QualityHD720    = "hd720"
	QualityFHD1080  = "fhd1080"
	QualityBest     = "best"
)

// NormalizeQualityPolicy validates a policy, falling back to auto. It mirrors
// internal/media.NormalizeQualityPolicy; duplicated deliberately to keep the
// engine importable without the media package in the same graph.
func NormalizeQualityPolicy(s string) string {
	switch s {
	case QualitySaver, QualityBalanced, QualityHD720, QualityFHD1080, QualityBest:
		return s
	default:
		return QualityAuto
	}
}

// QualityPolicyDisplay is the dashboard label for a policy.
func QualityPolicyDisplay(p string) string {
	switch NormalizeQualityPolicy(p) {
	case QualitySaver:
		return "Data Saver (≤360p)"
	case QualityBalanced, QualityHD720:
		return "Balanced (≤720p)"
	case QualityFHD1080:
		return "Full HD (≤1080p)"
	case QualityBest:
		return "Best Available"
	default:
		return "Auto"
	}
}

// SetQualityPolicy changes the session's active quality policy. The next
// media load (restart, switch, queue advance) extracts with the new policy;
// an already-playing file is not re-fetched behind the user's back.
func (s *Session) SetQualityPolicy(policy string) error {
	normalized := NormalizeQualityPolicy(policy)
	s.queueMu.Lock()
	// The active policy and the session-level default move together: an
	// explicit operator change supersedes the launch-time default.
	s.qualityPolicy = normalized
	s.defaultQuality = normalized
	s.queueMu.Unlock()
	s.emit(EventState, "quality policy: "+normalized)
	return nil
}

// QualityPolicy returns the session's active policy.
func (s *Session) QualityPolicy() string {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	return s.qualityPolicy
}

// applyItemQualityLocked applies a queue item's quality override: an
// explicit per-item policy wins while that item plays; otherwise the
// session-level default is restored. Callers hold queueMu.
func (s *Session) applyItemQualityLocked(item QueueItem) {
	if q := NormalizeQualityPolicy(item.Quality); q != QualityAuto {
		s.qualityPolicy = q
	} else {
		s.qualityPolicy = s.defaultQuality
	}
}

// SetQueueItemQuality attaches a per-item quality override. An empty policy
// clears the override. It reports not-found for unknown or foreign items.
func (s *Session) SetQueueItemQuality(id string, policy string) error {
	s.queueMu.Lock()
	found := false
	for _, it := range s.queue {
		if it.ID == id {
			found = true
			break
		}
	}
	if !found {
		s.queueMu.Unlock()
		return ErrQueueItemNotFound
	}
	normalized := ""
	if p := NormalizeQualityPolicy(policy); p != QualityAuto {
		normalized = p
	}
	for i := range s.queue {
		if s.queue[i].ID == id {
			s.queue[i].Quality = normalized
		}
	}
	s.queueMu.Unlock()
	s.emit(EventState, "queue item quality: "+policy)
	return nil
}

// QueueItemQuality returns a queue item's override ("" = none).
func (s *Session) QueueItemQuality(id string) string {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	for _, it := range s.queue {
		if it.ID == id {
			return it.Quality
		}
	}
	return ""
}

// observed is a snapshot of the player-reported quality telemetry.
type observedQuality struct {
	Width     int
	Height    int
	YtQuality string // YouTube's own label ("hd720", "large", …), "" elsewhere
}

// Describe renders the observed quality for a dashboard line. It only ever
// states what the player reported; nothing here is inferred from the
// selection.
func (o observedQuality) Describe() string {
	if o.Height > 0 {
		if o.Width > 0 {
			return fmt.Sprintf("%dx%d", o.Width, o.Height)
		}
		return fmt.Sprintf("%dp", o.Height)
	}
	if o.YtQuality != "" {
		return "YouTube: " + o.YtQuality
	}
	return ""
}
