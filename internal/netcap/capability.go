// Package netcap is the adaptive network capability layer: it answers
// "can this process reach YouTube right now, through which route, and how
// sure is it?" without blocking meeting startup and without requiring the
// end user to configure a VPN, proxy or DNS.
//
// The design constraints are deliberate:
//
//   - No mandatory startup probe. Nothing runs until a caller asks, and the
//     first answer is always "unknown" so a meeting is never delayed by a
//     network check.
//   - Short-lived capability caching. A probe result is reused for a few
//     minutes, then forgotten. Network conditions change; a stale "direct
//     works" must not be trusted forever, and a stale "direct is blocked"
//     must not be trusted either.
//   - Advisory, never gating. Route() returns a recommendation; callers keep
//     the right to ignore it. Nothing here can fail a session.
//   - Direct-first. The unproxied route is always probed and always
//     preferred; a configured proxy is only used when the direct route was
//     actually observed being blocked, and only when an operator configured
//     it. Tor/ambient proxies are never auto-enabled.
package netcap

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Route is one egress path the capability layer knows about.
type Route string

const (
	// RouteDirect is the unproxied connection: no proxy, no custom DNS.
	RouteDirect Route = "direct"
	// RouteProxy is an operator-configured proxy (download.proxy or
	// download.fallback_proxies).
	RouteProxy Route = "proxy"
)

// probeURLTarget is the endpoint used for DIRECT connectivity probes:
// YouTube's own lightweight, redirect-free page. A 2xx or 3xx answer proves
// the TLS+HTTP stack reaches the source, which is the cheapest signal that
// carries real meaning (not a bare TCP connect).
//
// It is a var (not a const) protected by probeURLMu purely so tests can
// redirect it at a local httptest server while a probe goroutine may be
// reading it; production code must never reassign it.
var (
	probeURLMu   sync.RWMutex
	probeURLBody = "https://www.youtube.com/"
)

// probeURL returns the endpoint capability probes use, under the read lock a
// concurrent probe goroutine also honours. NETCAP_PROBE_URL overrides the
// default target so an operator (or a validation harness) can point the
// capability probe at a known-good or known-bad endpoint without editing
// code.
func probeURL() string {
	probeURLMu.RLock()
	defer probeURLMu.RUnlock()
	if v := strings.TrimSpace(os.Getenv("NETCAP_PROBE_URL")); v != "" {
		return v
	}
	return probeURLBody
}

// ProbeURL returns the endpoint capability probes use.
func ProbeURL() string { return probeURL() }

// setProbeURL overrides the probe target and returns a restore function.
// Test seam only.
func setProbeURL(url string) func() {
	probeURLMu.Lock()
	orig := probeURLBody
	probeURLBody = url
	probeURLMu.Unlock()
	return func() {
		probeURLMu.Lock()
		probeURLBody = orig
		probeURLMu.Unlock()
	}
}

// Outcome is what a probe learned about one route.
type Outcome int

const (
	// OutcomeUnknown means no probe has produced an answer yet (or the cached
	// answer expired). Callers must treat this as "no information": it never
	// justifies avoiding a route, and it never justifies using one.
	OutcomeUnknown Outcome = iota
	// OutcomeOK means the route reached the source and got a real HTTP
	// response. It does NOT prove YouTube will serve media through the route
	// (a bot check can still apply per video) — only that the transport
	// works.
	OutcomeOK
	// OutcomeBlocked means the source itself refused the route (HTTP 403/429
	// or the documented bot-challenge response). Distinct from a transport
	// failure: the network is fine, the source is the problem.
	OutcomeBlocked
	// OutcomeUnreachable means the route could not carry the request at all
	// (DNS failure, TLS EOF, connection reset, timeout, dead proxy). The
	// route is broken, not refused.
	OutcomeUnreachable
)

// String renders an Outcome for logs and diagnostics.
func (o Outcome) String() string {
	switch o {
	case OutcomeOK:
		return "ok"
	case OutcomeBlocked:
		return "blocked"
	case OutcomeUnreachable:
		return "unreachable"
	default:
		return "unknown"
	}
}

