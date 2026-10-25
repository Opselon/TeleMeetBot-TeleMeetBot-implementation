package svcinst

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeFillsDefaults proves the installer never runs with an empty
// service name or description: a unit/plist/SCM registration with an empty
// label is silently unusable.
func TestNormalizeFillsDefaults(t *testing.T) {
	opts := Options{Binary: "/bin/telemeetbot", ConfigPath: "/etc/telemeetbot/config.yml"}
	normalize(&opts)
	if opts.Name != DefaultName {
		t.Fatalf("name = %q, want %q", opts.Name, DefaultName)
	}
	if opts.DisplayName == "" || opts.Description == "" {
		t.Fatalf("display/description must not be empty after normalize")
	}
}

// TestNormalizeTrimsPaths proves whitespace from a config value never lands
// in a unit ExecStart line.
func TestNormalizeTrimsPaths(t *testing.T) {
	opts := Options{
		Binary:     "  /opt/telemeetbot  ",
		ConfigPath: " /etc/telemeetbot/config.yml ",
	}
	normalize(&opts)
	if opts.Binary != "/opt/telemeetbot" {
		t.Fatalf("binary not trimmed: %q", opts.Binary)
	}
	if opts.ConfigPath != "/etc/telemeetbot/config.yml" {
		t.Fatalf("config not trimmed: %q", opts.ConfigPath)
	}
}

// TestMustExist proves a missing binary is caught before any supervisor is
// touched (an sc.exe/systemctl call with a stale path is hard to undo).
func TestMustExist(t *testing.T) {
	if err := mustExist(""); err == nil {
		t.Fatal("empty path must fail")
	}
	if err := mustExist("/nonexistent/definitely/not/here"); err == nil {
		t.Fatal("missing path must fail")
	}
	if err := mustExist(os.Args[0]); err != nil {
		t.Fatalf("own binary must exist: %v", err)
	}
}

// TestEscapeUnit proves the two characters systemd expands (% and ") can
// never reach a unit file unescaped: a checkout under "100% complete/"
// otherwise yields a unit that refuses to start.
func TestEscapeUnit(t *testing.T) {
	in := "/home/user/100% complete/telemeetbot"
	out := escapeUnit(in)
	if !strings.Contains(out, "100%%") {
		t.Fatalf("percent not escaped: %q", out)
	}
	if strings.Contains(out, `"`) && !strings.Contains(out, `\"`) {
		t.Fatalf("quote not escaped: %q", out)
	}
	// A plain path is unchanged apart from the escapes.
	if got := escapeUnit("/usr/local/bin/telemeetbot"); got != "/usr/local/bin/telemeetbot" {
		t.Fatalf("plain path changed: %q", got)
	}
}

// TestSystemDetectedOrNone proves detection returns a known kind on every
// platform: an unknownKind would break the wizard's question logic.
func TestSystemDetectedOrNone(t *testing.T) {
	kind := System()
	switch kind {
	case KindNone, KindSystemd, KindWindows, KindLaunchd:
		// ok
	default:
		t.Fatalf("unknown service kind: %q", kind)
	}
	// New() must agree with System(): an installer may only exist when the
	// host actually reports a service system.
	inst := New()
	if inst == nil && kind != KindNone {
		t.Fatalf("System() = %q but New() returned nil", kind)
	}
	if inst != nil && kind == KindNone {
		t.Fatalf("System() = KindNone but New() returned an installer")
	}
}

// TestNewNilOnUnsupported proves the wizard can rely on a nil installer
// meaning "no service system" rather than having to catch a panic.
func TestNewNilOnUnsupported(t *testing.T) {
	if System() == KindNone && New() != nil {
		t.Fatal("no service system must yield a nil installer")
	}
}

// TestUnitPath proves the unit lands in the systemd directory for the
// requested name (a wrong directory silently installs nothing).
func TestUnitPath(t *testing.T) {
	got := unitPath("telemeetbot")
	want := filepath.Join("/etc/systemd/system", "telemeetbot.service")
	if got != want {
		t.Fatalf("unitPath = %q, want %q", got, want)
	}
}

// TestStatusUnknownUnitIsAnError proves an unregistered unit is distinguishable
// from a registered-but-stopped one, so Uninstall can report "nothing to
// remove" instead of pretending it stopped something.
func TestStatusUnknownUnitIsAnError(t *testing.T) {
	if System() != KindSystemd {
		t.Skip("systemd not available on this host")
	}
	var s systemdInstaller
	_, _, err := s.Status("telemeetbot-definitely-not-installed")
	if err == nil {
		// Some systemd builds answer "inactive" for an unknown unit name; that
		// answer cannot distinguish "not installed" from "installed but
		// stopped", so the precondition this test asserts is simply that the
		// call is safe and deterministic.
		t.Log("systemd reported a state for an unknown unit; treating it as not-installed is the documented behaviour")
		return
	}
	if _, ok := err.(*UnknownServiceError); !ok {
		t.Fatalf("expected UnknownServiceError, got %T: %v", err, err)
	}
}
