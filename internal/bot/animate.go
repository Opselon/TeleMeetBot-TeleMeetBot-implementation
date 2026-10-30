package bot

import (
	"strings"
	"sync/atomic"
)

// Card animation helpers.
//
// Telegram text is static, but the dashboard refresh loop re-renders every
// card every few seconds — so the "animation" is a frame advanced on every
// render: a braille spinner, a comet sweeping an indeterminate track, and a
// filled bar with a bright head. Frames derive from a process-wide counter
// so every refresh shows the next frame regardless of clock skew.

// spinnerFrames is the braille spinner sequence.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// moonFrames is the emoji spinner used on headline rows.
var moonFrames = []string{"🌑", "🌒", "🌓", "🌔", "🌕", "🌖", "🌗", "🌘"}

// animCounter advances once per animated card render.
var animCounter atomic.Uint64

// animFrame returns the i-th frame of the current animation step.
func animFrame(frames []string) string {
	if len(frames) == 0 {
		return ""
	}
	return frames[int(animCounter.Add(1))%len(frames)]
}

// renderAnimatedBar renders a download progress bar with a bright head
// block: ▰▰▰▰▰◆▱▱▱▱. width is the slot count.
func renderAnimatedBar(pct int, width int) string {
	if width <= 0 {
		width = 10
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := (pct * width) / 100
	if filled > width {
		filled = width
	}
	var b strings.Builder
	for i := 0; i < width; i++ {
		switch {
		case i < filled-1:
			b.WriteString("▰")
		case i == filled-1 || (filled == 0 && i == 0 && pct > 0):
			// The leading block is the "head": slightly brighter glyph.
			b.WriteString("◆")
		default:
			b.WriteString("▱")
		}
	}
	// Special case: bar just started (no full block yet) — the head sits on
	// the first slot.
	if filled == 0 && pct > 0 {
		b.Reset()
		b.WriteString("◆")
		for i := 1; i < width; i++ {
			b.WriteString("▱")
		}
	}
	return b.String()
}

// renderComet renders an indeterminate loading bar: a 3-slot bright segment
// sweeping a track of the given width.
func renderComet(width int) string {
	if width <= 0 {
		width = 10
	}
	if width < 4 {
		width = 4
	}
	pos := int(animCounter.Load()) % width
	var b strings.Builder
	for i := 0; i < width; i++ {
		switch {
		case i == pos || i == (pos+1)%width:
			b.WriteString("▰")
		case i == (pos+2)%width:
			b.WriteString("◆")
		default:
			b.WriteString("▱")
		}
	}
	return b.String()
}

// spinner returns the current braille spinner frame.
func spinner() string { return animFrame(spinnerFrames) }

// pulsingDots renders "…" that cycles through 0-3 dots on consecutive
// renders, for stage labels.
func pulsingDots() string {
	return strings.Repeat(".", int(animCounter.Add(0)+1)%4)
}
