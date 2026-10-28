package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/config"
)

func TestProfileConfiguration(t *testing.T) {
	cfg := config.Default()

	// Test ProfileMinimal disables media downloads
	optsMin := WizardOptions{
		NonInteractive: true,
		Profile:        ProfileMinimal,
		TokenValidator: func(string) (string, error) { return "test_bot", nil },
	}
	t.Setenv("TELEMEETBOT_TOKEN", "123:test")
	dir := t.TempDir()
	optsMin.ConfigPath = filepath.Join(dir, "min.yml")

	savedMin, err := Run(optsMin)
	if err != nil {
		t.Fatalf("Run(ProfileMinimal): %v", err)
	}
	if savedMin.Download.Enabled {
		t.Errorf("expected Download.Enabled = false for ProfileMinimal, got true")
	}

	// Test ProfileFull enables media downloads
	optsFull := WizardOptions{
		NonInteractive:   true,
		Profile:          ProfileFull,
		SkipMediaInstall: true,
		TokenValidator:   func(string) (string, error) { return "test_bot", nil },
	}
	optsFull.ConfigPath = filepath.Join(dir, "full.yml")

	savedFull, err := Run(optsFull)
	if err != nil {
		t.Fatalf("Run(ProfileFull): %v", err)
	}
	if !savedFull.Download.Enabled {
		t.Errorf("expected Download.Enabled = true for ProfileFull, got false")
	}
	_ = cfg
}

func TestCheckAllDependencies(t *testing.T) {
	cfg := config.Default()
	deps := CheckAllDependencies(cfg, nil)
	if len(deps) < 4 {
		t.Fatalf("expected at least 4 dependencies checked, got %d", len(deps))
	}

	ids := make(map[DependencyID]bool)
	for _, d := range deps {
		ids[d.ID] = true
		if d.Name == "" {
			t.Errorf("dependency %s missing Name", d.ID)
		}
		if d.FallbackHint == "" {
			t.Errorf("dependency %s missing FallbackHint", d.ID)
		}
	}

	for _, required := range []DependencyID{DepChromium, DepYTDLP, DepFFmpeg, DepDeno} {
		if !ids[required] {
			t.Errorf("missing expected dependency check: %s", required)
		}
	}
}

func TestPackageManagerDetection(t *testing.T) {
	pm := DetectPackageManager()
	// On Linux or macOS or Windows, if an OS package manager exists, it should be recognized
	if pm != nil {
		t.Logf("Discovered package manager: %s at %s", pm.Name, pm.Path)
		if pm.Name == "" || pm.Path == "" {
			t.Errorf("invalid package manager discovered: %+v", pm)
		}
	}
}

func TestTransactionalBinaryInstallation(t *testing.T) {
	targetDir := t.TempDir()
	var out bytes.Buffer

	installed, err := installBinaryTransactionally(targetDir, &out)
	if err != nil {
		t.Fatalf("installBinaryTransactionally: %v", err)
	}

	if !strings.HasPrefix(installed, targetDir) {
		t.Errorf("expected installed binary in %s, got %s", targetDir, installed)
	}

	stat, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("installed binary does not exist: %v", err)
	}
	if runtime.GOOS != "windows" && (stat.Mode()&0111) == 0 {
		t.Errorf("expected installed binary to be executable, got mode %v", stat.Mode())
	}
}
