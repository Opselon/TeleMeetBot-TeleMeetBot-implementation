package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() *Config {
	c := Default()
	c.Telegram.Token = "123456:AAHtesttoken"
	c.Telegram.Admins = []int64{5094837833}
	return c
}

func TestDefaultIsValidWithToken(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
}

func TestValidateCatchesProblems(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"no token", func(c *Config) { c.Telegram.Token = "" }, "telegram.token is empty"},
		{"malformed token", func(c *Config) { c.Telegram.Token = "nope" }, "does not look like"},
		{"no way to be admin", func(c *Config) {
			c.Telegram.Admins = nil
			c.Telegram.FirstStartClaimsAdmin = false
		}, "nobody could control the bot"},
		{"bad present mode", func(c *Config) { c.Meet.PresentMode = "window" }, `must be "tab" or "screen"`},
		{"tab mode with screen source", func(c *Config) {
			c.Meet.CaptureSource = "Entire screen (monitor 1)"
		}, "looks like a screen source"},
		{"volume", func(c *Config) { c.Media.DefaultVolume = 140 }, "0..100"},
		{"speed", func(c *Config) { c.Media.DefaultSpeed = 9 }, "0.25..4"},
		{"sessions", func(c *Config) { c.Limits.MaxSessions = 0 }, "1..8"},
		{"data dir", func(c *Config) { c.Paths.DataDir = " " }, "paths.data_dir is empty"},
		{"google auth enabled missing client_id", func(c *Config) {
			c.Google.Enabled = true
			c.Google.ClientSecret = "secret"
			c.Google.RedirectURL = "http://localhost:18080/auth/google/callback"
		}, "google.client_id is required"},
		{"google auth enabled missing client_secret", func(c *Config) {
			c.Google.Enabled = true
			c.Google.ClientID = "client-id"
			c.Google.RedirectURL = "http://localhost:18080/auth/google/callback"
		}, "google.client_secret is required"},
		{"google auth enabled missing redirect_url", func(c *Config) {
			c.Google.Enabled = true
			c.Google.ClientID = "client-id"
			c.Google.ClientSecret = "secret"
		}, "google.redirect_url is required"},
		{"google auth enabled invalid redirect_url", func(c *Config) {
			c.Google.Enabled = true
			c.Google.ClientID = "client-id"
			c.Google.ClientSecret = "secret"
			c.Google.RedirectURL = "ftp://invalid-url"
		}, "google.redirect_url must be a valid http or https URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "telemeetbot.yml")
	c := validConfig()
	c.Meet.GuestName = "Stream Bot"
	c.Browser.ExtraArgs = []string{"mute-audio"}
	c.Media.DefaultVolume = 42
	if err := c.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config permissions = %o, want 600", perm)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Telegram.Token != c.Telegram.Token || got.Meet.GuestName != "Stream Bot" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Media.DefaultVolume != 42 || len(got.Browser.ExtraArgs) != 1 {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if got.Path() != path {
		t.Fatalf("Path() = %q", got.Path())
	}
}

func TestLoadKeepsDefaultsForOmittedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "min.yml")
	body := "telegram:\n  token: \"1:abc\"\n  admins: [42]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Limits.MaxSessions != Default().Limits.MaxSessions {
		t.Fatalf("defaults not preserved: %d", c.Limits.MaxSessions)
	}
	if c.Meet.PresentMode != "tab" || c.Meet.CaptureSource != "TeleMeet Media" {
		t.Fatalf("meet defaults wrong: %+v", c.Meet)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("minimal config should validate: %v", err)
	}
}

func TestClaimAdmin(t *testing.T) {
	c := Default()
	c.Telegram.Token = "1:abc"
	if !c.ClaimAdmin(7) {
		t.Fatal("first claim should succeed")
	}
	if !c.IsAdmin(7) || c.IsAdmin(8) {
		t.Fatal("claim did not stick")
	}
	if c.ClaimAdmin(8) {
		t.Fatal("second claim must be refused")
	}

	c2 := Default()
	c2.Telegram.Token = "1:abc"
	c2.Telegram.Admins = []int64{1}
	if c2.ClaimAdmin(2) {
		t.Fatal("claim must be refused when admins already exist")
	}

	c3 := Default()
	c3.Telegram.FirstStartClaimsAdmin = false
	if c3.ClaimAdmin(3) {
		t.Fatal("claim must be refused when disabled")
	}
}

func TestResolvePathEnvWins(t *testing.T) {
	t.Setenv("TELEMEETBOT_CONFIG", "/tmp/custom-config.yml")
	if got := ResolvePath(""); got != "/tmp/custom-config.yml" {
		t.Fatalf("ResolvePath() = %q", got)
	}
	if got := ResolvePath("/explicit.yml"); got != "/explicit.yml" {
		t.Fatalf("explicit path ignored: %q", got)
	}
}

func TestTokenPreview(t *testing.T) {
	c := Default()
	if c.TokenPreview() != "(unset)" {
		t.Fatalf("unset preview = %q", c.TokenPreview())
	}
	c.Telegram.Token = "123456:secret"
	if c.TokenPreview() != "123456:***" {
		t.Fatalf("preview = %q", c.TokenPreview())
	}
	if strings.Contains(c.TokenPreview(), "secret") {
		t.Fatal("preview leaks the secret")
	}
}
