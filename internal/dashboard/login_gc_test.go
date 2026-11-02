package dashboard

import (
	"fmt"
	"testing"
	"time"
)

// TestLoginFailuresMapBounded is the regression test for the login rate
// limiter's state. loginAllowed trimmed an IP's timestamps but never removed
// IPs that stopped hitting the endpoint, so the map only grew: an
// unauthenticated client rotating source IPs (NAT pools, IPv6 rotation, a
// botnet, or just a fleet of health checkers) turned it into a slow memory
// leak on a process meant to run unattended for months.
//
// Two bounds now apply: a GC drops IPs with no attempt inside the window,
// and a hard cap evicts the oldest entries past maxLoginFailureIPs — which
// can only relieve a limit that has already expired, never an active one.
// Both reclaims are amortized, so a login storm cannot make the hot path
// O(n^2).
func TestLoginFailuresMapBounded(t *testing.T) {
	s, _ := testServer(t)

	// A rotating attacker: twice the hard cap in distinct IPs, one failure
	// each. The login handler itself stays cheap because the eviction is
	// amortized; the sweep then must bring the map back under the cap.
	for i := 0; i < maxLoginFailureIPs*2; i++ {
		s.recordLoginFailure(ipOf(i))
	}
	if got := len(s.failures); got < maxLoginFailureIPs {
		// Sanity: without the cap this loop reaches 2x the cap.
		t.Fatalf("expected the rotation to reach past the cap before the sweep, map = %d", got)
	}
	// Then exercise the CAP on its own, with entries the quiet sweep cannot
	// touch: an IP at maxLoginFailures attempts is inside its window, so the
	// only reclaim that can bring the map back under the cap is the eviction
	// itself. This is the invariant that fails without the cap.
	for i := 0; i < maxLoginFailureIPs*2; i++ {
		ip := ipOf(1_000_000 + i)
		for k := 0; k < maxLoginFailures; k++ {
			s.recordLoginFailure(ip)
		}
	}
	s.mu.Lock()
	s.lastEvict = time.Time{} // release the amortization gate
	s.mu.Unlock()
	s.evictOldestFailuresLocked(time.Now())
	if got := len(s.failures); got > maxLoginFailureIPs {
		t.Fatalf("failure map = %d entries with active limits, want at most %d — the hard cap is not enforced", got, maxLoginFailureIPs)
	}
}

// TestLoginFailuresGCHotPathCheap pins the amortization: a login storm must
// not pay an O(n) sweep per request. Before the amortization, the eviction
// ran on every failure once past the cap and 8192 login attempts cost ~10s —
// a self-inflicted denial of service on the login path itself.
func TestLoginFailuresGCHotPathCheap(t *testing.T) {
	s, _ := testServer(t)

	// Prime the map past the cap so the eviction branch is live.
	for i := 0; i < maxLoginFailureIPs+8; i++ {
		s.recordLoginFailure(ipOf(i))
	}

	start := time.Now()
	for i := 0; i < 1000; i++ {
		s.recordLoginFailure(ipOf(maxLoginFailureIPs + i))
	}
	elapsed := time.Since(start)
	// 1000 amortized appends must stay far below the ~10s the per-call
	// eviction cost. The bound is generous so slow CI machines stay green.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("1000 login failures after the cap took %s — the eviction is not amortized", elapsed.Round(time.Millisecond))
	}
}

// TestLoginFailuresGCReclaimsQuietIPs: the sweep drops IPs whose last
// attempt is older than the window, so quiet entries are reclaimed instead
// of accumulating for the process lifetime.
func TestLoginFailuresGCReclaimsQuietIPs(t *testing.T) {
	s, _ := testServer(t)

	s.recordLoginFailure("10.0.0.1")
	s.recordLoginFailure("10.0.0.2")
	if len(s.failures) != 2 {
		t.Fatalf("setup: map has %d entries, want 2", len(s.failures))
	}

	// An hour later — far past loginFailureWindow (15m) — the sweep reclaims
	// both, and a fresh attempt is allowed again.
	later := time.Now().Add(time.Hour)
	s.gcLoginFailures(later)
	if len(s.failures) != 0 {
		t.Fatalf("gc left %d entries for quiet IPs, want 0: %v", len(s.failures), s.failures)
	}
	if !s.loginAllowed("10.0.0.1") {
		t.Fatal("an IP whose failures are long expired is still rate-limited")
	}
}

// TestLoginFailuresGCKeepsActiveIPLimited: the sweep must not reclaim an
// entry that is still inside its window, and an IP at its limit stays
// limited after the sweep runs.
func TestLoginFailuresGCKeepsActiveIPLimited(t *testing.T) {
	s, _ := testServer(t)

	for i := 0; i < maxLoginFailures; i++ {
		s.recordLoginFailure("10.0.0.9")
	}
	if s.loginAllowed("10.0.0.9") {
		t.Fatal("an IP at its failure limit is not rate-limited")
	}
	s.gcLoginFailures(time.Now())
	if s.loginAllowed("10.0.0.9") {
		t.Fatal("the GC cleared an active limit")
	}
}

// ipOf synthesizes a distinct IPv4 address for an index.
func ipOf(i int) string {
	return fmt.Sprintf("%d.%d.%d.%d", 1+(i>>24)%254, (i>>16)%256, (i>>8)%256, i%256)
}
