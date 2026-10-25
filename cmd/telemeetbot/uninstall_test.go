package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/svcinst"
)

// fakeUninstallDeps builds a dependency set where the "installed binary" is a
// temp file (never the binary the test suite is running from) and the service
// installer is a stub, so no test can touch the host supervisor or delete the
// running test binary.
func fakeUninstallDeps(t *testing.T, dir string, inst svcinst.Installer) (uninstallDeps, string) {
	t.Helper()
	bin := filepath.Join(dir, "telemeetbot")
	if err := os.WriteFile(bin, []byte("fake installed binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return uninstallDeps{
		resolveSelf:  func() (string, error) { return bin, nil },
		cwd:          func() string { return t.TempDir() },
		newInstaller: func() svcinst.Installer { return inst },
		remove:       os.Remove,
	}, bin
}

// noInstaller models a host with no service system at all (a container
// without systemd): New() returns nil there.
type noInstaller struct{}

func (noInstaller) Install(svcinst.Options) error { return errors.New("not supported") }
func (noInstaller) Uninstall(string) error        { return errors.New("not supported") }
func (noInstaller) Status(string) (string, bool, error) {
	return "not-installed", false, fmt.Errorf("no service")
}

// runningInstaller models a host where the telemeetbot service is live.
type runningInstaller struct{ uninstalled bool }

func (r *runningInstaller) Install(svcinst.Options) error { return nil }
func (r *runningInstaller) Uninstall(string) error        { r.uninstalled = true; return nil }
func (r *runningInstaller) Status(string) (string, bool, error) {
	return "running", true, nil
}

// TestUninstallKeepsConfigAndData proves an uninstall removes the binary but
// leaves the config behind: the token and the session history must survive an
// uninstall so a re-install picks up where the operator left off.
func TestUninstallKeepsConfigAndData(t *testing.T) {
	dir := t.TempDir()
	deps, bin := fakeUninstallDeps(t, dir, noInstaller{})
	cfg := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfg, []byte("telegram:\n  token: \"keep-me\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := runUninstallWith(cfg, true, true, strings.NewReader(""), &out, deps); err != nil {
		t.Fatalf("uninstall failed: %v\noutput:\n%s", err, out.String())
	}
	if _, err := os.Stat(bin); err == nil {
		t.Fatal("the binary was not removed")
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("uninstall deleted the config, which must survive: %v", err)
	}
	got, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "keep-me") {
		t.Fatalf("config contents changed: %q", got)
	}
	if !strings.Contains(out.String(), "binary removed") {
		t.Fatalf("uninstall never confirmed the removal:\n%s", out.String())
	}
}

// TestUninstallRefusesInTreeBinary proves --uninstall never deletes a binary
// that sits in the current working directory (a developer build): the
// operator's build output is not an installed artifact, and removing it would
// silently destroy local work.
func TestUninstallRefusesInTreeBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "telemeetbot")
	if err := os.WriteFile(bin, []byte("fake build artifact"), 0o755); err != nil {
		t.Fatal(err)
	}
	deps := uninstallDeps{
		resolveSelf:  func() (string, error) { return bin, nil },
		cwd:          func() string { return dir },
		newInstaller: func() svcinst.Installer { return nil },
		remove: func(p string) error {
			t.Errorf("remove was called on the in-tree binary %s", p)
			return os.Remove(p)
		},
	}

	var out strings.Builder
	if err := runUninstallWith(filepath.Join(dir, "config.yml"), true, true, strings.NewReader(""), &out, deps); err != nil {
		t.Fatalf("uninstall failed: %v", err)
	}
	if !strings.Contains(out.String(), "not deleting") {
		t.Fatalf("uninstall did not refuse the in-tree binary:\n%s", out.String())
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("in-tree binary was deleted: %v", err)
	}
}

// TestUninstallNeedsConfirmationForRunningService proves a running service is
// never stopped without an explicit "yes": on a host with a live bot the
// uninstall must fail closed rather than drop real sessions.
func TestUninstallNeedsConfirmationForRunningService(t *testing.T) {
	dir := t.TempDir()
	inst := &runningInstaller{}
	deps, _ := fakeUninstallDeps(t, dir, inst)

	var out strings.Builder
	err := runUninstallWith(filepath.Join(dir, "config.yml"), true, false, strings.NewReader("no\n"), &out, deps)
	if err == nil {
		t.Fatal("uninstall of a running service without confirmation must fail")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected a cancellation error, got: %v", err)
	}
	if inst.uninstalled {
		t.Fatal("the service was uninstalled despite the operator declining")
	}
}

// TestUninstallYesFlagBypassesConfirmation proves --yes is the documented
// escape hatch for unattended uninstalls.
func TestUninstallYesFlagBypassesConfirmation(t *testing.T) {
	dir := t.TempDir()
	inst := &runningInstaller{}
	deps, bin := fakeUninstallDeps(t, dir, inst)

	var out strings.Builder
	if err := runUninstallWith(filepath.Join(dir, "config.yml"), true, true, strings.NewReader(""), &out, deps); err != nil {
		t.Fatalf("uninstall with --yes failed: %v", err)
	}
	if !inst.uninstalled {
		t.Fatal("the service was not uninstalled despite --yes")
	}
	if _, err := os.Stat(bin); err == nil {
		t.Fatal("the binary was not removed despite --yes")
	}
}

// TestUninstallKeepBinaryRemovesServiceOnly proves --keep-binary unregisters
// the service and leaves the binary in place.
func TestUninstallKeepBinaryRemovesServiceOnly(t *testing.T) {
	dir := t.TempDir()
	inst := &runningInstaller{}
	deps, bin := fakeUninstallDeps(t, dir, inst)

	var out strings.Builder
	if err := runUninstallWith(filepath.Join(dir, "config.yml"), false, true, strings.NewReader(""), &out, deps); err != nil {
		t.Fatalf("uninstall --keep-binary failed: %v", err)
	}
	if !inst.uninstalled {
		t.Fatal("the service was not uninstalled")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("the binary was deleted despite --keep-binary: %v", err)
	}
	if !strings.Contains(out.String(), "keeping the binary") {
		t.Fatalf("uninstall never said it kept the binary:\n%s", out.String())
	}
}