// dialTimeout bounds every TCP connect in a probe. A probe must never pin
// the caller: this is the hard ceiling on one route's connect attempt.
const dialTimeout = 5 * time.Second

// tlsHandshakeTimeout bounds the TLS handshake after the connect succeeds.
// A torn-down handshake (the classic datacenter "TLS/SSL connection has been
// closed (EOF)") must not consume the whole probe budget.
const tlsHandshakeTimeout = 5 * time.Second

// responseHeaderTimeout bounds the time to receive response headers. It is
// the backstop for a server that accepts the connection and then stalls.
const responseHeaderTimeout = 8 * time.Second

// probeTimeout bounds one whole probe (all routes, sequentially). The probe
// runs on a goroutine with its own context; this is the absolute ceiling.
const probeTimeout = 20 * time.Second

// timeouts are the bounds a probe runs under. They are package constants in
// production and overridable per-Capability in tests, so a test that needs
// a "stalled server" does not have to wait the real 8-second header timeout
// to prove the bound works.
type timeouts struct {
	dial       time.Duration
	tls        time.Duration
	respHeader time.Duration
	probe      time.Duration
}

// defaultTimeouts is the production bound set.
var defaultTimeouts = timeouts{
	dial:       dialTimeout,
	tls:        tlsHandshakeTimeout,
	respHeader: responseHeaderTimeout,
	probe:      probeTimeout,
}

// tmux returns the effective bounds: the test override when installed,
// otherwise the production constants.
func (c *Capability) tmux() timeouts {
	if c.timeoutsOverride != nil {
		return *c.timeoutsOverride
	}
	return defaultTimeouts
}

// cacheTTL is how long a probe result stays authoritative. Short by design:
// the capability layer exists to adapt, and adaptation needs fresh
// observations. Both positive and negative results expire together — a
// recovered network and a newly broken network are equally important.
const cacheTTL = 3 * time.Minute

// probeCooldown bounds how often the SAME probe may be re-run while a cached
// result is still live. Without it, a caller that ignores the cache (or a
// busy loop of sessions) would re-probe on every call; with it, repeated
// demands collapse into one probe per cooldown window.
const probeCooldown = 30 * time.Second

// Result is one probe outcome for one route.
type Result struct {
	Route       Route
	Proxy       string // the proxy URL for RouteProxy, "" otherwise
	Outcome     Outcome
	HTTPStatus  int    // 0 when no response was received
	Reason      string // short, credential-free classification hint
	CheckedAt   time.Time
	Elapsed     time.Duration
	ProbeOrigin string // "startup" | "demand" | "recovery", for diagnostics
}

// Capability is the concurrency-safe, cached view of the network capability.
// The zero value is a usable, always-unknown capability: every method is
// safe to call before the first probe and after the clock has moved on.
type Capability struct {
	// now overrides the wall clock (tests). nil means time.Now.
	now func() time.Time

	mu      sync.Mutex
	direct  Result
	proxies map[string]Result
	// lastProbeAt is when a probe last completed, used by the cooldown.
	lastProbeAt time.Time
	// inFlight is non-nil while a probe goroutine is running. Closed when it
	// finishes, so concurrent callers can wait for the SAME probe instead of
	// starting their own (single-flight without a dependency).
	inFlight chan struct{}
	// lastErr is the last probe's non-fatal error, surfaced in diagnostics.
	lastErr string
	// suppressUntil is the deadline until which new probes must not be
	// launched (see Suppress). Zero means "not suppressed".
	suppressUntil time.Time
	// timeoutsOverride replaces the production probe bounds in tests. nil
	// means "use the package constants".
	timeoutsOverride *timeouts

	// udpMu guards the UDP egress observation (udpprobe.go). It is separate
	// from mu because the probe goroutine only ever touches these fields.
	udpMu sync.Mutex
	udp   udpProbeState
}

