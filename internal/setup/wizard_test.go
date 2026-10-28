package setup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// fakeChrome writes a fake chrome executable for tests (never launched).
func fakeChrome(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chromium")
	script := "#!/bin/sh\necho 'Fake Chrome 999.0'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseIDs(t *testing.T) {
	got, err := parseIDs(" 11, 22 ;33 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != 11 || got[2] != 33 {
		t.Fatalf("parseIDs = %v", got)
	}
	for _, bad := range []string{"abc", "12,x", "-5", "0"} {
		if _, err := parseIDs(bad); err == nil {
			t.Fatalf("expected %q to fail", bad)
		}
	}
}

// scriptedPrompter answers by question keyword, so tests do not depend on
// prompt order (which changes with root/systemd/CI differences).
type scriptedPrompter struct {
	t       *testing.T
	answers map[string][]string
	bools   map[string]bool
	choices map[string]int
	asked   []string
}

func (p *scriptedPrompter) take(q string) (string, bool) {
	for key, queue := range p.answers {
		if !strings.Contains(q, key) || len(queue) == 0 {
			continue
		}
		v := queue[0]
		if len(queue) > 1 {
			p.answers[key] = queue[1:]
		}
		return v, true
	}
	return "", false
}

func (p *scriptedPrompter) Ask(q, def string) (string, error) {
	p.asked = append(p.asked, q)
	if v, ok := p.take(q); ok {
		if v == "" {
			return def, nil
		}
		return v, nil
	}
	return def, nil
}

func (p *scriptedPrompter) AskRequired(q, def string) (string, error) {
	p.asked = append(p.asked, q)
	if v, ok := p.take(q); ok {
		if strings.TrimSpace(v) == "" {
			return "", errors.New("required answer missing")
		}
		return v, nil
	}
	if strings.TrimSpace(def) == "" {
		return "", errors.New("required answer missing")
	}
	return def, nil
}

func (p *scriptedPrompter) Confirm(q string, def bool) (bool, error) {
	p.asked = append(p.asked, q)
	for key, v := range p.bools {
		if strings.Contains(q, key) {
			return v, nil
		}
	}
	return def, nil
}

func (p *scriptedPrompter) Choose(q string, _ []string, def int) (int, error) {
	p.asked = append(p.asked, q)
	for key, v := range p.choices {
		if strings.Contains(q, key) {
			return v, nil
		}
	}
	return def, nil
}

