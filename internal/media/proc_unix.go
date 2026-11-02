//go:build !windows

package media

import (
	"os/exec"
	"syscall"
)

// setProcessGroup and killProcessGroup are the unix half of subprocess
// management for the yt-dlp/ffmpeg exec path. They mirror
// internal/localbotapi's proc_unix.go: yt-dlp spawns ffmpeg as a child, so a
// plain cmd.Process.Kill leaves the transcode running — holding the output
// file open and burning CPU after the parent already reported failure.
// Killing the process group takes the child with it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the whole process group. On any failure it
// degrades to killing the process itself, so a child that could not be
// grouped still dies.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil && pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = cmd.Process.Kill()
}
