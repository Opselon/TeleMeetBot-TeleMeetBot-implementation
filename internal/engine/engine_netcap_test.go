package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/netcap"
)

// engine_netcap_test.go — regression tests for the adaptive network
// capability integration into the engine, the health surface and the doctor.
//
// The integration contract under test:
//
//  1. A manager always has a capability; asking it questions never blocks.
//  2. ObserveNetwork seeds the cache without delaying session start.
//  3. The configured egress ladder is what the capability probes, in
//     operator order, with the direct route always first.
//  4. An operator-configured download proxy is honoured as an explicit
//     preference, evidence or not.
//  5. A classified download failure invalidates the cache (recovery), while
//     a genuine extractor verdict does NOT.
//  6. The network summary never carries credentials.

// newNetcapTestServer builds a local HTTP server standing in for the YouTube
// probe endpoint plus the restore function that points netcap at it.
func newNetcapTestServer(t *testing.T, status int) (*httptest.Server, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	return srv, func() { srv.Close() }
}

// swapNetcapProbeTarget redirects the netcap probe target for the duration
// of a test and returns the restore function.
func swapNetcapProbeTarget(t *testing.T, url string) func() {
	t.Helper()
	return netcap.SetProbeURLForTest(url)
}

// A manager built by NewManager must always expose a usable capability, and
// every question the capability answers must return immediately — this is
// the "network detection never blocks meeting startup" guarantee.
func TestManagerExposesNonBlockingNetworkCapability(t *testing.T) {
	mgr := NewManager(BrowserConfig{PerSession: true}, MediaOptions{}, MeetOptions{GuestName: "T"}, dummyLogger{})
	if mgr.Netcap() == nil {
		t.Fatal("a manager must always carry a network capability")
	}
	done := make(chan struct{})
	go func() {
		_ = mgr.NetworkSummary()
		_ = mgr.Netcap().Direct()
		_ = mgr.Netcap().Recommend("", nil)
		mgr.ObserveNetwork(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("network capability questions must never block")
	}
}

// ObserveNetwork must seed the cache asynchronously: a caller that waits a
// bounded time sees a measurement, and a second immediate call does NOT
// launch a second probe (the cooldown collapses it).
func TestObserveNetworkSeedsCacheAsynchronously(t *testing.T) {
	srv, closeSrv := newNetcapTestServer(t, http.StatusOK)
	defer closeSrv()
	restore := swapNetcapProbeTarget(t, srv.URL)
	defer restore()

	mgr := NewManager(BrowserConfig{PerSession: true}, MediaOptions{}, MeetOptions{GuestName: "T"}, dummyLogger{})
	mgr.ObserveNetwork(context.Background())

	// ObserveNetwork returns immediately; the probe lands asynchronously.
	// Wait a bounded window, never sleep the full cooldown.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.Netcap().Direct().Outcome == netcap.OutcomeOK {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := mgr.Netcap().Direct().Outcome; got != netcap.OutcomeOK {
		t.Fatalf("the seeded capability must measure the direct route, got %v", got)
	}
}

