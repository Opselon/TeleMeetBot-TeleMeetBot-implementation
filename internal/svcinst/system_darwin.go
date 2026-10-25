//go:build darwin

package svcinst

import "os/exec"

// System reports which service system is available on macOS: launchd when
// launchctl is present (it always is on a normal install), otherwise none.
func systemForOS() Kind {
	if _, err := exec.LookPath("launchctl"); err == nil {
		return KindLaunchd
	}
	return KindNone
}

// New returns the launchd installer, or nil when launchctl is missing.
func installerForOS() Installer {
	if systemForOS() == KindLaunchd {
		return &launchdInstaller{}
	}
	return nil
}
