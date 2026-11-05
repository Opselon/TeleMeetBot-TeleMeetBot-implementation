package engine

import (
	"os"
	"testing"
)

// The "direct" sentinel forces an unproxied launch even when an ambient
// SOCKS5 listener (Tor) is reachable: an empty browser.proxy cannot express
// that, because it is indistinguishable from an unset key and the ambient
// probe wins. On this host Tor is 60-500x slower than direct (2026-11-05
// benchmark: Tor 0.4-2.8 MB/s vs direct 165-232 MB/s), so the sentinel is
// the only way to keep the browser off Tor.
func TestDirectSentinelDefeatsAmbientTor(t *testing.T) {
	if host := os.Getenv("TELEMEETBOT_PROXY"); host != "" {
		t.Skipf("TELEMEETBOT_PROXY=%q is set; ambient detection cannot be isolated", host)
	}
	for _, sentinel := range []string{"direct", "direct://", " direct ", "DIRECT", "Direct://"} {
		if got := detectProxy(BrowserConfig{Proxy: sentinel}); got != "" {
			t.Fatalf("browser.proxy %q must force a direct launch, got %q", sentinel, got)
		}
	}
}

// An empty or unset browser.proxy falls through to the ambient SOCKS5 probe,
// because the empty string cannot express "direct": this host runs a Tor
// listener that is 60-500x slower than the direct route, and the probe
// silently hijacks every browser tab onto it. Operators who want direct use
// the "direct" sentinel (TestDirectSentinelDefeatsAmbientTor).
func TestEmptyBrowserProxyFallsThroughToAmbientProbe(t *testing.T) {
	if host := os.Getenv("TELEMEETBOT_PROXY"); host != "" {
		t.Skipf("TELEMEETBOT_PROXY=%q is set; ambient detection cannot be isolated", host)
	}
	// Without an ambient listener the answer is "" (direct); with one present
	// (this host) it is the listener. Both are correct: the probe is the
	// documented fallback for an unset proxy, never a silent override of an
	// explicit choice.
	got := detectProxy(BrowserConfig{Proxy: ""})
	if got != "" && !startsWith(got, "socks5://") {
		t.Fatalf("empty browser.proxy must yield direct or an ambient socks5 proxy, got %q", got)
	}
}

// An explicit proxy is used verbatim and never shadowed by the ambient probe.
func TestExplicitBrowserProxyWinsOverAmbient(t *testing.T) {
	t.Setenv("TELEMEETBOT_PROXY", "")
	if got := detectProxy(BrowserConfig{Proxy: "http://172.17.0.1:3339"}); got != "http://172.17.0.1:3339" {
		t.Fatalf("explicit proxy must win, got %q", got)
	}
}

// TELEMEETBOT_PROXY applies when browser.proxy is unset, but a "direct"
// sentinel in the config still wins: the operator's explicit choice outranks
// an environment default.
func TestEnvProxyAppliesWhenConfigUnset(t *testing.T) {
	t.Setenv("TELEMEETBOT_PROXY", "http://10.0.0.1:8080")
	if got := detectProxy(BrowserConfig{Proxy: ""}); got != "http://10.0.0.1:8080" {
		t.Fatalf("TELEMEETBOT_PROXY must apply when config is unset, got %q", got)
	}
	if got := detectProxy(BrowserConfig{Proxy: "direct"}); got != "" {
		t.Fatalf("config direct sentinel must outrank TELEMEETBOT_PROXY, got %q", got)
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
