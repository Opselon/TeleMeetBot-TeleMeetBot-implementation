package netcap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Test infrastructure ---------------------------------------------------

// countingServer records how many probe requests reached it so tests can
// assert that probes were launched — or were NOT.
type countingServer struct {
	hits atomic.Int64
}

func (s *countingServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// -----------------------------------------------------------------------------
// Zero state: the component must be safe before any probe ran.
// -----------------------------------------------------------------------------

// The meeting-critical path never waits on netcap: a brand-new Capability
// must answer instantly, with an OutcomeUnknown that justifies neither using
// nor avoiding any route.
func TestZeroValueIsUnknownAndNeverBlocks(t *testing.T) {
	c := New()
	done := make(chan struct{})
	go func() {
		_ = c.Recommend("", nil)
		_ = c.Direct()
		_ = c.Proxy("http://p:3128")
		_ = c.Summarize("", nil)
		_ = c.String("", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a never-probed Capability must answer immediately")
	}

	if got := c.Direct().Outcome; got != OutcomeUnknown {
		t.Errorf("unprobed direct outcome = %v, want unknown", got)
	}
	rec := c.Recommend("", nil)
	if rec.Route != RouteDirect {
		t.Errorf("unprobed recommendation = %v, want direct (no evidence against it)", rec)
	}
	// A preferred proxy the operator configured always stands, even unprobed.
	rec = c.Recommend("socks5://127.0.0.1:9050", nil)
	if rec.Route != RouteProxy || rec.Proxy != "socks5://127.0.0.1:9050" {
		t.Errorf("operator preference must win unprobed, got %v", rec)
	}
	// A blank preference is not a preference at all.
	rec = c.Recommend("   ", nil)
	if rec.Route != RouteDirect {
		t.Errorf("a blank preference must not select a proxy, got %v", rec)
	}
}

// -----------------------------------------------------------------------------
// Classification: the failure taxonomy is the heart of the component.
// -----------------------------------------------------------------------------

func TestClassifyTransportErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		want   Outcome
		reason string
	}{
		{"dns failure", fmt.Errorf("dial tcp: lookup nope.invalid: no such host"), OutcomeUnreachable, "DNS resolution failed"},
		{"connection refused", fmt.Errorf("dial tcp 127.0.0.1:1: connect: connection refused"), OutcomeUnreachable, "connection refused"},
		{"connection reset", fmt.Errorf("read: connection reset by peer"), OutcomeUnreachable, "connection reset by peer"},
		{"tls eof", fmt.Errorf("tls: handshake failure: EOF"), OutcomeUnreachable, "TLS/SSL connection has been closed (EOF)"},
		{"timeout", fmt.Errorf("context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), OutcomeUnreachable, "timeout"},
		{"deadline exceeded", fmt.Errorf("Get https://x: i/o timeout"), OutcomeUnreachable, "timeout"},
		{"no route", fmt.Errorf("dial: no route to host"), OutcomeUnreachable, "no route to host"},
		{"network unreachable", fmt.Errorf("dial: network is unreachable"), OutcomeUnreachable, "no route to host"},
		{"proxy unusable", fmt.Errorf("proxyconnect tcp: dial tcp: proxy refused"), OutcomeUnreachable, "proxy unusable"},
		{"generic", fmt.Errorf("something odd happened"), OutcomeUnreachable, "transport error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, status, reason := classify(nil, tc.err)
			if out != tc.want {
				t.Errorf("outcome = %v, want %v", out, tc.want)
			}
			if status != 0 {
				t.Errorf("status must be 0 on transport error, got %d", status)
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

func TestClassifyHTTPStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   Outcome
	}{
		{200, OutcomeOK},
		{204, OutcomeOK},
		{301, OutcomeOK}, // 3xx proves the transport works; no redirect is followed
		{302, OutcomeOK},
		{400, OutcomeUnknown},
		{403, OutcomeBlocked},
		{404, OutcomeUnknown},
		{418, OutcomeUnknown},
		{429, OutcomeBlocked},
		{500, OutcomeUnknown},
		{502, OutcomeUnknown},
		{503, OutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status %d", tc.status), func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Body: http.NoBody}
			out, status, _ := classify(resp, nil)
			if out != tc.want {
				t.Errorf("status %d -> outcome %v, want %v", tc.status, out, tc.want)
			}
			if status != tc.status {
				t.Errorf("reported status = %d, want %d", status, tc.status)
			}
		})
	}
}

// A nil response with no error is an impossible-but-defensive state: it must
// surface unknown rather than crash.
func TestClassifyNilResponseNoError(t *testing.T) {
	out, status, reason := classify(nil, nil)
	if out != OutcomeUnknown || status != 0 || reason == "" {
		t.Errorf("nil response without error must be unknown with a reason, got (%v,%d,%q)", out, status, reason)
	}
}

// Reasons surface in /healthz, which anyone can read: they must never carry
// a proxy URL, a credential, or an internal address.
func TestReasonsCarryNoCredentialsOrAddresses(t *testing.T) {
	errs := []error{
		fmt.Errorf("Get https://secret-user:hunter2@proxy.example:3128: dial tcp: no such host"),
		fmt.Errorf("proxyconnect socks5://10.0.0.5:1080: connection refused"),
		fmt.Errorf("timeout calling https://www.youtube.com/"),
	}
	for _, e := range errs {
		_, _, reason := classify(nil, e)
		for _, banned := range []string{"hunter2", "secret-user", "10.0.0.5", "www.youtube.com", "proxy.example:3128"} {
			if stringsContains(reason, banned) {
				t.Errorf("reason %q leaks %q", reason, banned)
			}
		}
	}
}

// Proxy identifiers surface in /healthz too, so a configured proxy URL that
// carries credentials must be reduced to its identifier form the moment it
// enters a Result. The unredacted URL is still used to dial; only what is
// STORED is stripped.
func TestProxyCredentialsAreStrippedFromResults(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://user:pass@p:3128", "http://p:3128"},
		{"socks5://user:pass@127.0.0.1:9050", "socks5://127.0.0.1:9050"},
		{"http://p:3128", "http://p:3128"},
		{"socks5://127.0.0.1:9050", "socks5://127.0.0.1:9050"},
		{"", ""},
		{"not-a-url", "not-a-url"},
	}
	for _, tc := range cases {
		if got := stripProxyCredentials(tc.in); got != tc.want {
			t.Errorf("stripProxyCredentials(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// And the summary (the /healthz surface) never carries credentials even
	// when the operator configured them.
	c := New()
	now := time.Now()
	c.now = func() time.Time { return now }
	s := c.Summarize("", []string{"http://user:pass@p:3128"})
	for _, p := range s.Proxies {
		if stringsContains(p.Proxy, "user:pass@") {
			t.Errorf("summary carried credentials: %q", p.Proxy)
		}
	}
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Recommendation semantics
// -----------------------------------------------------------------------------

// The recommendation must never promote a proxy above a working direct route
// — that is the zero-config promise — and must never keep recommending a
// route that no longer has a fresh observation.
func TestRecommendPreferenceOrder(t *testing.T) {
	t.Run("fresh direct ok beats configured proxies", func(t *testing.T) {
		c := New()
		now := time.Now()
		c.now = func() time.Time { return now }
		c.direct = Result{Outcome: OutcomeOK, CheckedAt: now}
		c.proxies = map[string]Result{"http://p:3128": {Outcome: OutcomeOK, CheckedAt: now}}
		rec := c.Recommend("", []string{"http://p:3128"})
		if rec.Route != RouteDirect {
			t.Errorf("fresh OK direct must win over a proxy, got %v", rec)
		}
	})

	t.Run("blocked direct falls back to a fresh OK proxy", func(t *testing.T) {
		c := New()
		now := time.Now()
		c.now = func() time.Time { return now }
		c.direct = Result{Outcome: OutcomeBlocked, CheckedAt: now, Reason: "bot check"}
		c.proxies = map[string]Result{
			"http://dead:3128": {Outcome: OutcomeUnreachable, CheckedAt: now},
			"http://good:3128": {Outcome: OutcomeOK, CheckedAt: now},
		}
		rec := c.Recommend("", []string{"http://dead:3128", "http://good:3128"})
		if rec.Route != RouteProxy || rec.Proxy != "http://good:3128" {
			t.Errorf("expected the first OK proxy, got %v", rec)
		}
		if !stringsContains(rec.Reason, "blocked") {
			t.Errorf("the fallback reason must explain why direct lost: %q", rec.Reason)
		}
	})

	t.Run("blocked direct prefers the FIRST ok proxy in operator order", func(t *testing.T) {
		c := New()
		now := time.Now()
		c.now = func() time.Time { return now }
		c.direct = Result{Outcome: OutcomeBlocked, CheckedAt: now}
		c.proxies = map[string]Result{
			"http://first:3128":  {Outcome: OutcomeOK, CheckedAt: now},
			"http://second:3128": {Outcome: OutcomeOK, CheckedAt: now},
		}
		rec := c.Recommend("", []string{"http://second:3128", "http://first:3128"})
		if rec.Proxy != "http://second:3128" {
			t.Errorf("operator order must win, got %v", rec)
		}
	})

	t.Run("unreachable direct with no working proxy stays direct", func(t *testing.T) {
		c := New()
		now := time.Now()
		c.now = func() time.Time { return now }
		c.direct = Result{Outcome: OutcomeUnreachable, CheckedAt: now}
		c.proxies = map[string]Result{"http://dead:3128": {Outcome: OutcomeUnreachable, CheckedAt: now}}
		rec := c.Recommend("", []string{"http://dead:3128"})
		if rec.Route != RouteDirect {
			t.Errorf("a dead proxy must not be promoted over direct, got %v", rec)
		}
	})

	t.Run("stale results are not evidence", func(t *testing.T) {
		c := New()
		base := time.Now()
		c.now = func() time.Time { return base }
		c.direct = Result{Outcome: OutcomeOK, CheckedAt: base.Add(-cacheTTL - time.Second)}
		rec := c.Recommend("", nil)
		if rec.Route != RouteDirect || rec.Reason != "no evidence against the direct route" {
			t.Errorf("a stale OK must not be reported as a fresh win, got %v", rec)
		}
	})

	t.Run("unknown direct never justifies a proxy", func(t *testing.T) {
		c := New()
		now := time.Now()
		c.now = func() time.Time { return now }
		c.direct = Result{}
		c.proxies = map[string]Result{"http://p:3128": {Outcome: OutcomeOK, CheckedAt: now}}
		rec := c.Recommend("", []string{"http://p:3128"})
		if rec.Route != RouteDirect {
			t.Errorf("an unprobed network must not inherit a proxy, got %v", rec)
		}
	})

	t.Run("explicit operator preference always wins", func(t *testing.T) {
		c := New()
		now := time.Now()
		c.now = func() time.Time { return now }
		c.direct = Result{Outcome: OutcomeOK, CheckedAt: now}
		rec := c.Recommend("socks5://127.0.0.1:9050", []string{"http://other:3128"})
		if rec.Route != RouteProxy || rec.Proxy != "socks5://127.0.0.1:9050" {
			t.Errorf("operator preference must override evidence, got %v", rec)
		}
	})
}

// -----------------------------------------------------------------------------
// Caching and expiry
// -----------------------------------------------------------------------------

// A cached result must expire so a recovered network is re-observed and a
// newly broken one is not trusted forever.
func TestCachedResultsExpire(t *testing.T) {
	c := New()
	base := time.Now()
	c.now = func() time.Time { return base }
	c.direct = Result{Outcome: OutcomeOK, CheckedAt: base}

	if c.Direct().Outcome != OutcomeOK {
		t.Fatal("a fresh result must be reported")
	}
	// Advance past the TTL.
	c.now = func() time.Time { return base.Add(cacheTTL + time.Second) }
	if got := c.Direct().Outcome; got != OutcomeUnknown {
		t.Errorf("an expired result must report unknown, got %v", got)
	}
	// The expiry applies to proxies too.
	c.proxies = map[string]Result{"http://p:3128": {Outcome: OutcomeOK, CheckedAt: base}}
	if got := c.Proxy("http://p:3128").Outcome; got != OutcomeUnknown {
		t.Errorf("an expired proxy result must report unknown, got %v", got)
	}
	// And Invalidate clears everything immediately.
	c.direct = Result{Outcome: OutcomeOK, CheckedAt: base}
	c.Invalidate()
	if got := c.Direct().Outcome; got != OutcomeUnknown {
		t.Errorf("Invalidate must clear the direct result, got %v", got)
	}
}

// -----------------------------------------------------------------------------
// Real HTTP stack: the probe actually classifies live responses.
// These are the tests that prove the component works rather than merely
// being self-consistent.
// -----------------------------------------------------------------------------

// The end-to-end probe path: a real httptest server, a real transport, a
// real classification.
func TestProbeClassifiesRealResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.WaitProbed(ctx, probeConfig{origin: "demand"}); err != nil {
		t.Fatalf("WaitProbed: %v", err)
	}
	d := c.Direct()
	if d.Outcome != OutcomeOK {
		t.Errorf("local 200 server must classify as OK, got %v (reason %q)", d.Outcome, d.Reason)
	}
	if d.HTTPStatus != http.StatusOK {
		t.Errorf("HTTPStatus = %d, want 200", d.HTTPStatus)
	}
	if d.Elapsed <= 0 {
		t.Error("the result must report a positive elapsed time")
	}
}

// A server that refuses with 403 must classify as BLOCKED, not unreachable:
// the transport works, the source is the problem.
func TestProbeClassifiesRefusalAsBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = c.WaitProbed(ctx, probeConfig{origin: "demand"})
	d := c.Direct()
	if d.Outcome != OutcomeBlocked {
		t.Errorf("403 must classify as blocked, got %v (reason %q)", d.Outcome, d.Reason)
	}
	if d.HTTPStatus != http.StatusForbidden {
		t.Errorf("HTTPStatus = %d, want 403", d.HTTPStatus)
	}
}

// A 429 must classify as blocked (rate limit), not as a transport failure.
func TestProbeClassifiesRateLimitAsBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = c.WaitProbed(ctx, probeConfig{origin: "demand"})
	if got := c.Direct().Outcome; got != OutcomeBlocked {
		t.Errorf("429 must classify as blocked, got %v", got)
	}
}

// A stalled server must hit the bounded response-header timeout, classify as
// unreachable, and never pin the caller. The bounds are shortened for the
// test so proving the bound does not cost the full 8 seconds.
func TestProbeTimesOutOnStalledServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stall far beyond the response-header timeout, but release as soon
		// as the client goes away so the test never waits on Close().
		select {
		case <-time.After(60 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	c.timeoutsOverride = &timeouts{dial: 2 * time.Second, tls: 2 * time.Second, respHeader: 500 * time.Millisecond, probe: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_ = c.WaitProbed(ctx, probeConfig{origin: "demand"})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("probe of a stalled server took %s; the bounded timeout must cap it", elapsed)
	}
	d := c.Direct()
	if d.Outcome != OutcomeUnreachable {
		t.Errorf("stalled server must classify as unreachable, got %v (reason %q)", d.Outcome, d.Reason)
	}
}

// A server that tears the connection down before responding must classify as
// unreachable — the datacenter "TLS/SSL connection has been closed (EOF)"
// failure shape.
func TestProbeClassifiesConnectionTeardown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			if conn != nil {
				_ = conn.Close()
			}
		}
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = c.WaitProbed(ctx, probeConfig{origin: "demand"})
	d := c.Direct()
	if d.Outcome != OutcomeUnreachable {
		t.Errorf("torn-down connection must classify as unreachable, got %v", d.Outcome)
	}
	if d.Reason == "" {
		t.Error("an unreachable result must carry a reason")
	}
}

// A dead port (connection refused) must classify as unreachable with the
// refused reason, not hang.
func TestProbeClassifiesRefusedConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.Listener.Addr().String()
	srv.Close() // free the port so dialing it refuses

	restore := setProbeURL("http://" + addr + "/")
	defer restore()

	c := New()
	c.timeoutsOverride = &timeouts{dial: 2 * time.Second, tls: 2 * time.Second, respHeader: 2 * time.Second, probe: 8 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_ = c.WaitProbed(ctx, probeConfig{origin: "demand"})
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("probe of a refused port took %s", elapsed)
	}
	d := c.Direct()
	if d.Outcome != OutcomeUnreachable {
		t.Errorf("refused connection must classify as unreachable, got %v", d.Outcome)
	}
}

// A configured proxy that is actually reachable must be probed and reported
// OK only when the direct route is not OK — proving the component does not
// consume proxy resources nobody asked it to.
func TestProxyRouteProbedOnlyWhenDirectFails(t *testing.T) {
	// The "direct" target answers 200.
	directSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer directSrv.Close()

	var proxyHits atomic.Int64
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxySrv.Close()

	restore := setProbeURL(directSrv.URL)
	defer restore()

	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Direct works: the proxy must NOT be probed.
	if err := c.WaitProbed(ctx, probeConfig{proxies: []string{proxySrv.URL}, origin: "demand"}); err != nil {
		t.Fatalf("WaitProbed: %v", err)
	}
	if got := proxyHits.Load(); got != 0 {
		t.Errorf("a working direct route must not consume proxy resources (proxy hits = %d)", got)
	}
	if got := c.Proxy(proxySrv.URL).Outcome; got != OutcomeUnknown {
		t.Errorf("an unprobed proxy must report unknown, got %v", got)
	}

	// Now make the direct route fail; the proxy must be probed and reported OK.
	c.Invalidate()
	restoreDead := setProbeURL("http://127.0.0.1:1/") // nothing listens
	defer restoreDead()

	if err := c.WaitProbed(ctx, probeConfig{proxies: []string{proxySrv.URL}, origin: "demand"}); err != nil {
		t.Fatalf("WaitProbed (dead direct): %v", err)
	}
	if got := proxyHits.Load(); got == 0 {
		t.Error("a failing direct route must probe the configured proxy")
	}
	if got := c.Proxy(proxySrv.URL).Outcome; got != OutcomeOK {
		t.Errorf("proxy probe of a 200 server must report OK, got %v", got)
	}
	// And the recommendation must now select that proxy.
	rec := c.Recommend("", []string{proxySrv.URL})
	if rec.Route != RouteProxy || rec.Proxy != proxySrv.URL {
		t.Errorf("recommendation with a dead direct and a live proxy = %v, want the proxy", rec)
	}
}

// -----------------------------------------------------------------------------
// Concurrency safety
// -----------------------------------------------------------------------------

// The capability is shared across sessions and probe goroutines: every
// combination of readers and one writer must be race-free.
func TestConcurrentAccessIsRaceFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = c.Direct()
				_ = c.Proxy(fmt.Sprintf("http://p%d:3128", i))
				_ = c.Recommend("", nil)
				_ = c.Summarize("", nil)
				if j%50 == 0 {
					c.Invalidate()
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.EnsureProbed(context.Background(), probeConfig{origin: "demand"})
			time.Sleep(10 * time.Millisecond)
		}()
	}
	wg.Wait()
}

