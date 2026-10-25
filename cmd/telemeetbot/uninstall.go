package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/svcinst"
)

// uninstallDeps is the host surface runUninstall touches. Every field has a
// production default and can be replaced by a test, so the uninstall tests
// never point at the binary the test suite is running from, and never touch
// the host's real service supervisor.
type uninstallDeps struct {
	// resolveSelf returns the path of the running binary (os.Executable).
	resolveSelf func() (string, error)
	// cwd is the working directory used by the in-tree build guard.
	cwd func() string
	// newInstaller returns the OS service installer (svcinst.New) or nil.
	newInstaller func() svcinst.Installer
	// remove deletes a path (os.Remove).
	remove func(string) error
}

// defaultUninstallDeps is the production surface.
func defaultUninstallDeps() uninstallDeps {
	return uninstallDeps{
		resolveSelf:  os.Executable,
		cwd:          func() string { d, _ := os.Getwd(); return d },
		newInstaller: svcinst.New,
		remove:       os.Remove,
	}
}

// runUninstall stops and removes the OS service registration, then (unless
// keepBinary is set) deletes the installed binary. The config, data directory
// and session history are deliberately untouched: an uninstall must never
// destroy the operator's token or meetings — that is what a separate
// --purge would be for, and it does not exist on purpose.
//
// The command is idempotent: a missing service, a missing binary, or a host
// with no service system at all all succeed, so a re-run after a partial
// uninstall always converges instead of failing halfway.
//
// Uninstalling a *running* service is destructive, so it requires an explicit
// confirmation: either --yes on the command line or a typed "yes" on stdin.
// A non-interactive context with neither fails closed instead of stopping a
// live bot.
func runUninstall(cfgFlag string, deleteBinary bool, yes bool, in io.Reader, out io.Writer) error {
	return runUninstallWith(cfgFlag, deleteBinary, yes, in, out, defaultUninstallDeps())
}

// runUninstallWith is the testable core; see runUninstall for the contract.
func runUninstallWith(cfgFlag string, deleteBinary bool, yes bool, in io.Reader, out io.Writer, deps uninstallDeps) error {
	name := svcinst.DefaultName

	// Remove the service registration first: a running service would keep the
	// binary busy and the delete below would fail on Windows.
	if inst := deps.newInstaller(); inst != nil {
		if state, running, err := inst.Status(name); err == nil && running {
			if !yes {
				fmt.Fprintf(out, "⚠️  the %s service is currently running (%s).\n", name, state)
				fmt.Fprint(out, "    Uninstalling will stop the bot and drop any live sessions.\n    Type 'yes' to confirm: ")
				buf := bufio.NewReader(in)
				line, _ := buf.ReadString('\n')
				if strings.TrimSpace(strings.ToLower(line)) != "yes" {
					return errors.New("uninstall cancelled; the service was left untouched")
				}
			}
		}
		if err := inst.Uninstall(name); err != nil {
			// Report but continue: a half-removed registration must not block
			// the rest of the uninstall.
			fmt.Fprintf(out, "  ⚠️  service removal: %v\n", err)
		} else {
			fmt.Fprintf(out, "✅ service %s stopped and removed\n", name)
		}
	} else {
		fmt.Fprintln(out, "ℹ️  no service system on this host; nothing to remove")
	}

	if !deleteBinary {
		fmt.Fprintln(out, "ℹ️  keeping the binary (--keep-binary)")
		return nil
	}

	exe, err := deps.resolveSelf()
	if err != nil {
		return fmt.Errorf("resolve own executable: %w", err)
	}
	// Never delete the binary the operator built in a checkout: an in-tree
	// path means this is a developer binary, not an installed one.
	if rel, rerr := filepath.Rel(deps.cwd(), exe); rerr == nil && rel == "telemeetbot" {
		fmt.Fprintf(out, "ℹ️  %s is a build artifact in the current directory; not deleting\n", exe)
		return nil
	}
	if err := deps.remove(exe); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete binary %s: %w", exe, err)
	}
	fmt.Fprintf(out, "✅ binary removed (%s)\n", exe)
	fmt.Fprintf(out, "ℹ️  config and data kept at %s — delete manually if you want a full purge\n", config.ResolvePath(cfgFlag))
	return nil
}
