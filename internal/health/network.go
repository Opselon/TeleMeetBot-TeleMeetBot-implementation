package health

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// osStat is a seam for tests.
var osStat = os.Stat

// netDevFn / netScopeFn are seams so tests can stub the counters and the
// container detection without touching /proc.
var (
	netDevFn   = readNetDevTotals
	netScopeFn = netScope
)

// Network I/O (upload/download) for the resource cards.
//
// The rates are read from the process's own network namespace
// (/proc/self/net/dev on Linux). Inside Docker that namespace holds exactly
// the container's interfaces (eth0 + lo), so the numbers are this
// container's traffic — not the host's. On bare metal the same reader
// reports the host's interfaces and the card labels the scope honestly.
//
// Rates need two samples: the collector keeps the previous RX/TX totals and
// divides the delta by the elapsed time. The sample window follows the
// resource monitor's ~2s ticker, matching the card refresh cadence.

// NetStats is the network-I/O block of ServerResources.
type NetStats struct {
	// RxRateBps / TxRateBps are download/upload rates in bytes per second.
	RxRateBps float64
	TxRateBps float64
	// RxTotal / TxTotal are the lifetime counters of the namespace
	// (bytes) — inside a container, the container's lifetime traffic.
	RxTotal uint64
	TxTotal uint64
	// Scope is "container" or "host", so the card never overclaims.
	Scope string
	// Available is false when the platform or the read failed.
	Available bool
}

// netSampler keeps the previous /proc/net/dev sample for rate computation.
type netSampler struct {
	mu       sync.Mutex
	prevAt   time.Time
	prevRx   uint64
	prevTx   uint64
	cached   NetStats
	cachedAt time.Time
}

var globalNetSampler netSampler

// sampleNetLocked updates the collector's cached NetStats. Called on the
// resource collector's ticker (every ~2s) and on cache-miss refreshes; the
// delta window is whatever elapsed since the previous sample.
func (c *resourceCollector) sampleNetLocked(now time.Time) {
	c.cached.Net = globalNetSampler.sample(now)
}

// sample reads the current counters, computes rates against the previous
// sample, and caches the result for 1.5s (the same freshness window the
// resource collector uses).
func (n *netSampler) sample(now time.Time) NetStats {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.cachedAt.IsZero() && now.Sub(n.cachedAt) < 1500*time.Millisecond {
		return n.cached
	}

	rx, tx, ok := netDevFn()
	stats := NetStats{Scope: netScopeFn()}
	if ok {
		stats.RxTotal = rx
		stats.TxTotal = tx
		stats.Available = true
		if !n.prevAt.IsZero() {
			dt := now.Sub(n.prevAt).Seconds()
			if dt > 0.2 && rx >= n.prevRx && tx >= n.prevTx {
				stats.RxRateBps = float64(rx-n.prevRx) / dt
				stats.TxRateBps = float64(tx-n.prevTx) / dt
			}
		}
		n.prevAt = now
		n.prevRx = rx
		n.prevTx = tx
	}
	n.cached = stats
	n.cachedAt = now
	return stats
}

// netScope labels where the counters come from.
func netScope() string {
	if inContainer() {
		return "container"
	}
	return "host"
}

// inContainer reports whether the process runs inside a container. The
// marker files are the documented Docker/Podman/LXC signals.
func inContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if fileExists(marker) {
			return true
		}
	}
	return containerCgroupMarker()
}

func fileExists(path string) bool {
	// os.Stat is enough; the markers are plain files.
	_, err := osStat(path)
	return err == nil
}

// formatRate renders a bytes/sec rate compactly ("12.5 MiB/s").
func formatRate(bps float64) string {
	if bps < 0 {
		return "?"
	}
	const unit = 1024
	if bps < unit {
		return fmt.Sprintf("%.0f B/s", bps)
	}
	div, exp := float64(unit), 0
	for n := bps / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB/s", bps/div, "KMGTPE"[exp])
}

// formatTotalGiB renders a lifetime byte counter in GiB with one decimal,
// or in MiB when small.
func formatTotalGiB(b uint64) string {
	const GiB = 1 << 30
	const MiB = 1 << 20
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.1fG", float64(b)/GiB)
	case b >= MiB:
		return fmt.Sprintf("%.0fM", float64(b)/MiB)
	default:
		return fmt.Sprintf("%.0fK", float64(b)/1024)
	}
}

// formatNetLine renders the card's network line:
//
//	🌐 Net (container): ⬇️ 12.5 MiB/s · ⬆️ 0.3 MiB/s · Σ 45.2G/1.1G
//
// Σ totals are the namespace lifetime counters. Empty when unavailable.
func formatNetLine(n NetStats) string {
	if !n.Available {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🌐 Net (%s): ⬇️ %s · ⬆️ %s · Σ %s/%s",
		n.Scope, formatRate(n.RxRateBps), formatRate(n.TxRateBps),
		formatTotalGiB(n.RxTotal), formatTotalGiB(n.TxTotal))
	return b.String()
}
