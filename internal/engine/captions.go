package engine

// Caption (subtitle) policy on the session.
//
// The same honesty rules as video quality apply, per medium:
//
//   - Download & Stream: a native <track> is attached to the embedded player
//     and enabled; the probe reads textTracks mode/readyState back, so
//     "enabled" is what the player reports.
//   - Direct YouTube: the documented getOption/setOption captions-module API
//     is used best-effort; a request that cannot be read back is reported as
//     "issued (unverified)".
//   - Generic pages: the source's own player controls captions; the session
//     reports "source-controlled".
//
// Precedence mirrors quality: per-item override → session override → the
// user's saved default (applied at launch via StartRequest).

import (
	"fmt"
)

// Caption policies (canonical strings; mirrors store.NormalizeCaptionPolicy).
const (
	CaptionOff       = "off"
	CaptionAuto      = "auto"
	CaptionPreferred = "preferred"
	CaptionOriginal  = "original"
)

// NormalizeCaptionPolicy validates a policy, falling back to off.
func NormalizeCaptionPolicy(s string) string {
	switch s {
	case CaptionAuto, CaptionPreferred, CaptionOriginal:
		return s
	default:
		return CaptionOff
	}
}

// CaptionPolicyDisplay is the dashboard label for a policy.
func CaptionPolicyDisplay(p string) string {
	switch NormalizeCaptionPolicy(p) {
	case CaptionAuto:
		return "Auto (preferred language when available)"
	case CaptionPreferred:
		return "Preferred Language"
	case CaptionOriginal:
		return "Original Language"
	default:
		return "Off"
	}
}

// captionState is what is actually known about captions right now. Zero
// values mean "nothing observable" — never rendered as success.
type captionState struct {
	// Source: "native" (local <track>), "youtube" (captions module),
	// "source-controlled" (generic page), "" (nothing observable).
	Source string
	// Lang is the language the player reports for the active track.
	Lang string
	// Loaded: the player reports the track's data is loaded.
	Loaded bool
	// Enabled: the player reports the track showing. A request that was
	// issued but not confirmed never sets this.
	Enabled bool
	// Available: any caption track was discovered.
	Available bool
	// Tracks lists the languages the player currently exposes.
	Tracks []string
	// Note carries the honest caveat ("issued (unverified)", "module not
	// ready", "no caption control on this page", …).
	Note string
}

// Describe renders the caption state for a dashboard line, without ever
// equating an issued request with a visible caption.
func (c captionState) Describe() string {
	if c.Source == "" {
		return ""
	}
	var sb []byte
	switch c.Source {
	case "native":
		sb = append(sb, "native <track>"...)
	case "youtube":
		sb = append(sb, "YouTube captions module"...)
	case "source-controlled":
		sb = append(sb, "source's own player"...)
	default:
		sb = append(sb, c.Source...)
	}
	if c.Enabled {
		sb = append(sb, " · ON"...)
		if c.Lang != "" {
			sb = append(sb, fmt.Sprintf(" (%s)", c.Lang)...)
		}
	} else {
		sb = append(sb, " · off"...)
	}
	if c.Note != "" {
		sb = append(sb, fmt.Sprintf(" — %s", c.Note)...)
	}
	return string(sb)
}

// SetCaptionPolicy changes the session's caption policy. The active values
// and the session-level defaults move together: an explicit operator change
// supersedes the launch-time default (per-item overrides then restore to it).
// The next media load applies the policy (download mode re-fetches the
// track; YouTube mode issues the module commands on the next probe cycle).
func (s *Session) SetCaptionPolicy(policy string) error {
	normalized := NormalizeCaptionPolicy(policy)
	s.queueMu.Lock()
	s.captionPolicy = normalized
	s.defaultCaptionPolicy = normalized
	s.queueMu.Unlock()
	s.emit(EventState, "caption policy: "+normalized)
	return nil
}

// CaptionPolicy returns the session's active caption policy.
func (s *Session) CaptionPolicy() string {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	return s.captionPolicy
}

// SetCaptionLang sets the session's requested subtitle language (BCP-47-ish
// code). Empty clears the preference. Like the policy, the active value and
// the session-level default move together.
func (s *Session) SetCaptionLang(lang string) error {
	normalized := NormalizeCaptionLang(lang)
	s.queueMu.Lock()
	s.captionLang = normalized
	s.defaultCaptionLang = normalized
	s.queueMu.Unlock()
	return nil
}