// A burst of concurrent demands must collapse into a single probe
// (single-flight), not a probe storm.
func TestConcurrentDemandsShareOneProbe(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(200 * time.Millisecond) // let the demand burst overlap
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.WaitProbed(context.Background(), probeConfig{origin: "demand"})
		}()
	}
	wg.Wait()
	if got := hits.Load(); got != 1 {
		t.Errorf("16 concurrent demands produced %d probe requests, want 1 (single-flight)", got)
	}
}

// Neither EnsureProbed nor WaitProbed may leak a goroutine that outlives a
// cancelled context.
func TestProbeGoroutineRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stall, but release the moment the client goes away so the test's
		// srv.Close() never waits on a dead connection.
		select {
		case <-time.After(30 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	// Short probe bound so the goroutine terminates quickly once the probe
	// deadline fires, proving the goroutine cannot outlive its context.
	c.timeoutsOverride = &timeouts{dial: 1 * time.Second, tls: 1 * time.Second, respHeader: 500 * time.Millisecond, probe: 2 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.WaitProbed(ctx, probeConfig{origin: "demand"})

	// Wait for the probe goroutine to finish: the probe runs on its own
	// bounded deadline, so it must terminate even though the caller's context
	// already expired. A leaked goroutine would keep c.inFlight set forever.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		inFlight := c.inFlight
		c.mu.Unlock()
		if inFlight == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.mu.Lock()
	inFlight := c.inFlight
	c.mu.Unlock()
	if inFlight != nil {
		t.Fatal("the probe goroutine must terminate after its bounded deadline, not leak")
	}
}

// -----------------------------------------------------------------------------
// Outage suppression: avoid repeated failed probes
// -----------------------------------------------------------------------------

// After a probe confirms no route works, re-probing the same dead network
// every cooldown is pure cost. The capability must suppress further probes
// for a bounded window and automatically re-arm when the window expires.
//
// This test drives the suppression logic directly through the real
// EnsureProbed path: it runs a probe against a server that never answers OK,
// confirms suppression engaged, confirms a demand inside the window does NOT
// launch a new probe, then advances the clock and confirms probing resumes.
func TestTotalOutageSuppressesFurtherProbes(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable) // 503: unknown, never OK
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	// A controllable clock so the suppression window can be advanced
	// deterministically without sleeping.
	base := time.Now()
	var tick atomic.Int64
	c.now = func() time.Time { return base.Add(time.Duration(tick.Load()) * time.Second) }

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = c.WaitProbed(ctx, probeConfig{origin: "demand"})

	// No route was OK (503 is unknown, direct is not OK): suppression must
	// have engaged.
	c.mu.Lock()
	suppressedUntil := c.suppressUntil
	c.mu.Unlock()
	if suppressedUntil.IsZero() {
		t.Fatal("a probe with no working route must engage suppression")
	}

	// A demand inside the suppression window must NOT launch a new probe.
	hitsBefore := hits.Load()
	c.EnsureProbed(ctx, probeConfig{origin: "demand"})
	if got := hits.Load(); got != hitsBefore {
		t.Errorf("a suppressed capability must not re-probe (hits %d -> %d)", hitsBefore, got)
	}

	// Advance the clock past the suppression window AND the cooldown, then
	// demand again: probing must resume.
	tick.Store(int64(suppressAfterOutage/time.Second) + int64(probeCooldown/time.Second) + 1)
	c.EnsureProbed(ctx, probeConfig{origin: "demand"})

	// The probe runs asynchronously on a goroutine with a bounded deadline.
	// Wait for it to land, with a hard cap so the test can never hang.
	deadline := time.Now().Add(probeTimeout + 5*time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() > hitsBefore {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := hits.Load(); got == hitsBefore {
		t.Errorf("probing must resume after the suppression window expires (hits still %d)", got)
	}
}

// Suppression must NOT survive a positive observation: a working network
// stays observable.
func TestPositiveObservationClearsSuppression(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = c.WaitProbed(ctx, probeConfig{origin: "demand"})
	if c.suppressed() {
		t.Error("a positive observation must not engage suppression")
	}
	// And the summary reports the un-suppressed state.
	if c.Summarize("", nil).Suppressed {
		t.Error("the summary must report Suppressed=false after a positive probe")
	}
}

// The summary's Suppressed field must reflect the registry: engaged after a
// total-outage probe, and re-armable after the window expires.
func TestSummaryReportsSuppressionState(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable) // never OK
	}))
	defer srv.Close()
	restore := setProbeURL(srv.URL)
	defer restore()

	c := New()
	base := time.Now()
	var tick atomic.Int64
	c.now = func() time.Time { return base.Add(time.Duration(tick.Load()) * time.Second) }

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = c.WaitProbed(ctx, probeConfig{origin: "demand"})

	if !c.Summarize("", nil).Suppressed {
		t.Fatal("the summary must report Suppressed=true after a total-outage probe")
	}
	// Expiry re-arms: the summary returns to un-suppressed once the window
	// passes (the next probe decides whether to re-engage).
	tick.Store(int64(suppressAfterOutage/time.Second) + 1)
	c.mu.Lock()
	suppressed := c.suppressedLocked()
	c.mu.Unlock()
	if suppressed {
		t.Error("suppression must expire with the window")
	}
}

