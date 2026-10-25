// Package svcinst installs, removes and supervises TeleMeetBot as an OS
// service: systemd on Linux, a Windows service through the SCM, and a
// launchd agent on macOS. Everything here must compile on every platform
// (the wizard runs the same code path everywhere) and no-op cleanly where
// the platform has no service system.
package svcinst

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Kind is the service system available on this host.
type Kind string

const (
	KindNone    Kind = "none"
	KindSystemd Kind = "systemd"
	KindWindows Kind = "windows"
	KindLaunchd Kind = "launchd"
)

// Options describes the service to install.
type Options struct {
	// Binary is the executable path the service starts.
	Binary string
	// ConfigPath is passed to the binary as --config.
	ConfigPath string
	// DataDir is the working directory and the dir the service must own.
	DataDir string
	// Name is the service/agent/unit name (default "telemeetbot").
	Name string
	// DisplayName is the human-readable label shown by the SCM / Preferences.
	DisplayName string
	// Description is the long description shown next to the service.
	Description string
}

// System reports which service system is available and usable on this host.
// Detection is behavioural: it checks for the supervisor binary/IPC surface,
// not just the OS, so a Linux container without systemd reports KindNone and
// the wizard falls back to a plain process. The per-OS implementation lives
// in system_<os>.go.
func System() Kind { return systemForOS() }

// New returns the installer for this host, or nil when System() is KindNone.
func New() Installer { return installerForOS() }

// Installer is the platform service installer surface.
type Installer interface {
	// Install creates and enables the service, then starts it.
	Install(opts Options) error
	// Uninstall stops and removes the service.
	Uninstall(name string) error
	// Status reports the current service state as a human-readable line.
	Status(name string) (state string, running bool, err error)
}

// ErrUnsupported is returned when the host has no usable service system.
var ErrUnsupported = errors.New("no service system available on this host")

// supported is true on every platform build that implements New() in
// system_<os>.go. It exists so the cross-platform file documents its own
// contract instead of leaving the reader to grep the build tags.
const supported = true

var _ = supported

// DefaultName is the service/agent/unit name used when Options.Name is empty.
const DefaultName = "telemeetbot"

// normalize fills the optional fields of opts.
func normalize(opts *Options) {
	if strings.TrimSpace(opts.Name) == "" {
		opts.Name = DefaultName
	}
	if strings.TrimSpace(opts.DisplayName) == "" {
		opts.DisplayName = "TeleMeetBot"
	}
	if strings.TrimSpace(opts.Description) == "" {
		opts.Description = "Google Meet streaming bot with a Telegram dashboard"
	}
	opts.Binary = strings.TrimSpace(opts.Binary)
	opts.ConfigPath = strings.TrimSpace(opts.ConfigPath)
}

// run executes a command and wraps a non-zero exit with its stderr tail so
// install failures stay actionable instead of a bare "exit status 1".
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		tail := strings.TrimSpace(string(out))
		if tail == "" {
			tail = err.Error()
		}
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), tail)
	}
	return nil
}

// mustExist fails fast when the binary the service should start is missing.
func mustExist(path string) error {
	if path == "" {
		return errors.New("no binary path given")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("binary %s: %w", path, err)
	}
	return nil
}
