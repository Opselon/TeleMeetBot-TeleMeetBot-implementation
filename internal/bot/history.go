package bot

import (
	"fmt"
	"strings"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// /history — the last completed sessions from the persistent store.
//
// A read-only companion to /sessions: the live dashboard shows what is
// running now, /history answers "what happened in the last runs" — state,
// duration, error — straight from state.json (capped at 50 records). No
// secrets are printed: only ids, titles and terminal states.

// historyView renders the /history card for the given records (newest last,
// as the store keeps them).
func historyView(records []store.Record) string {
	// Show the most recent first, capped.
	const maxShown = 5
	var rows []string
	n := len(records)
	start := n - maxShown
	if start < 0 {
		start = 0
	}
	for i := n - 1; i >= start; i-- {
		r := records[i]
		rows = append(rows, historyRow(r))
	}
	if len(rows) == 0 {
		return "📜 <b>History</b>\n\nNo sessions yet. Start one with /new."
	}
	var b strings.Builder
	b.WriteString("📜 <b>Recent sessions</b>\n\n")
	for _, row := range rows {
		b.WriteString(row)
		b.WriteString("\n")
	}
	b.WriteString("\n<i>Oldest records rotate out automatically (max 50 kept).</i>")
	return b.String()
}

func historyRow(r store.Record) string {
	var icon string
	switch {
	case strings.Contains(r.State, "completed") || strings.Contains(r.State, "ended"):
		icon = "✅"
	case strings.Contains(r.State, "error") || strings.Contains(r.State, "failed"):
		icon = "🔴"
	case strings.Contains(r.State, "cancelled"):
		icon = "⏹"
	case strings.Contains(r.State, "timed_out"):
		icon = "⏱"
	default:
		icon = "•"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s <code>%s</code> — <b>%s</b>", icon, esc(string(r.ID)), esc(stateLabel(r.State)))
	if !r.CreatedAt.IsZero() && !r.EndedAt.IsZero() && r.EndedAt.After(r.CreatedAt) {
		fmt.Fprintf(&b, " · ⏳ %s", health.FormatUptime(r.EndedAt.Sub(r.CreatedAt)))
	} else if !r.CreatedAt.IsZero() {
		fmt.Fprintf(&b, " · 🕒 %s", r.CreatedAt.Format(time.RFC3339))
	}
	if r.Error != "" {
		fmt.Fprintf(&b, "\n   ⚠️ %s", esc(truncate(r.Error, 120)))
	}
	return b.String()
}

// stateLabel maps internal state names to short human labels.
func stateLabel(state string) string {
	switch state {
	case engine.StateCompleted:
		return "completed"
	case engine.StateEnded:
		return "ended"
	case engine.StateError, engine.StateFailed:
		return "failed"
	case engine.StateCancelled:
		return "cancelled"
	case engine.StateTimedOut:
		return "timed out"
	case "":
		return "unknown"
	default:
		return state
	}
}
