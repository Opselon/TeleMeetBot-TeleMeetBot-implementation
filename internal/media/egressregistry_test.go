package media

import (
	"testing"
	"time"
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
	if !m.EgressBlocked("") {
		t.Fatal("the empty (direct) egress must report blocked when marked")
	}
	if got := len(m.BlockedEgresses()); got != 3 {
		t.Fatalf("marking the empty proxy must record direct egress, got %d entries", got)
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

// A rotating egress (Tor, a rotating residential pool) is the reason the
// registry must expire: the refusal came from ONE exit circuit, and the next
// run lands on a different IP that YouTube has never seen. A permanent mark
// would retire the whole proxy for one transient 403, and every later session
// would avoid the one route that actually works. After egressBlockTTL the
// entry must drop out of both the lookup and the snapshot.
func TestEgressBlockedRegistryExpires(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(DefaultOptions(dir), &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	now := time.Now()
	m.egressNow = func() time.Time { return now }

	m.MarkEgressBlocked("socks5://172.17.0.1:9050")
	if !m.EgressBlocked("socks5://172.17.0.1:9050") {
		t.Fatal("a fresh mark must report blocked")
	}
	if got := len(m.BlockedEgresses()); got != 1 {
		t.Fatalf("a fresh mark must appear in the snapshot, got %d", got)
	}

	// Advance past the deadline: the route is usable again.
	now = now.Add(egressBlockTTL + time.Second)
	if m.EgressBlocked("socks5://172.17.0.1:9050") {
		t.Fatal("an expired mark must stop reporting blocked")
	}
	if got := len(m.BlockedEgresses()); got != 0 {
		t.Fatalf("an expired mark must drop out of the snapshot, got %d", got)
	}

	// Re-marking after expiry is a new entry, not a resurrection of the old
	// one: a route that lost again gets a fresh deadline.
	m.MarkEgressBlocked("socks5://172.17.0.1:9050")
	if !m.EgressBlocked("socks5://172.17.0.1:9050") {
		t.Fatal("a re-mark after expiry must report blocked again")
	}
}

// Repeated refusals inside the TTL window must not extend the deadline
// silently — the operator's log line should not repeat, and the original
// deadline stands.
func TestEgressBlockedRegistryRenamesAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(DefaultOptions(dir), &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	now := time.Now()
	m.egressNow = func() time.Time { return now }

	m.MarkEgressBlocked("http://172.17.0.1:3339")
	first := m.blockedEgresses["http://172.17.0.1:3339"]
	m.MarkEgressBlocked("http://172.17.0.1:3339")
	if got := m.blockedEgresses["http://172.17.0.1:3339"]; got != first {
		t.Fatalf("a live mark must keep its original deadline: first=%v again=%v", first, got)
	}
	if got := len(m.BlockedEgresses()); got != 1 {
		t.Fatalf("redundant marks must stay one entry, got %d", got)
	}
}
