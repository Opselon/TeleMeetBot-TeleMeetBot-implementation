package engine

import (
	"os"
	"testing"
)

// An explicitly empty browser.proxy is the operator's "direct" choice. The
// ambient Tor listener (present on this host) must never silently override
// it: that hijacks every browser tab onto the slowest route while the session
// keeps working, which is exactly the stable-but-slow misconfiguration the
// egress benchmark on 2026-11-05 uncovered (Tor 0.4-2.8 MB/s vs direct
// 165-232 MB/s).
func TestEmptyBrowserProxyIsDirectEvenWithAmbientTor(t *testing.T) {
	if host := os.Getenv("TELEMEETBOT_PROXY"); host != "" {
		t.Skipf("TELEMEETBOT_PROXY=%q is set; ambient detection cannot be isolated", host)
	}
	// Simulate the ambient listener: detectProxy probes 127.0.0.1:9050 first.
	// Without a listener present this still proves the empty config is kept
	// rather than replaced; with one present (this host) it proves the
	// explicit empty choice wins over the probe.
	t.Setenv("TELEMEETBOT_PROXY", "")
	if got := detectProxy(BrowserConfig{Proxy: ""}); got != "" {
		t.Fatalf("empty browser.proxy must stay direct, got ambient proxy %q", got)
	}
}

// An explicit proxy is used verbatim and never shadowed by the ambient probe.
func TestExplicitBrowserProxyWinsOverAmbient(t *testing.T) {
	t.Setenv("TELEMEETBOT_PROXY", "")
	if got := detectProxy(BrowserConfig{Proxy: "http://172.17.0.1:3339"}); got != "http://172.17.0.1:3339" {
		t.Fatalf("explicit proxy must win, got %q", got)
	}
}

// TELEMEETBOT_PROXY continues to override an unset browser.proxy so the env
// documented escape hatch is honoured.
func TestEnvProxyAppliesWhenConfigUnset(t *testing.T) {
	t.Setenv("TELEMEETBOT_PROXY", "http://10.0.0.1:8080")
	if got := detectProxy(BrowserConfig{Proxy: ""}); got != "http://10.0.0.1:8080" {
		t.Fatalf("TELEMEETBOT_PROXY must apply when config is unset, got %q", got)
	}
}
