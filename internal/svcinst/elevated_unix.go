//go:build !windows

package svcinst

import "os"

// isElevated reports whether the current process is privileged on Unix:
// euid 0 (root). systemd installs need it, and the error message for a
// non-root install should say so before systemctl is ever run.
func isElevated() bool {
	return os.Geteuid() == 0
}
