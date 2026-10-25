//go:build windows

package svcinst

import "golang.org/x/sys/windows"

// isElevated reports whether the current process is running with
// administrator rights. Only an elevated shell can create a service.
func isElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}
