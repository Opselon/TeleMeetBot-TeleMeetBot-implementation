package media

import (
	"net"
	"testing"
)

// TestIsBlockedIP pins the SSRF gate used by both the downloader and the
// engine's classification probe: every private/loopback/link-local range is
// refused, and unknown input is treated as blocked (fail-closed).
func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.4", "172.16.0.2", "192.168.1.1", "169.254.169.254",
		"0.0.0.0", "::1", "fe80::1", "fd00::1", "100.64.0.1",
	}
	for _, s := range blocked {
		if !IsBlockedIP(net.ParseIP(s)) {
			t.Fatalf("IsBlockedIP(%q) = false, want true", s)
		}
	}
	// A public address is not blocked.
	if IsBlockedIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("IsBlockedIP(8.8.8.8) = true, want false")
	}
	// Unknown input is blocked, never allowed.
	if !IsBlockedIP(nil) {
		t.Fatal("IsBlockedIP(nil) = false, want true")
	}
}

// TestIsPrivateIPExported keeps the exported gate and its internal alias in
// lockstep: one must not drift from the other.
func TestIsPrivateIPExported(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "::1"} {
		ip := net.ParseIP(s)
		if IsPrivateIP(ip) != isPrivateIP(ip) {
			t.Fatalf("IsPrivateIP(%q) and isPrivateIP(%q) disagree", s, s)
		}
	}
}
