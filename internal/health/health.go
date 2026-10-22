// Package health collects runtime facts for the dashboard and /healthz.
package health

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
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
func DiskUsage(path string) (freeGB, totalGB float64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := float64(st.Bsize)
	return float64(st.Bavail) * bs / (1 << 30), float64(st.Blocks) * bs / (1 << 30)
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
