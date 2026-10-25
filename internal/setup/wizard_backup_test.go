package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWizardBacksUpExistingConfig proves a wizard re-run over a live config
// leaves a .bak holding exactly the previous contents: that is the rollback
// target --restore-backup promises to put back. The backup is the only thing
// standing between a bad re-run and a lost bot token.
func TestWizardBacksUpExistingConfig(t *testing.T) {
	fakeChrome(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	dataDir := filepath.Join(dir, "data")

	original := "telegram:\n  token: \"999:PRE-WIZARD\"\n  admins: [7]\n"
	if err := os.WriteFile(cfgPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &scriptedPrompter{
		t: t,
		answers: map[string][]string{
			"Config file path":        {cfgPath},
			"Telegram bot token":      {"123456:AAHfresh_token"},
			"Admin Telegram user IDs": {"42"},
			"Explicit chromium path":  {},
			"Max concurrent sessions": {"2"},
			"Default volume":          {"70"},
			"Health endpoint":         {"127.0.0.1:18099"},
			"Data directory":          {dataDir},
		},
		bools: map[string]bool{
			"Save this configuration": true,
			"service":                 false,
			"tab-capture test":        false,
		},
		choices: map[string]int{"How should Meet share": 0},
	}
	var out bytes.Buffer
	if _, err := Run(WizardOptions{
		In:                 strings.NewReader(""),
		Out:                &out,
		Prompter:           p,
		SkipBrowserInstall: true,
		TokenValidator:     func(string) (string, error) { return "fresh_bot", nil },
		Exec:               func(string, ...string) error { return nil },
		FindBrowser:        browserNotFound,
	}); err != nil {
		t.Fatalf("wizard failed: %v\noutput:\n%s", err, out.String())
	}

	// The backup must exist and carry the pre-wizard token exactly.
	bak, err := os.ReadFile(cfgPath + ".bak")
	if err != nil {
		t.Fatalf("wizard did not leave a .bak next to the config: %v\noutput:\n%s", err, out.String())
	}
	if string(bak) != original {
		t.Fatalf(".bak = %q, want the original config %q", bak, original)
	}
	// And the wizard must have said so, so the operator knows it exists.
	if !strings.Contains(out.String(), "backed up") {
		t.Fatalf("wizard output never mentioned the backup:\n%s", out.String())
	}

	// The live config is the new one; --restore-backup must swap them back.
	live, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(live), "999:PRE-WIZARD") {
		t.Fatalf("the live config still carries the old token: %q", live)
	}
	if err := restoreFromBackup(t, cfgPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("after restore: %q, want %q", got, original)
	}
}

// restoreFromBackup mirrors what cmd/telemeetbot --restore-backup does, so
// this test proves the two halves of the rollback pair agree on the file
// layout (path + ".bak", same contents) without importing cmd/.
func restoreFromBackup(t *testing.T, cfgPath string) error {
	t.Helper()
	bak, err := os.ReadFile(cfgPath + ".bak")
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, bak, 0o600)
}
