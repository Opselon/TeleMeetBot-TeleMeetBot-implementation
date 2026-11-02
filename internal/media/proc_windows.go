//go:build windows

package media

import (
	"os/exec"
)

// setProcessGroup and killProcessGroup are the Windows half of subprocess
// management for the yt-dlp/ffmpeg exec path, mirroring
// internal/localbotapi's proc_windows.go: process groups work differently
// here and the default attributes are sufficient.
func setProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
