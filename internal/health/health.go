// Package health collects runtime facts for the dashboard and /healthz.
package health

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

// Snapshot is a point-in-time view of the service.
type Snapshot struct {
	Version     string
	GoVersion   string
	Uptime      time.Duration
	StartedAt   time.Time
	ActiveCount int
	MaxSessions int
	BrowserPath string
	ConfigPath  string
	DataDir     string
	DiskFreeGB  float64
	DiskTotalGB float64
	PID         int
}

// DiskUsage reports free/total space on the config data filesystem.
//
// On Unix it is a statfs(2) lookup; on Windows the same information comes
// from GetDiskFreeSpaceEx through the standard library. Either way a failure
// is reported as zeros instead of aborting the health surface.
func DiskUsage(path string) (freeGB, totalGB float64) {
	free, total, ok := diskUsage(path)
	if !ok {
		return 0, 0
	}
	return float64(free) / (1 << 30), float64(total) / (1 << 30)
}

// FormatUptime renders a human duration, e.g. "3h 12m".
func FormatUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Env summarises the host the bot runs on.
func Env() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s · go%s", host, runtime.Version())
}
