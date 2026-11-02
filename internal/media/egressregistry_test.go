package media

import (
	"testing"
)

// The registry is the bridge between the download ladder and the media
// browser: fetchYTDLP records every egress the source refused, and the engine
// launches the media tab on a route that is not in this set. Its semantics
// must stay boring and safe:
//
//   - marking is idempotent and never grows unbounded,
//   - a direct connection (the empty proxy) is never "blocked", because there
//     is no alternative to direct and the answer must not become a launch
//     killer,
//   - a nil manager answers false, so callers that run with Download & Stream
//     disabled never panic.
func TestEgressBlockedRegistry(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(DefaultOptions(dir), &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	if m.EgressBlocked("socks5://172.17.0.1:9050") {
		t.Fatal("an unmarked egress must not report blocked")
	}
	// Repeated marks are one entry, not many.
	m.MarkEgressBlocked("socks5://172.17.0.1:9050")
	m.MarkEgressBlocked("socks5://172.17.0.1:9050")
	if !m.EgressBlocked("socks5://172.17.0.1:9050") {
		t.Fatal("a marked egress must report blocked")
	}
	if got := len(m.BlockedEgresses()); got != 1 {
		t.Fatalf("duplicate marks must collapse to one entry, got %d", got)
	}

	m.MarkEgressBlocked("http://172.17.0.1:3339")
	if !m.EgressBlocked("http://172.17.0.1:3339") {
		t.Fatal("a second marked egress must report blocked")
	}
	if got := len(m.BlockedEgresses()); got != 2 {
		t.Fatalf("expected two blocked egresses, got %d", got)
	}

	// "Direct" can never be blocked: there is no alternative route, so the
	// answer must never be used to fail a browser launch.
	m.MarkEgressBlocked("")
	if m.EgressBlocked("") {
		t.Fatal("the empty (direct) egress must never report blocked")
	}
	if got := len(m.BlockedEgresses()); got != 2 {
		t.Fatalf("marking the empty proxy must be a no-op, got %d entries", got)
	}
}

// A nil manager is the Download & Stream disabled state: every lookup must
// answer false instead of panicking, because the engine consults this
// registry on the browser launch path where a nil media manager is normal.
func TestEgressBlockedNilManager(t *testing.T) {
	var m *Manager
	if m.EgressBlocked("socks5://172.17.0.1:9050") {
		t.Fatal("nil manager must not report an egress blocked")
	}
	if m.EgressBlocked("") {
		t.Fatal("nil manager must not report direct blocked")
	}
	if got := m.BlockedEgresses(); got != nil {
		t.Fatalf("nil manager must report no blocked egresses, got %q", got)
	}
	m.MarkEgressBlocked("socks5://172.17.0.1:9050") // must not panic
}
