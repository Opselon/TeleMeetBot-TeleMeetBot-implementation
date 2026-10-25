package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// SystemConfigDir returns the canonical location for telemeetbot.yml on the
// current platform: /etc/telemeetbot on Unix, %ProgramData%\TeleMeetBot on
// Windows. The Windows convention is a machine-wide, service-writable
// directory — %ProgramData% is the documented equivalent of /etc.
func SystemConfigDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); strings.TrimSpace(pd) != "" {
			return filepath.Join(pd, "TeleMeetBot")
		}
		// Fallback when the environment is stripped (some service hosts do
		// not set ProgramData).
		return filepath.Join(os.Getenv("SystemDrive"), "ProgramData", "TeleMeetBot")
	}
	return "/etc/telemeetbot"
}

// SystemDataDir returns the default data directory for a system service:
// /var/lib/telemeetbot on Unix, %ProgramData%\TeleMeetBot\data on Windows.
func SystemDataDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(SystemConfigDir(), "data")
	}
	return "/var/lib/telemeetbot"
}

// DefaultDataDir returns the data directory a fresh interactive install
// should use: relative "data" on Unix (matches the historical default and
// keeps a checkout self-contained), %ProgramData%\TeleMeetBot\data on Windows
// where a relative path would land in the service's C:\Windows.
func DefaultDataDir() string {
	if runtime.GOOS == "windows" {
		return SystemDataDir()
	}
	return "data"
}

// DefaultConfigPath is where the wizard writes when no path is given: the
// system directory for root, ./telemeetbot.yml for a normal user. Windows
// non-admin installs fall back to the user's LOCALAPPDATA folder.
func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); strings.TrimSpace(local) != "" {
			return filepath.Join(local, "TeleMeetBot", "config.yml")
		}
		return filepath.Join(SystemConfigDir(), "config.yml")
	}
	if os.Geteuid() == 0 {
		return "/etc/telemeetbot/config.yml"
	}
	return "telemeetbot.yml"
}

// ExpandUserPath resolves a leading ~ to the current user's home directory.
// The wizard accepts ~/telemeetbot.yml on every platform; on Windows ~ maps
// to %USERPROFILE%.
func ExpandUserPath(p string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~"+string(os.PathSeparator)) || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[2:])
	}
	return p
}