func TestInteractiveWizardWritesConfig(t *testing.T) {
	chrome := fakeChrome(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	dataDir := filepath.Join(dir, "data")

	p := &scriptedPrompter{
		t: t,
		answers: map[string][]string{
			"Config file path":        {cfgPath},
			"Telegram bot token":      {"123456:AAHtest_token"},
			"Admin Telegram user IDs": {"42,43"},
			"Explicit chromium path":  {chrome},
			"Max concurrent sessions": {"3"},
			"Default volume":          {"55"},
			"Health endpoint":         {"127.0.0.1:18099"},
			"Data directory":          {dataDir},
		},
		bools: map[string]bool{
			"Save this configuration": true,
			"systemd service":         false,
			"tab-capture test":        false,
		},
		choices: map[string]int{"How should Meet share": 0},
	}
	var out bytes.Buffer
	cfg, err := Run(WizardOptions{
		In:                 strings.NewReader(""),
		Out:                &out,
		Prompter:           p,
		SkipBrowserInstall: true,
		TokenValidator:     func(string) (string, error) { return "wizard_test_bot", nil },
		Exec:               func(string, ...string) error { return nil },
		FindBrowser:        browserNotFound,
	})
	if err != nil {
		t.Fatalf("wizard failed: %v\noutput:\n%s", err, out.String())
	}

	for _, want := range []string{"Telegram bot token", "Admin Telegram user IDs", "Explicit chromium path", "Save this configuration"} {
		found := false
		for _, q := range p.asked {
			if strings.Contains(q, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("wizard never asked %q; asked: %v", want, p.asked)
		}
	}
	if cfg.Telegram.Token != "123456:AAHtest_token" {
		t.Fatalf("token = %q", cfg.Telegram.Token)
	}
	if len(cfg.Telegram.Admins) != 2 || cfg.Telegram.Admins[1] != 43 {
		t.Fatalf("admins = %v", cfg.Telegram.Admins)
	}
	if cfg.Browser.Executable != chrome {
		t.Fatalf("chromium = %q", cfg.Browser.Executable)
	}
	if cfg.Meet.PresentMode != "tab" || cfg.Meet.CaptureSource != "TeleMeet Media" {
		t.Fatalf("present mode = %+v", cfg.Meet)
	}
	if cfg.Limits.MaxSessions != 3 || cfg.Media.DefaultVolume != 55 {
		t.Fatalf("limits/media = %+v %+v", cfg.Limits, cfg.Media)
	}
	if cfg.Server.HealthAddr != "127.0.0.1:18099" || cfg.Paths.DataDir != dataDir {
		t.Fatalf("server/paths = %+v %+v", cfg.Server, cfg.Paths)
	}
	if !config.Exists(cfgPath) {
		t.Fatal("config file missing")
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Telegram.Token != cfg.Telegram.Token {
		t.Fatal("reloaded config does not match")
	}
	if !strings.Contains(out.String(), "config saved") {
		t.Fatalf("wizard output lacks confirmation:\n%s", out.String())
	}
}

func TestNonInteractiveWizardUsesDefaults(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("TELEMEETBOT_TOKEN", "999:noninteractive")
	chrome := fakeChrome(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")

	// Pre-seed the chromium path via an existing config so auto-detect is not needed.
	seed := config.Default()
	seed.Telegram.Token = "999:noninteractive"
	seed.Browser.Executable = chrome
	seed.Paths.DataDir = filepath.Join(dir, "data")
	if err := seed.Save(cfgPath); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cfg, err := Run(WizardOptions{
		ConfigPath:         cfgPath,
		In:                 strings.NewReader(""),
		Out:                &out,
		NonInteractive:     true,
		SkipBrowserInstall: true,
		TokenValidator:     func(string) (string, error) { return "bot", nil },
		Exec:               func(string, ...string) error { return nil },
	})
	if err != nil {
		t.Fatalf("wizard failed: %v\n%s", err, out.String())
	}
	if cfg.Telegram.Token != "999:noninteractive" {
		t.Fatalf("token = %q", cfg.Telegram.Token)
	}
	if !cfg.Telegram.FirstStartClaimsAdmin {
		t.Fatal("non-interactive setup must allow first-start claim with no admins")
	}
	if cfg.Limits.MaxSessions != config.Default().Limits.MaxSessions {
		t.Fatalf("defaults changed: %+v", cfg.Limits)
	}
}

func TestWizardRequiresToken(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	dir := t.TempDir()
	answers := []string{
		filepath.Join(dir, "c.yml"), // config path
		"",                          // token: empty => re-prompt, then EOF
	}
	var out bytes.Buffer
	_, err := Run(WizardOptions{
		In:                 strings.NewReader(strings.Join(answers, "\n") + "\n"),
		Out:                &out,
		SkipBrowserInstall: true,
		Exec:               func(string, ...string) error { return nil },
	})
	if err == nil {
		t.Fatalf("expected failure without a token; output:\n%s", out.String())
	}
}

func TestDoctorPassesWithFakeEnvironment(t *testing.T) {
	chrome := fakeChrome(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Telegram.Token = "1:doctor"
	cfg.Telegram.Admins = []int64{42}
	cfg.Browser.Executable = chrome
	cfg.Paths.DataDir = filepath.Join(dir, "data")
	cfg.Server.HealthAddr = "127.0.0.1:0"
	cfgPath := filepath.Join(dir, "config.yml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}

	rep := Doctor(cfg, cfgPath, DoctorOptions{
		SkipNetwork:    true,
		SkipMediaTools: true,
		Validate:       func(string) (string, error) { return "doctor_bot", nil },
	})
	if !rep.OK() {
		for _, c := range rep.Checks {
			t.Logf("%s ok=%t %s", c.Name, c.OK, c.Detail)
		}
		t.Fatal("doctor reported failures")
	}
	if len(rep.Checks) < 6 {
		t.Fatalf("expected at least 6 checks, got %d", len(rep.Checks))
	}
}

func TestWizardReportsInvalidToken(t *testing.T) {
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	dir := t.TempDir()
	var out bytes.Buffer
	calls := 0
	_, err := Run(WizardOptions{
		FindBrowser: browserNotFound,
		In: strings.NewReader(strings.Join([]string{
			filepath.Join(dir, "c.yml"),
			"1:bad",
			"1:good",
			"42",
			fakeChrome(t),
			"1",
			"2",
			"80",
			"127.0.0.1:0",
			filepath.Join(dir, "data"),
			"y",
			"n",
			"n",
		}, "\n") + "\n"),
		Out:                &out,
		Profile:            ProfileFull,
		SkipBrowserInstall: true,
		SkipMediaInstall:   true,
		TokenValidator: func(tok string) (string, error) {
			calls++
			if tok == "1:bad" {
				return "", errTestRejected
			}
			return "good_bot", nil
		},
		Exec: func(string, ...string) error { return nil },
	})
	if err != nil {
		t.Fatalf("wizard should retry after a bad token: %v\n%s", err, out.String())
	}
	if calls < 2 {
		t.Fatalf("token validator called %d times, expected retry", calls)
	}
	if !strings.Contains(out.String(), "❌") {
		t.Fatal("bad token was not reported to the operator")
	}
}

var errTestRejected = errString("telegram rejected the token")

type errString string

func (e errString) Error() string { return string(e) }

var browserNotFound = func(engine.BrowserConfig) (string, error) {
	return "", errors.New("no chromium found on this system")
}
