//go:build windows

package svcinst

// System reports the Windows Service Control Manager. Every supported
// Windows has one; do not try to probe it from a non-service context, that
// can block on a supervisor handshake.
func systemForOS() Kind { return KindWindows }

// New returns the Windows service installer.
func installerForOS() Installer { return &windowsInstaller{} }
