//go:build windows

package svcinst

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// windowsInstaller registers TeleMeetBot with the Windows Service Control
// Manager. Two implementations live behind one surface:
//
//   - when the process is itself running as a service, the SCM wants the
//     binary to call StartServiceCtrlDispatcher and run the service loop
//     in-process. servicedispatch.go (windows only) handles that.
//   - when the wizard installs from an interactive shell, the registration
//     is done with the sc.exe or a PowerShell New-Service call, which is
//     what this file does.
//
// A binary that is not started by the SCM must not call
// StartServiceCtrlDispatcher: it fails immediately with
// ERROR_FAILED_SERVICE_CONTROLLER_CONNECT, and that failure is expected and
// must not abort a normal interactive run.
type windowsInstaller struct{}

// Install registers the service with sc.exe and starts it.
func (windowsInstaller) Install(opts Options) error {
	normalize(&opts)
	if err := mustExist(opts.Binary); err != nil {
		return err
	}
	if opts.ConfigPath == "" {
		return fmt.Errorf("no config path given")
	}
	if !isElevated() {
		return fmt.Errorf("windows service install needs an elevated (administrator) shell")
	}

	bin := quoteArg(opts.Binary)
	// The SCM binPath must be a single fully-quoted argument string; the
	// config path goes in as a second quoted argument.
	binPath := bin + " --config " + quoteArg(opts.ConfigPath)

	// Delete any previous registration: sc create fails with
	// "The specified service already exists" on an idempotent re-install.
	_ = run("sc.exe", "stop", opts.Name)
	_ = run("sc.exe", "delete", opts.Name)

	if err := run("sc.exe", "create", opts.Name, "binPath=", binPath,
		"DisplayName=", opts.DisplayName, "start=", "auto"); err != nil {
		return err
	}
	if err := run("sc.exe", "description", opts.Name, opts.Description); err != nil {
		return err
	}
	// Failure recovery: restart on the exit codes a crash produces.
	delay := "5000"
	if err := run("sc.exe", "failure", opts.Name, "reset=", "86400",
		"actions=", "restart/"+delay); err != restartOnly(err) {
		return err
	}
	return run("sc.exe", "start", opts.Name)
}

// restartOnly passes a failure-action error through; sc failure is not
// available on every SKU, and a missing restart policy must not fail an
// otherwise successful install.
func restartOnly(err error) error { return err }

// Uninstall stops and deletes the service registration.
func (windowsInstaller) Uninstall(name string) error {
	if name == "" {
		name = DefaultName
	}
	if !isElevated() {
		return fmt.Errorf("windows service uninstall needs an elevated (administrator) shell")
	}
	_ = run("sc.exe", "stop", name)
	if err := run("sc.exe", "delete", name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Status reads the service state with sc.exe query.
func (windowsInstaller) Status(name string) (string, bool, error) {
	if name == "" {
		name = DefaultName
	}
	out, err := execOutput("sc.exe", "query", name)
	if err != nil {
		return "not-installed", false, fmt.Errorf("no service named %s", name)
	}
	state := "unknown"
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "STATE") {
			if i := strings.Index(line, ":"); i >= 0 {
				state = strings.TrimSpace(line[i+1:])
			}
		}
	}
	return state, strings.Contains(state, "RUNNING"), nil
}

// quoteArg wraps an argument in double quotes for the Windows command line.
func quoteArg(a string) string {
	return "\"" + strings.ReplaceAll(a, `"`, `\"`) + "\""
}
