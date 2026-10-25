//go:build linux

package svcinst

import (
	"os/exec"
	"strings"
)

// System reports which service system is available on this host. Detection
// is behavioural: it checks for the supervisor binary/IPC surface, not just
// the OS, so a Linux container without systemd reports KindNone and the
// wizard falls back to a plain process.
func systemForOS() Kind {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return KindNone
	}
	// systemctl exists in containers without a running PID 1 supervisor; ask
	// systemd itself. is-system-running exits non-zero for every non-"running"
	// state, so the output text is what distinguishes a live supervisor.
	out, err := exec.Command("systemctl", "is-system-running").CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		// "degraded" is a live systemd with a failed unit; that is still a
		// supervisor an install can target. Anything else (offline/down) is not.
		if s == "degraded" {
			return KindSystemd
		}
		return KindNone
	}
	return KindSystemd
}

// New returns the installer for this host, or nil when there is no service
// system to install into.
func installerForOS() Installer {
	if systemForOS() == KindSystemd {
		return &systemdInstaller{}
	}
	return nil
}
