package config

import (
	"path/filepath"
	"testing"
)

// download.proxy must round-trip and be validated: a typo'd scheme would
// silently disable the only egress that works on a VPS.
func TestDownloadProxyRoundTripAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yml")
	c := Default()
	c.Download.Dir = filepath.Join(dir, "dl")
	c.Download.Proxy = "http://172.17.0.1:3339"
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Download.Proxy != c.Download.Proxy {
		t.Fatalf("round-trip lost download.proxy: %q", got.Download.Proxy)
	}
	for _, bad := range []string{"172.17.0.1:3339", "ftp://172.17.0.1:3339", "http://"} {
		c := Default()
		c.Download.Proxy = bad
		if err := c.Validate(); err == nil {
			t.Fatalf("download.proxy=%q should fail validation", bad)
		}
	}
}
