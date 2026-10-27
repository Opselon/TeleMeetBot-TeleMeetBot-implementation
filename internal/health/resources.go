package health

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// ServerResources holds point-in-time system and container resource metrics.
type ServerResources struct {
	CPUUsagePercent int
	CPUScope        string // "system-wide" or "container"
	CPUAvailable    bool

	RAMUsedGiB      float64
	RAMTotalGiB     float64
	RAMUsagePercent int
	RAMAvailable    bool

	SwapUsedGiB      float64
	SwapTotalGiB     float64
	SwapUsagePercent int
	SwapAvailable    bool

	StorageUsedGiB      float64
	StorageTotalGiB     float64
	StorageUsagePercent int
	StorageAvailable    bool

	CollectedAt time.Time
}

var (
	globalCollector *resourceCollector
	collectorOnce   sync.Once
)

type resourceCollector struct {
	mu           sync.RWMutex
	dataDir      string
	cached       ServerResources
	lastSampled  time.Time
	prevTotalCPU uint64
	prevBusyCPU  uint64
	prevCgroupUs uint64
	prevSampleAt time.Time
	tickerCancel context.CancelFunc
}

func getCollector() *resourceCollector {
	collectorOnce.Do(func() {
		globalCollector = &resourceCollector{
			dataDir: ".",
		}
		// Prime the collector with an initial sample
		globalCollector.sampleLocked(true)
	})
	return globalCollector
}

// SetResourceDataDir configures the directory monitored for storage metrics.
func SetResourceDataDir(dir string) {
	c := getCollector()
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(dir) != "" {
		c.dataDir = dir
	}
}

// StartResourceMonitor starts a background ticker that refreshes resource
// metrics periodically so requests can be served instantly without syscall overhead.
func StartResourceMonitor(ctx context.Context, dataDir string) {
	c := getCollector()
	c.mu.Lock()
	if strings.TrimSpace(dataDir) != "" {
		c.dataDir = dataDir
	}
	if c.tickerCancel != nil {
		c.tickerCancel()
	}
	subCtx, cancel := context.WithCancel(ctx)
	c.tickerCancel = cancel
	c.mu.Unlock()

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-subCtx.Done():
				return
			case <-ticker.C:
				c.mu.Lock()
				c.sampleLocked(false)
				c.mu.Unlock()
			}
		}
	}()
}

// GetServerResources returns current resource metrics. It uses a lightweight
// bounded cache (< 1.5s) to avoid excessive system calls.
func GetServerResources() ServerResources {
	c := getCollector()
	c.mu.RLock()
	// If cache is fresh and CPU is available, return immediately.
	if time.Since(c.lastSampled) < 1500*time.Millisecond && c.cached.CPUAvailable {
		res := c.cached
		c.mu.RUnlock()
		return res
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check under write lock
	if time.Since(c.lastSampled) < 1500*time.Millisecond && c.cached.CPUAvailable {
		return c.cached
	}
	c.sampleLocked(false)
	return c.cached
}

// formatStorageTotal formats the total gigabytes for storage.
// Whole gigabytes like 80.0 format as "80", fractional like 79.5 format as "79.5".
func formatStorageTotal(g float64) string {
	if g <= 0 {
		return "0"
	}
	if math.Abs(g-math.Round(g)) < 0.05 {
		return fmt.Sprintf("%.0f", math.Round(g))
	}
	return fmt.Sprintf("%.1f", g)
}

// FormatServerResources formats the current server resource metrics for
// display in session status messages.
func FormatServerResources() string {
	return FormatServerResourcesFrom(GetServerResources())
}

// FormatServerResourcesFrom formats a ServerResources instance into the exact
// 5-line block required by TeleMeetBot session status cards.
func FormatServerResourcesFrom(res ServerResources) string {
	var sb strings.Builder
	sb.WriteString("🖥 Server Resources\n")
	if res.CPUAvailable {
		sb.WriteString(fmt.Sprintf("⚙️ CPU: %d%% (%s)\n", res.CPUUsagePercent, res.CPUScope))
	} else {
		sb.WriteString("⚙️ CPU: N/A\n")
	}
	if res.RAMAvailable {
		sb.WriteString(fmt.Sprintf("🧠 RAM: %.1f / %.1f GiB (%d%%)\n", res.RAMUsedGiB, res.RAMTotalGiB, res.RAMUsagePercent))
	} else {
		sb.WriteString("🧠 RAM: N/A\n")
	}
	if res.SwapAvailable {
		sb.WriteString(fmt.Sprintf("🔄 Swap: %.1f / %.1f GiB (%d%%)\n", res.SwapUsedGiB, res.SwapTotalGiB, res.SwapUsagePercent))
	} else {
		sb.WriteString("🔄 Swap: N/A\n")
	}
	if res.StorageAvailable {
		sb.WriteString(fmt.Sprintf("💽 Storage: %.1f / %s GiB used (%d%%)", res.StorageUsedGiB, formatStorageTotal(res.StorageTotalGiB), res.StorageUsagePercent))
	} else {
		sb.WriteString("💽 Storage: N/A")
	}
	return sb.String()
}