// udpProbeState caches the latest UDP egress observation with single-flight
// refresh (see EnsureUDPProbed). All fields are guarded by udpMu.
type udpProbeState struct {
	result  UDPResult
	running bool
	lastTry time.Time
}

// New returns a fresh Capability with no observations.
func New() *Capability { return &Capability{} }

// utcNow returns the effective clock.
func (c *Capability) utcNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// probeDialer builds the transport for one probe request. proxy is the
// route's proxy URL; "" means a direct connection. Every stage is bounded so
// a probe cannot hang, and the dialer refuses nothing about destination
// addresses: probing an operator-configured proxy is the operator's own
// intent, and YouTube is public infrastructure.
func probeDialer(proxy string, tm timeouts) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   tm.dial,
		KeepAlive: 0, // one-shot request; no pool to keep warm
	}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   tm.tls,
		ResponseHeaderTimeout: tm.respHeader,
		// Probes are intentionally discardable: no idle pool may outlive the
		// request, and no connection may be reused across probes.
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     5 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return tr
}

// classify converts an HTTP response or an error into an Outcome plus a
// short reason. This is the one place the failure taxonomy is decided, so
// callers (and tests) share one definition of "blocked" vs "unreachable".
//
// The taxonomy mirrors what yt-dlp and the media browser actually
// experience, so the capability layer's verdicts stay comparable to the
// pipeline's own:
//
//   - DNS failure, connection refused/reset, TLS EOF, timeout:
//     OutcomeUnreachable — the route cannot carry traffic.
//   - HTTP 403/429 on the YouTube probe page: OutcomeBlocked — the transport
//     works, the source refuses this egress (bot check / rate limit).
//   - HTTP 200/3xx: OutcomeOK.
//   - Anything else (odd status, half-usable proxy): OutcomeUnknown, which
//     never justifies either using or avoiding the route.
func classify(resp *http.Response, err error) (Outcome, int, string) {
	if err != nil {
		return OutcomeUnreachable, 0, classifyErr(err)
	}
	if resp == nil {
		return OutcomeUnknown, 0, "no response"
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	switch {
	case status >= 200 && status < 400:
		return OutcomeOK, status, ""
	case status == 403 || status == 429:
		return OutcomeBlocked, status, "source refused this egress (bot check or rate limit)"
	default:
		// 4xx/5xx that is not a refusal shape: inconclusive.
		return OutcomeUnknown, status, "unexpected status"
	}
}

// classifyErr turns a transport error into a short, credential-free reason
// string. It must never embed the proxy URL or any address: reasons surface
// in /healthz and the doctor, which are readable by anyone.
func classifyErr(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	// Order matters: the specific shapes first, the generic ones last.
	switch {
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "dns"):
		return "DNS resolution failed"
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "connection reset"):
		return "connection reset by peer"
	case strings.Contains(msg, "eof"):
		return "TLS/SSL connection has been closed (EOF)"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	case strings.Contains(msg, "no route to host"), strings.Contains(msg, "network is unreachable"):
		return "no route to host"
	case strings.Contains(msg, "proxy"):
		return "proxy unusable"
	default:
		return "transport error"
	}
}

// stripProxyCredentials removes any user:password@ component from a proxy
// URL. The capability layer's(Result.Proxy, and every summary derived from
// it, surfaces in /healthz and the doctor — endpoints readable by anyone —
// so a proxy URL that carries credentials must be reduced to its identifier
// form before it is stored. The unredacted URL is still used to dial.
func stripProxyCredentials(proxy string) string {
	if proxy == "" {
		return ""
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return proxy // not a URL: pass through untouched
	}
	if u.User == nil {
		return proxy
	}
	// Drop the userinfo entirely: even an empty password leaves "user:@" in
	// the rendered URL, and the summary must show the bare identifier.
	u.User = nil
	return u.String()
}

