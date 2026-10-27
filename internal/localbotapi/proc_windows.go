//go:build windows

package localbotapi

import (
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd) {
	// Process groups work differently on Windows; default process attributes are sufficient.
}

func terminateProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func killProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