// -----------------------------------------------------------------------------
// Summary shape: the diagnostics surface
// -----------------------------------------------------------------------------

func TestSummarizeReportsConfiguredProxiesWithUnknown(t *testing.T) {
	c := New()
	now := time.Now()
	c.now = func() time.Time { return now }
	c.direct = Result{Outcome: OutcomeOK, CheckedAt: now}

	s := c.Summarize("", []string{"  http://p:3128  ", ""})
	if len(s.Proxies) != 1 {
		t.Fatalf("blank and whitespace proxies must be dropped, got %d", len(s.Proxies))
	}
	if s.Proxies[0].Proxy != "http://p:3128" {
		t.Errorf("proxy identifier = %q", s.Proxies[0].Proxy)
	}
	if s.Proxies[0].Outcome != OutcomeUnknown {
		t.Errorf("never-probed proxy must report unknown, got %v", s.Proxies[0].Outcome)
	}
	if s.Recommended.Route != RouteDirect {
		t.Errorf("recommendation = %v, want direct", s.Recommended)
	}
	// LastProbeAt follows the probe clock exactly: zero only when no probe
	// ever ran.
	if s.LastProbeAt.IsZero() != c.lastProbeAt.IsZero() {
		t.Errorf("LastProbeAt zero=%v but lastProbeAt zero=%v", s.LastProbeAt.IsZero(), c.lastProbeAt.IsZero())
	}
}

// The String() rendering is the log-line format; it must be compact and
// contain the outcome of every configured route.
func TestStringRendering(t *testing.T) {
	c := New()
	now := time.Now()
	c.now = func() time.Time { return now }
	c.direct = Result{Outcome: OutcomeOK, CheckedAt: now}
	c.proxies = map[string]Result{"http://p:3128": {Outcome: OutcomeBlocked, CheckedAt: now}}

	got := c.String("", []string{"http://p:3128"})
	for _, want := range []string{"direct=ok", "proxy=blocked", "route=direct"} {
		if !stringsContains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}
