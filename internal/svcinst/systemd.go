//go:build !windows

package svcinst

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// systemdInstaller writes a hardened unit under /etc/systemd/system and
// enables+starts it with systemctl.
type systemdInstaller struct{}

// unitPath is where the unit file is written for a service name.
func unitPath(name string) string {
	return filepath.Join("/etc/systemd/system", name+".service")
}

// Install writes the unit, daemon-reloads, then enables and starts it.
func (systemdInstaller) Install(opts Options) error {
	normalize(&opts)
	if err := mustExist(opts.Binary); err != nil {
		return err
	}
	if opts.ConfigPath == "" {
		return fmt.Errorf("no config path given")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("systemd install needs root (run with sudo)")
	}

	workDir := opts.DataDir
	if workDir == "" {
		workDir = filepath.Dir(opts.ConfigPath)
	}

	// Quote every path-bearing line: systemd expands % and " in unit files.
	// The classic trap is a checkout under "100% complete/". escapeUnit
	// handles both so the unit never fails to start over a path character.
	execLine := fmt.Sprintf("ExecStart=%s --config %s", escapeUnit(opts.Binary), escapeUnit(opts.ConfigPath))

	unit := fmt.Sprintf(`[Unit]
Description=%s
Documentation=https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
%s
WorkingDirectory=%s
Restart=always
RestartSec=5
NoNewPrivileges=true
Environment=HOME=%s
LimitNOFILE=65536
# The bot only needs its data dir; everything else stays read-only.
ReadWritePaths=%s
ProtectSystem=full
PrivateTmp=true

[Install]
WantedBy=multi-user.target
`, opts.Description, execLine, escapeUnit(workDir), escapeUnit(homeForService(workDir)), escapeUnit(workDir))

	dir := filepath.Dir(unitPath(opts.Name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(unitPath(opts.Name), []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := run("systemctl", "enable", "--now", opts.Name); err != nil {
		return err
	}
	return nil
}

// homeForService picks a HOME the service user can actually write to; the
// Chromium profile and the SQLite temp files land under it on some hosts.
func homeForService(workDir string) string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	return workDir
}

// Uninstall stops, disables and removes the unit.
func (systemdInstaller) Uninstall(name string) error {
	if name == "" {
		name = DefaultName
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("systemd uninstall needs root (run with sudo)")
	}
	// Best-effort stop: a failed unit must not abort the removal.
	_ = run("systemctl", "stop", name)
	_ = run("systemctl", "disable", name)
	if err := os.Remove(unitPath(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit: %w", err)
	}
	return run("systemctl", "daemon-reload")
}

// Status reads the unit state from systemctl.
func (systemdInstaller) Status(name string) (string, bool, error) {
	if name == "" {
		name = DefaultName
	}
	out, err := execOutput("systemctl", "is-active", name)
	state := trim(out)
	if err != nil {
		// systemctl is-active exits non-zero for every non-active state.
		// "inactive"/"failed" are states, not errors; only an unknown unit
		// name is an error so uninstall stays safe to call twice.
		switch state {
		case "inactive", "failed", "activating", "deactivating":
			return state, false, nil
		}
		return state, false, &UnknownServiceError{Name: name, Output: state}
	}
	return state, state == "active", nil
}

// UnknownServiceError reports a service/unit/agent that is not registered
// with the host supervisor.
type UnknownServiceError struct {
	Name   string
	Output string
}

func (e *UnknownServiceError) Error() string {
	return "no service named " + e.Name + " (" + e.Output + ")"
}

// escapeUnit makes a path safe for a systemd unit file: %% for a literal
// percent, \" for a quote.
func escapeUnit(p string) string {
	p = strings.ReplaceAll(p, "%", "%%")
	return strings.ReplaceAll(p, `"`, `\"`)
}