// The capability ladder the engine builds must be the operator's download
// ladder, de-duplicated and in order, while the direct route stays the
// always-first leg that needs no configuration.
func TestEngineNetcapLadderMatchesConfiguredEgresses(t *testing.T) {
	mgr := NewManager(
		BrowserConfig{PerSession: true},
		MediaOptions{
			DownloadProxy:     "http://p1:3128",
			DownloadFallbacks: []string{"socks5://p2:9050", "http://p1:3128", "  "},
		},
		MeetOptions{GuestName: "T"}, dummyLogger{},
	)
	got := mgr.netcapProxies()
	want := []string{"http://p1:3128", "socks5://p2:9050"}
	if len(got) != len(want) {
		t.Fatalf("netcapProxies() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("netcapProxies()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// An explicit download proxy is the operator preference.
	if got := mgr.netcapPreferred(); got != "http://p1:3128" {
		t.Errorf("netcapPreferred() = %q, want the configured download proxy", got)
	}
	// And the preference is honoured by a recommendation even unprobed.
	rec := mgr.Netcap().Recommend(mgr.netcapPreferred(), mgr.netcapProxies())
	if rec.Route != netcap.RouteProxy || rec.Proxy != "http://p1:3128" {
		t.Errorf("operator-configured egress must win, got %v", rec)
	}
}

// A zero-config install has no proxies and no preference: the capability
// ladder is exactly [direct] and the recommendation is direct.
func TestZeroConfigNetcapLadderIsDirectOnly(t *testing.T) {
	mgr := NewManager(BrowserConfig{PerSession: true}, MediaOptions{}, MeetOptions{GuestName: "T"}, dummyLogger{})
	if got := mgr.netcapProxies(); len(got) != 0 {
		t.Errorf("a zero-config install must expose no proxy ladder, got %q", got)
	}
	if got := mgr.netcapPreferred(); got != "" {
		t.Errorf("a zero-config install must have no egress preference, got %q", got)
	}
	rec := mgr.Netcap().Recommend(mgr.netcapPreferred(), mgr.netcapProxies())
	if rec.Route != netcap.RouteDirect {
		t.Errorf("zero-config recommendation = %v, want direct", rec)
	}
}

// The network summary is what /healthz and the doctor print. It must carry
// the ladder, the outcomes and the selected route, and must never carry a
// credential. Proxy URLs are identifiers, so they appear; secrets do not.
func TestNetworkSummaryExposesLadderWithoutCredentials(t *testing.T) {
	srv, closeSrv := newNetcapTestServer(t, http.StatusOK)
	defer closeSrv()
	restore := swapNetcapProbeTarget(t, srv.URL)
	defer restore()

	mgr := NewManager(
		BrowserConfig{PerSession: true},
		MediaOptions{DownloadProxy: "http://user:pass@p:3128", DownloadFallbacks: []string{"socks5://tor:9050"}},
		MeetOptions{GuestName: "T"}, dummyLogger{},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = mgr.Netcap().WaitProbed(ctx, netcap.NewProbeConfig(mgr.netcapPreferred(), mgr.netcapProxies(), "test"))

	sum := mgr.NetworkSummary()
	if sum.Direct.Outcome == netcap.OutcomeUnknown {
		t.Fatal("the summary must report a measured direct route")
	}
	if len(sum.Proxies) != 2 {
		t.Fatalf("the summary must list both configured proxies, got %d", len(sum.Proxies))
	}
	// The proxy URL appears (it is an identifier), a credential must not.
	for _, p := range sum.Proxies {
		if strings.Contains(p.Proxy, "user:pass@") {
			t.Errorf("the summary must not carry credentials, found %q", p.Proxy)
		}
	}
	detail := formatEgressSummaryPublic(sum)
	if detail == "" {
		t.Error("the summary must render a non-empty diagnostics line")
	}
}

// formatEgressSummaryPublic mirrors the doctor renderer so the engine test
// can assert on the rendered shape without importing setup (which would be
// a cyclic dependency).
func formatEgressSummaryPublic(s netcap.Summary) string {
	var b strings.Builder
	b.WriteString("direct=")
	b.WriteString(s.Direct.Outcome.String())
	for _, p := range s.Proxies {
		b.WriteString(", ")
		b.WriteString(p.Proxy)
		b.WriteString("=")
		b.WriteString(p.Outcome.String())
	}
	return b.String()
}

// Recovery after network conditions change: when the download pipeline marks
// an egress blocked, the session must invalidate the cached capability so the
// next demand re-measures instead of trusting a stale verdict.
func TestDownloadFailureInvalidatesStaleCapability(t *testing.T) {
	mgr := NewManager(
		BrowserConfig{PerSession: true},
		MediaOptions{DownloadEnabled: true, DownloadBaseDir: t.TempDir()},
		MeetOptions{GuestName: "T"}, dummyLogger{},
	)
	if mgr.mediaMgr == nil {
		t.Fatal("this test needs a real media manager")
	}
	c := mgr.Netcap()

	// Seed the capability with a "known good" reading: a completed probe that
	// found the direct route OK.
	now := time.Now()
	c.SetForTest(func() time.Time { return now }, netcap.Result{Route: netcap.RouteDirect, Outcome: netcap.OutcomeOK, CheckedAt: now})
	if c.Direct().Outcome != netcap.OutcomeOK {
		t.Fatal("the seeded reading must be visible")
	}

	// A download failure with NO egress blocked (a genuine extractor verdict)
	// must NOT invalidate: nothing about the network changed.
	s := &Session{mgr: mgr, log: mgr.log}
	s.invalidateNetworkCapability()
	if c.Direct().Outcome != netcap.OutcomeOK {
		t.Error("a genuine extractor verdict must not invalidate the capability")
	}

	// Now the pipeline marks an egress blocked (the classified failure) and
	// the same hook must drop the stale reading.
	mgr.mediaMgr.MarkEgressBlocked("http://p:3128")
	s.invalidateNetworkCapability()
	if c.Direct().Outcome != netcap.OutcomeUnknown {
		t.Errorf("an egress-shaped failure must invalidate the cache, got %v", c.Direct().Outcome)
	}
}

// Concurrent sessions with different network outcomes must not race on the
// shared capability: two sessions can observe and invalidate simultaneously.
func TestConcurrentSessionsShareCapabilitySafely(t *testing.T) {
	srv, closeSrv := newNetcapTestServer(t, http.StatusOK)
	defer closeSrv()
	restore := swapNetcapProbeTarget(t, srv.URL)
	defer restore()

	mgr := NewManager(BrowserConfig{PerSession: true}, MediaOptions{DownloadEnabled: true, DownloadBaseDir: t.TempDir()}, MeetOptions{GuestName: "T"}, dummyLogger{})
	s := &Session{mgr: mgr, log: mgr.log}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mgr.ObserveNetwork(context.Background())
			_ = mgr.NetworkSummary()
			if i%2 == 0 {
				s.invalidateNetworkCapability()
			}
		}(i)
	}
	wg.Wait()
}
