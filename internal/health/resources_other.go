//go:build !linux

package health

import (
	"math"
	"time"
)

// sampleLocked provides non-Linux fallback metric collection.
func (c *resourceCollector) sampleLocked(initial bool) {
	now := time.Now()
	var res ServerResources
	res.CollectedAt = now

	// Storage metrics via diskUsage (works on Unix and Windows)
	dataDir := c.dataDir
	if dataDir == "" {
		dataDir = "."
	}
	if free, total, ok := diskUsage(dataDir); ok && total > 0 {
		freeBytes := free
		totalBytes := total
		var usedBytes uint64
		if totalBytes > freeBytes {
			usedBytes = totalBytes - freeBytes
		}
		res.StorageTotalGiB = float64(totalBytes) / (1 << 30)
		res.StorageUsedGiB = float64(usedBytes) / (1 << 30)
		res.StorageUsagePercent = int(math.Round(float64(usedBytes) * 100.0 / float64(totalBytes)))
		if res.StorageUsagePercent < 0 {
			res.StorageUsagePercent = 0
		} else if res.StorageUsagePercent > 100 {
			res.StorageUsagePercent = 100
		}
		res.StorageAvailable = true
	}

	c.cached = res
	c.lastSampled = now
}
