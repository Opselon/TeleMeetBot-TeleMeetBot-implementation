package config

import (
	"testing"
)

// The default download policy must be ON with a mandatory one-hour TTL: that
// is the production policy the feature is built around.
func TestDownloadDefaults(t *testing.T) {
	c := Default()
	if !c.Download.Enabled {
		t.Fatal("download.enabled should default to true")
	}
	if c.Download.TTLMinutes != 60 {
		t.Fatalf("default ttl_minutes = %d, want 60", c.Download.TTLMinutes)
	}
	if c.Download.MaxSizeMB != 2048 {
		t.Fatalf("default max_size_mb = %d, want 2048", c.Download.MaxSizeMB)
	}
	if c.Download.TimeoutSeconds != 600 {
		t.Fatalf("default timeout_seconds = %d, want 600", c.Download.TimeoutSeconds)
	}
	if !c.Download.AllowTranscode {
		t.Fatal("allow_transcode should default to true")
	}
}

// Round-trip: a written config must load back with the download section intact.
func TestDownloadRoundTrip(t *testing.T) {
	c := Default()
	c.Telegram.Token = "123456:AAHtest"
	c.Paths.DataDir = t.TempDir()
	c.Download.TTLMinutes = 90
	c.Download.MaxSizeMB = 512
	path := c.Paths.DataDir + "/telemeetbot.yml"
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Download.TTLMinutes != 90 {
		t.Fatalf("ttl_minutes round-trip: got %d", loaded.Download.TTLMinutes)
	}
	if loaded.Download.MaxSizeMB != 512 {
		t.Fatalf("max_size_mb round-trip: got %d", loaded.Download.MaxSizeMB)
	}
	if !loaded.Download.Enabled {
		t.Fatal("enabled flag lost in the round-trip")
	}
}

// Validation must reject a TTL over 24h and a download.dir pointing at a
// system directory.
func TestDownloadValidation(t *testing.T) {
	c := Default()
	c.Telegram.Token = "123456:AAHtest"
	c.Paths.DataDir = t.TempDir()
	c.Download.TTLMinutes = 2000
	if err := c.Validate(); err == nil {
		t.Fatal("a >24h TTL should fail validation")
	}
	c.Download.TTLMinutes = 60
	c.Download.Dir = "/etc"
	if err := c.Validate(); err == nil {
		t.Fatal("download.dir=/etc should fail validation")
	}
	c.Download.Dir = "/tmp/dl"
	if err := c.Validate(); err != nil {
		t.Fatalf("a sane download dir should validate: %v", err)
	}
	// A disabled feature skips the checks entirely.
	c.Download.Enabled = false
	c.Download.TTLMinutes = 99999
	if err := c.Validate(); err != nil {
		t.Fatalf("a disabled download section should not validate-fail: %v", err)
	}
}
