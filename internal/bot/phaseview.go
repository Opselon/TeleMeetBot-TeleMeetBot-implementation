package bot

import (
	"fmt"
	"strings"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// Playback-phase rendering for the Telegram surfaces.
//
// The engine records an honest, observed phase for the current media (see
// internal/engine/phase.go). This file turns that machine value into the line a
// user reads on the session card, and nothing else: it holds no state, makes no
// decisions about what happened, and never renders the operator diagnostic.
//
// The split is deliberate:
//   - the MODE line says what the user asked for (browser / download / auto),
//   - the METHOD line says which transport is playing (download / browser),
//   - the PHASE line says what actually happened and what to expect next.
//
// Without the phase line a Download-mode session whose download failed and
// recovered via the browser reads exactly like a healthy download to a user
// who only looks at the mode line.

// phaseEmoji is the icon each phase renders with. Terminal/neutral phases that
// need no attention carry no icon clutter; the ones a user must notice carry
// warning glyphs.
func phaseEmoji(phase engine.PlaybackPhase) string {
	switch phase {
	case engine.PhaseDownloading:
		return "⬇️"
	case engine.PhaseDownloaded:
		return "💾"
	case engine.PhaseBrowser:
		return "🌐"
	case engine.PhaseFallback:
		return "♻️"
	case engine.PhaseFailed:
		return "🔴"
	case engine.PhaseNetworkUnavailable:
		return "📡"
	case engine.PhaseRecovering:
		return "🛠️"
	default:
		return ""
	}
}

// phaseLabel is the short, user-facing name of a phase. It reads next to the
// icon on the card.
func phaseLabel(phase engine.PlaybackPhase) string {
	switch phase {
	case engine.PhaseDownloading:
		return "Downloading"
	case engine.PhaseDownloaded:
		return "Downloaded"
	case engine.PhaseBrowser:
		return "Browser playback"
	case engine.PhaseFallback:
		return "Browser fallback"
	case engine.PhaseFailed:
		return "Playback failed"
	case engine.PhaseNetworkUnavailable:
		return "Network unavailable"
	case engine.PhaseRecovering:
		return "Recovering"
	default:
		return ""
	}
}

// playbackPhaseLine renders the phase line for one session card. It returns ""
// when there is nothing honest to say (no phase recorded, or a plain browser
// phase with no reason to flag it — a healthy browser stream is the default and
// needs no banner).
//
// The user reason is rendered when present; otherwise the label alone carries
// the message. The operator detail is NEVER rendered here.
func playbackPhaseLine(st engine.Status) string {
	phase := engine.PlaybackPhase(st.Phase)
	if phase == engine.PhaseNone {
		return ""
	}
	// A plain, healthy browser phase is the everyday case: the mode and method
	// lines already say it. Rendering a banner for it would be noise — unless
	// the engine recorded a user reason worth reading (e.g. a recovered
	// fallback's explanation).
	if phase == engine.PhaseBrowser && strings.TrimSpace(st.PhaseUserReason) == "" {
		return ""
	}
	icon := phaseEmoji(phase)
	label := phaseLabel(phase)
	reason := strings.TrimSpace(st.PhaseUserReason)
	var sb strings.Builder
	if icon != "" {
		sb.WriteString(icon + " ")
	}
	if label != "" {
		sb.WriteString("<b>" + esc(label) + "</b>")
	}
	if reason != "" {
		if sb.Len() > 0 {
			sb.WriteString(" — ")
		}
		sb.WriteString("<i>" + esc(reason) + "</i>")
	}
	if sb.Len() == 0 {
		return ""
	}
	return sb.String() + "\n"
}

// phaseNoticeText renders the phase as a standalone notice (the message sent
// alongside the card when a transition deserves the user's attention: a
// fallback that just happened, a recovery that succeeded). It returns "" when
// the phase does not warrant a separate message, which is the default — the
// card already shows the line, and duplicate notifications are noise.
func phaseNoticeText(st engine.Status) string {
	phase := engine.PlaybackPhase(st.Phase)
	reason := strings.TrimSpace(st.PhaseUserReason)
	switch phase {
	case engine.PhaseFallback:
		if reason == "" {
			reason = "The download failed, so the link is playing directly from the browser. The meeting keeps streaming."
		}
	case engine.PhaseNetworkUnavailable:
		if reason == "" {
			reason = "The server can't reach the media source right now. Playback will retry automatically; the meeting keeps streaming."
		}
	case engine.PhaseFailed:
		if reason == "" {
			reason = "Playback failed. Use 🔀 Switch media to try another link — the meeting is still live."
		}
	default:
		// Downloading/downloaded/browser/recovering are progress, not news:
		// the card renders them without a separate notification.
		return ""
	}
	return fmt.Sprintf("%s <b>%s</b>\n\n<i>%s</i>", phaseEmoji(phase), esc(phaseLabel(phase)), esc(reason))
}