// probeRoute performs one bounded request over one route and classifies it.
// ctx must already carry the probe deadline. proxy is "" for the direct
// route.
func probeRoute(ctx context.Context, tm timeouts, proxy string) Result {
	rt := RouteDirect
	p := stripProxyCredentials(proxy)
	if p != "" {
		rt = RouteProxy
	}
	client := &http.Client{
		Transport: probeDialer(proxy, tm),
		// The probe follows no redirects: the probe page does not redirect,
		// and following one would silently turn a refusal into a success.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL(), nil)
	if err != nil {
		return Result{Route: rt, Proxy: p, Outcome: OutcomeUnreachable, Reason: "bad probe request", CheckedAt: start, Elapsed: time.Since(start)}
	}
	// A real, non-bot user agent: the probe measures what a user would see,
	// not what a bare Go client sees.
	req.Header.Set("User-Agent", probeUserAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	elapsed := time.Since(start)
	outcome, status, reason := classify(resp, err)
	r := Result{
		Route:      rt,
		Proxy:      p,
		Outcome:    outcome,
		HTTPStatus: status,
		Reason:     reason,
		CheckedAt:  time.Now(),
		Elapsed:    elapsed,
	}
	// A refused/loop redirect is reported through classify; a stray body is
	// drained and closed inside classify.
	return r
}

// probeUserAgent is the UA used for capability probes.
const probeUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// ProbeConfig describes what a probe should measure: the direct route first,
// then the operator-configured proxies, in operator order. Build it with
// NewProbeConfig; the zero value probes the direct route only, which is the
// correct zero-config behaviour.
type ProbeConfig = probeConfig

// NewProbeConfig builds a probe configuration from an operator's egress
// preference and egress ladder. preferred is the explicitly configured
// primary egress ("" when the operator left it to the capability layer);
// proxies are the fallback ladder in operator order. Both are used to decide
// which routes a probe should measure.
func NewProbeConfig(preferred string, proxies []string, origin string) ProbeConfig {
	return probeConfig{preferred: strings.TrimSpace(preferred), proxies: proxies, origin: origin}
}

// probeConfig describes what a probe should measure: the direct route first,
// then the operator-configured proxies, in operator order.
type probeConfig struct {
	// preferred is the operator's explicit primary egress. When non-empty it
	// overrides every evidence-based recommendation (see Recommend).
	preferred string
	// proxies are the egresses to probe, in preference order, AFTER the
	// direct route. Empty means "direct only" — the zero-config install.
	proxies []string
	// origin labels the probe for diagnostics ("startup", "demand",
	// "recovery").
	origin string
}

// runProbe probes the configured routes and returns the results. It always
// runs to completion or ctx cancellation, never panics, and never returns an
// error to the caller: a probe failure returns the measurements it did take,
// and publish() keeps whatever it is given.
//
// It must be called WITHOUT holding c.mu: results are published under the
// lock only by the caller.
func (c *Capability) runProbe(ctx context.Context, cfg probeConfig) (Result, []Result) {
	defer func() {
		if r := recover(); r != nil {
			// A panic in a probe goroutine must never kill the meeting
			// goroutine that triggered it.
			c.mu.Lock()
			c.lastErr = "probe panic"
			c.mu.Unlock()
		}
	}()
	pctx, cancel := context.WithTimeout(ctx, c.tmux().probe)
	defer cancel()

	// Direct route first: it is the answer a zero-config install needs, and
	// when it works the proxies do not matter at all — skipping them keeps
	// the probe cheap and avoids consuming proxy resources nobody asked for.
	direct := probeRoute(pctx, c.tmux(), "")
	if direct.Outcome == OutcomeOK && cfg.preferred == "" {
		// Only probe the configured alternatives when the direct route did
		// not cleanly work. An operator proxy is a scarce, sometimes
		// metered resource; probing it while direct works would burn it for
		// no reason. This is also what makes "fallback only when justified
		// by classified failures" the measured behaviour rather than a
		// slogan. An explicit operator preference is still measured even
		// when the direct route works, because the operator chose it and
		// the diagnostics must report its health.
		return direct, nil
	}
	return direct, probeRouteAll(pctx, c.tmux(), cfg.proxies)
}

// probeRouteAll probes the configured proxies sequentially, in order. It
// returns results only for routes actually measured.
func probeRouteAll(ctx context.Context, tm timeouts, proxies []string) []Result {
	out := make([]Result, 0, len(proxies))
	for _, p := range proxies {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		out = append(out, probeRoute(ctx, tm, p))
	}
	return out
}

// suppressAfterOutage is how long probes stay suppressed after a probe
// confirmed no route works at all. Long enough to make a hard outage cheap,
// short enough that a recovered network is re-observed promptly.
const suppressAfterOutage = 2 * time.Minute

// anyProxyOKLocked reports whether any cached proxy result is fresh and OK.
// Caller must hold c.mu.
func (c *Capability) anyProxyOKLocked() bool {
	for p, r := range c.proxies {
		if c.freshLocked(r.CheckedAt) && r.Outcome == OutcomeOK {
			_ = p
			return true
		}
	}
	return false
}

// EnsureProbed makes sure at least one probe has run. The FIRST call starts
// a probe goroutine; subsequent calls within the cooldown are no-ops. It
// returns immediately in every case — it never blocks the caller, and never
// blocks meeting startup. Use WaitProbed when a caller can afford to wait.
func (c *Capability) EnsureProbed(ctx context.Context, cfg probeConfig) {
	c.mu.Lock()
	if c.inFlight != nil {
		c.mu.Unlock()
		return // a probe is already running; its result will serve everyone
	}
	if c.suppressedLocked() {
		c.mu.Unlock()
		return // probes suppressed after a confirmed total outage
	}
	if !c.lastProbeAt.IsZero() && c.utcNow().Sub(c.lastProbeAt) < probeCooldown {
		c.mu.Unlock()
		return // a probe ran recently; let the cache serve
	}
	done := make(chan struct{})
	c.inFlight = done
	c.mu.Unlock()

	go func() {
		defer close(done)
		direct, proxies := c.runProbe(ctx, cfg)
		c.mu.Lock()
		if c.inFlight == done {
			c.inFlight = nil
		}
		// Publish under the same lock as the suppression decision so the two
		// never disagree about what the probe found.
		c.publishLocked(direct, proxies)
		// A probe that found NO working route has measured an outage, not a
		// route preference: stop re-measuring it every cooldown for a
		// bounded window. Any later positive observation clears it.
		if direct.Outcome != OutcomeOK && !anyOKLocked(proxies) {
			c.suppressUntil = c.utcNow().Add(suppressAfterOutage)
		} else {
			c.suppressUntil = time.Time{}
		}
		c.mu.Unlock()
	}()
}

// publishLocked stores a completed probe's results. Caller must hold c.mu.
func (c *Capability) publishLocked(direct Result, proxies []Result) {
	c.direct = direct
	if len(proxies) > 0 {
		if c.proxies == nil {
			c.proxies = map[string]Result{}
		}
		for _, r := range proxies {
			c.proxies[r.Proxy] = r
		}
	}
	c.lastProbeAt = c.utcNow()
	c.lastErr = ""
}

// anyOKLocked reports whether any probed proxy result is OK. Freshness is
// implied: the results come straight from a probe that just finished.
func anyOKLocked(proxies []Result) bool {
	for _, r := range proxies {
		if r.Outcome == OutcomeOK {
			return true
		}
	}
	return false
}

// WaitProbed blocks until a probe has completed (or the context expires),
// then returns. It exists for the non-critical path — the doctor, the
// diagnostics card, a background sweep — where a fresh answer is worth a
// bounded wait. Meeting startup must never call it.
func (c *Capability) WaitProbed(ctx context.Context, cfg probeConfig) error {
	c.EnsureProbed(ctx, cfg)
	c.mu.Lock()
	inFlight := c.inFlight
	c.mu.Unlock()
	if inFlight == nil {
		return nil // probe already finished (or recently ran)
	}
	select {
	case <-inFlight:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Direct returns the cached result for the unproxied route. Expired results
// are reported as OutcomeUnknown: an old observation is not evidence.
func (c *Capability) Direct() Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.direct
	if r.Outcome == OutcomeUnknown || !c.freshLocked(r.CheckedAt) {
		return Result{Route: RouteDirect}
	}
	return r
}

// Proxy returns the cached result for one configured proxy. Expired or
// never-probed results are OutcomeUnknown.
func (c *Capability) Proxy(proxy string) Result {
	p := strings.TrimSpace(proxy)
	if p == "" {
		return Result{Route: RouteDirect}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.proxies[p]
	if !ok || !c.freshLocked(r.CheckedAt) {
		return Result{Route: RouteProxy, Proxy: p}
	}
	return r
}

// freshLocked reports whether a result is still inside the cache window.
// The zero CheckedAt (never probed) is never fresh.
func (c *Capability) freshLocked(checkedAt time.Time) bool {
	if checkedAt.IsZero() {
		return false
	}
	return c.utcNow().Sub(checkedAt) < cacheTTL
}

// Recommendation is the route decision a caller should act on. It is
// advisory: nothing in netcap enforces it.
type Recommendation struct {
	// Route is the recommended egress: RouteDirect or RouteProxy.
	Route Route
	// Proxy is the proxy URL for RouteProxy, "" for RouteDirect.
	Proxy string
	// Reason is a short, credential-free explanation an operator can read.
	Reason string
}

// String renders a recommendation compactly for logs.
func (r Recommendation) String() string {
	if r.Route == RouteProxy {
		return "proxy:" + r.Proxy + " (" + r.Reason + ")"
	}
	return "direct (" + r.Reason + ")"
}

// Recommend answers "which egress should this download use right now?".
//
// Preference order, matching the operator's intent and the zero-config
// promise:
//
//  1. An explicit operator preference, when given and non-empty, wins without
//     any probing. The operator configured the network; their choice stands
//     unless evidence forces otherwise — and even then only the caller can
//     decide to demote it (see DemoteBlocked).
//  2. Otherwise the direct route, when it is freshly observed OK.
//  3. Otherwise the first configured proxy that is freshly observed OK,
//     when a probe actually ran and found the direct route NOT ok.
//  4. Otherwise direct again: no evidence exists that it is broken, an
//     unprobed network must not inherit a proxy it never asked for, and a
//     degraded direct route is still reachable while an unknown proxy might
//     not be.
func (c *Capability) Recommend(preferred string, proxies []string) Recommendation {
	pref := strings.TrimSpace(preferred)
	if pref != "" {
		return Recommendation{Route: RouteProxy, Proxy: pref, Reason: "operator-configured egress"}
	}
	d := c.Direct()
	if d.Outcome == OutcomeOK {
		return Recommendation{Route: RouteDirect, Reason: "direct egress reachable"}
	}
	if d.Outcome != OutcomeUnknown {
		// The direct route was observed failing (blocked or unreachable):
		// a configured proxy that was observed OK is the justified fallback.
		for _, p := range proxies {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			r := c.Proxy(p)
			if r.Outcome == OutcomeOK {
				return Recommendation{Route: RouteProxy, Proxy: p, Reason: "direct egress " + d.Outcome.String() + "; " + p + " is reachable"}
			}
		}
	}
	return Recommendation{Route: RouteDirect, Reason: "no evidence against the direct route"}
}

// Summary is the capability surface exposed through /healthz and the doctor.
// It reports only route identifiers and outcomes — never credentials, and
// never more history than the live cache window.
type Summary struct {
	// Direct is the cached direct-route result (OutcomeUnknown when no fresh
	// observation exists).
	Direct Result
	// Proxies lists the cached results for the configured proxies, in the
	// order the operator configured them. A proxy with no fresh observation
	// reports OutcomeUnknown.
	Proxies []Result
	// Recommended is the current route recommendation for the configured
	// preference and ladder.
	Recommended Recommendation
	// LastProbeAt is when a probe last completed (zero value: never).
	LastProbeAt time.Time
	// LastErr is the last probe's non-fatal failure note, for diagnostics.
	LastErr string
	// Suppressed reports whether probe launching is currently suppressed
	// after a probe confirmed no route works at all. It is a diagnostic:
	// suppression never gates what a caller is allowed to try.
	Suppressed bool
	// UDP is the latest WebRTC media-transport observation (udpprobe.go):
	// whether real UDP datagrams get answers on this network. Unknown until
	// the first probe lands.
	UDP UDPResult
}

// Summarize returns the full diagnostic view for one configuration.
func (c *Capability) Summarize(preferred string, proxies []string) Summary {
	c.mu.Lock()
	last := c.lastProbeAt
	lastErr := c.lastErr
	c.mu.Unlock()

	out := Summary{
		Direct:      c.Direct(),
		Proxies:     make([]Result, 0, len(proxies)),
		Recommended: c.Recommend(preferred, proxies),
		LastProbeAt: last,
		LastErr:     lastErr,
		Suppressed:  c.suppressed(),
		UDP:         c.UDP(),
	}
	for _, p := range proxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// A proxy that was never probed, or whose result expired, is
		// reported with an explicit unknown outcome rather than omitted, so
		// the diagnostics distinguish "configured and untested" from "not
		// configured at all". The identifier is stripped of credentials: the
		// summary reaches /healthz, which anyone can read.
		p = stripProxyCredentials(p)
		r := c.Proxy(p)
		if r.Outcome == OutcomeUnknown {
			r = Result{Route: RouteProxy, Proxy: p}
		}
		out.Proxies = append(out.Proxies, r)
	}
	return out
}

// Invalidate drops every cached observation. Callers that have independent
// evidence the network changed (a download just failed in a way no probe
// could see) use this so the next demand re-probes immediately instead of
// trusting a now-stale result.
func (c *Capability) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.direct = Result{}
	c.proxies = nil
	c.lastProbeAt = time.Time{}
}

// Suppress stops the capability from launching new probes until the given
// deadline. A probe that just confirmed a total outage (no route works) has
// learned nothing worth re-measuring every 30 seconds: suppressing further
// probes for a bounded window keeps an unreachable network cheap instead of
// burning the probe budget against a dead link. Positive observations reset
// the suppression, because a working network may stop working at any time
// and must stay observable.
func (c *Capability) Suppress(until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.suppressUntil = until
}

// suppressedLocked reports whether probe launching is currently suppressed.
func (c *Capability) suppressedLocked() bool {
	t := c.suppressUntil
	return !t.IsZero() && c.utcNow().Before(t)
}

// suppressed is the exported, lock-safe view used by diagnostics.
func (c *Capability) suppressed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.suppressedLocked()
}

// SetProbeURLForTest redirects the probe target and returns a restore
// function. Test seam only; production code must never call it.
func SetProbeURLForTest(url string) func() { return setProbeURL(url) }

// SetForTest installs a clock override and seeds a direct-route result in one
// call, so callers can build a deterministic capability state without running
// a probe. Test seam only.
func (c *Capability) SetForTest(now func() time.Time, direct Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
	c.direct = direct
	if direct.CheckedAt.IsZero() && now != nil {
		c.direct.CheckedAt = now()
	}
}

// String renders the capability compactly for a log line.
func (c *Capability) String(preferred string, proxies []string) string {
	s := c.Summarize(preferred, proxies)
	var b strings.Builder
	b.WriteString("direct=")
	b.WriteString(s.Direct.Outcome.String())
	for _, p := range s.Proxies {
		b.WriteString(" proxy=")
		b.WriteString(p.Outcome.String())
	}
	b.WriteString(" route=")
	b.WriteString(s.Recommended.String())
	return b.String()
}