// CaptionLang returns the session's requested subtitle language.
func (s *Session) CaptionLang() string {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	return s.captionLang
}

// NormalizeCaptionLang trims/validates a language tag (same fence as the
// store: lowercase letters, digits and dashes, ≤16 runes).
func NormalizeCaptionLang(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			return ""
		}
	}
	if len(out) == 0 || len(out) > 16 {
		return ""
	}
	return string(out)
}

// SetQueueItemCaption attaches a per-item subtitle language override ("off"
// forces none; "" clears the override). Unknown items are reported.
func (s *Session) SetQueueItemCaption(id string, lang string) error {
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
	switch p := NormalizeCaptionLang(lang); {
	case lang == "off" || p == "":
		normalized = lang // "off" passes through; garbage normalized to ""
	default:
		normalized = p
	}
	if lang != "off" && normalized == "" && lang != "" {
		s.queueMu.Unlock()
		return fmt.Errorf("invalid caption language %q", lang)
	}
	for i := range s.queue {
		if s.queue[i].ID == id {
			s.queue[i].CaptionLang = normalized
		}
	}
	s.queueMu.Unlock()
	s.emit(EventState, "queue item caption: "+lang)
	return nil
}

// QueueItemCaption returns an item's caption override ("" = none).
func (s *Session) QueueItemCaption(id string) string {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	for _, it := range s.queue {
		if it.ID == id {
			return it.CaptionLang
		}
	}
	return ""
}

// applyItemCaptionLocked resolves the caption request for an item: an
// explicit per-item language wins ("off" = none for this item), otherwise
// the session-level defaults are restored. Callers hold queueMu.
func (s *Session) applyItemCaptionLocked(item QueueItem) {
	if item.CaptionLang == "off" {
		s.captionLang = ""
		s.captionPolicy = CaptionOff
		return
	}
	if item.CaptionLang != "" {
		s.captionLang = item.CaptionLang
		s.captionPolicy = CaptionPreferred
		return
	}
	s.captionPolicy = s.defaultCaptionPolicy
	s.captionLang = s.defaultCaptionLang
}

// ToggleCaptions issues an enable/disable command through the player probe
// and reports what the player confirmed. The language rides in the action
// string ("ccOn:<lang>") because the probe's arg channel is numeric. The
// probe's own read-back (sampled by the watch loop into captionState) is
// authoritative: "issued" and "confirmed" are reported distinctly.
func (s *Session) ToggleCaptions(on bool, lang string) (confirmed bool, note string) {
	action := "ccOff"
	if on {
		action = "ccOn"
		if l := NormalizeCaptionLang(lang); l != "" {
			action += ":" + l
		}
	}
	if _, err := s.mediaAction(action, 0); err != nil {
		return false, truncateStr(err.Error(), 120)
	}
	// Read the state back immediately: one extra probe, not a polling loop.
	if st, err := s.mediaAction("seekStatus", 0); err == nil {
		s.mu.Lock()
		s.captionState = captionState{
			Source:    st.CaptionSource,
			Lang:      st.CaptionLang,
			Loaded:    st.CaptionLoaded,
			Enabled:   st.CaptionEnabled,
			Available: st.CaptionAvailable,
			Tracks:    st.CaptionTracks,
			Note:      st.CaptionNote,
		}
		confirmed = s.captionState.Enabled == on
		s.mu.Unlock()
	}
	s.mu.RLock()
	note = s.captionState.Note
	enabled := s.captionState.Enabled
	s.mu.RUnlock()
	if on {
		if enabled {
			return confirmed, "captions on (player reports the track showing)"
		}
		return false, "issued — the player has not confirmed captions yet"
	}
	if !enabled {
		return confirmed, "captions off (player confirmed)"
	}
	return false, "issued — the player still reports captions on"
}

// captionLangForDownload resolves the subtitle language for the next fetch:
// "off" or empty means none; a policy of preferred/auto/original with a
// saved language uses that language; auto/original without one stays empty
// (the source's default language is not guessed).
func (s *Session) captionLangForDownload() string {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	switch NormalizeCaptionPolicy(s.captionPolicy) {
	case CaptionPreferred:
		return s.captionLang
	case CaptionAuto:
		return s.captionLang
	default:
		return ""
	}
}
