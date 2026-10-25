package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSystemConfigDirPlatform proves the canonical config directory is never
// empty and is a real system location for the platform (not a relative path
// that would resolve differently depending on the service's working dir).
func TestSystemConfigDirPlatform(t *testing.T) {
	dir := SystemConfigDir()
	if dir == "" {
		t.Fatal("SystemConfigDir must not be empty")
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("SystemConfigDir must be absolute, got %q", dir)
	}
	if runtime.GOOS == "windows" {
		if !strings.Contains(strings.ToLower(dir), "programdata") {
			t.Fatalf("windows config dir should be under ProgramData, got %q", dir)
		}
		return
	}
	if dir != "/etc/telemeetbot" {
		t.Fatalf("unix config dir = %q, want /etc/telemeetbot", dir)
	}
}

// TestDefaultConfigPathNeverRelativeForRoot proves the wizard never tries to
// write into the service's C:\Windows or a random cwd.
func TestDefaultConfigPathNeverRelativeForRoot(t *testing.T) {
	p := DefaultConfigPath()
	if p == "" {
		t.Fatal("DefaultConfigPath must not be empty")
	}
	if runtime.GOOS == "windows" {
		if !filepath.IsAbs(p) {
			t.Fatalf("windows default config must be absolute, got %q", p)
		}
		return
	}
	if os.Geteuid() == 0 && !filepath.IsAbs(p) {
		t.Fatalf("root default config must be absolute, got %q", p)
	}
}

// TestExpandUserPath proves ~/... resolves on every platform: the wizard
// accepts it and must not create a directory literally named "~".
func TestExpandUserPath(t *testing.T) {
	if got := ExpandUserPath(""); got != "" {
		t.Fatalf("empty path must stay empty, got %q", got)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	if got := ExpandUserPath("~"); got != home {
		t.Fatalf("ExpandUserPath(~) = %q, want %q", got, home)
	}
	got := ExpandUserPath("~/telemeetbot.yml")
	want := filepath.Join(home, "telemeetbot.yml")
	if got != want {
		t.Fatalf("ExpandUserPath(~/telemeetbot.yml) = %q, want %q", got, want)
	}
	// An absolute path passes through untouched.
	abs := "/etc/telemeetbot/config.yml"
	if runtime.GOOS == "windows" {
		abs = `C:\ProgramData\TeleMeetBot\config.yml`
	}
	if got := ExpandUserPath(abs); got != abs {
		t.Fatalf("absolute path changed: %q", got)
	}
}

// TestDefaultDataDir proves the fresh-install data dir is a real location on
// Windows (a relative "data" would land inside the service's system dir).
func TestDefaultDataDir(t *testing.T) {
	d := DefaultDataDir()
	if d == "" {
		t.Fatal("DefaultDataDir must not be empty")
	}
	if runtime.GOOS == "windows" && !filepath.IsAbs(d) {
		t.Fatalf("windows default data dir must be absolute, got %q", d)
	}
}
