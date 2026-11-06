package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The defect: a YouTube download died with
// "Unable to download API page: 15036 bytes read (caused by <IncompleteRead>)"
// and the session went straight to error. That failure is a per-IP rate limit
// on the egress, not a verdict on the video: the same URL extracts fine over a
// different route. A configured fallback must be used.
// DIRECT FIRST: the ladder must try the unproxied route before any configured
// proxy, so a zero-config install works and a throttled proxy cannot take the
// whole feature down. The fake bot-checks the direct route (the exact shape
// that historically killed a bare-datacenter download), the configured proxy
// is then used, and only when BOTH lose does the Tor fallback run.
func TestYTDLPFallsBackToAlternateEgress(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Proxy = "http://172.17.0.1:3339"
	opts.FallbackProxies = []string{"socks5://172.17.0.1:9050"}
	// The bot-check here is an ACCOUNT gate; with a cookie jar configured
	// the ladder still advances (the jar can answer it on any route).
	opts.CookiesFile = "/cookies.txt"

	var calls [][]string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		calls = append(calls, append([]string(nil), args...))
		// Direct route: YouTube bot-checks the unproxied egress. This must
		// NOT terminate the download — it is an egress-shaped refusal and the
		// ladder advances to the configured proxy.
		if !hasProxy(args, opts.Proxy) && !hasProxy(args, opts.FallbackProxies[0]) {
			return nil, []byte("ERROR: [youtube] X: Sign in to confirm you're not a bot"), exec.ErrNotFound
		}
		// Configured primary proxy: YouTube truncates the innertube response
		// mid-body.
		if hasProxy(args, opts.Proxy) {
			return nil, []byte("ERROR: [youtube] DIjPGEQKbH8: Unable to download API page: 15036 bytes read (caused by <IncompleteRead: 15036 bytes read>); please report this issue"), exec.ErrNotFound
		}
		// Tor fallback: extraction succeeds and the file lands on disk.
		writeStub(t, dir+"/vid.mp4")
		return []byte("136,1280,720,30,avc1.64001f,mp4a.40.2,mp4,1500\n"), nil, nil
	})

	if _, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	}); err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if len(calls) < 3 {
		t.Fatalf("expected direct, primary and fallback runs, got %d call(s)", len(calls))
	}
	// Each route may run up to maxExecRetries+1 attempts, so assert on the
	// DISTINCT routes in order rather than on positional call indices.
	firstProxy := ""
	for _, c := range calls {
		if p := proxyOf(c); p != "" {
			firstProxy = p
			break
		}
	}
	if firstProxy != opts.Proxy {
		t.Fatalf("the first proxied run must use the configured proxy %q, got %q", opts.Proxy, firstProxy)
	}
	if hasProxy(calls[0], opts.Proxy) || hasProxy(calls[0], opts.FallbackProxies[0]) {
		t.Fatalf("first run must be the DIRECT route (no --proxy), argv: %q", calls[0])
	}
	if !hasProxy(calls[len(calls)-1], opts.FallbackProxies[0]) {
		t.Fatalf("last run must use the fallback proxy, argv: %q", calls[len(calls)-1])
	}
}

// proxyOf returns the --proxy value carried by argv, or "" for a direct run.
func proxyOf(argv []string) string {
	for i, a := range argv {
		if a == "--proxy" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// The defect (session s-7d2d1b03): the Tor fallback route extracted the video
// cleanly, then YouTube rejected the googlevideo body mid-transfer for that
// circuit ("ERROR: unable to download video data: HTTP Error 403: Forbidden").
// The historical classifier did not recognise a body-shaped rejection as
// egress-shaped, so the ladder treated it as a real verdict and the session
// died. A mid-body 403 is a property of the route (Tor's rotating exit pool
// lands on throttled IPs), not of the video: the next attempt rotates to a
// fresh circuit and must be given the chance.
func TestYTDLPMidBody403AdvancesEgressLadder(t *testing.T) {
	// A cookie jar is set because the direct leg of this scenario returns a
	// bot-check: the account-gate fast fail stops the ladder only when NO
	// jar could answer it (the without-jar half is pinned in cookies_test.go).
	jar := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(jar, []byte("# jar\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.CookiesFile = jar
	opts.Proxy = "http://172.17.0.1:3339"
	opts.FallbackProxies = []string{"socks5://172.17.0.1:9050"}

	var calls [][]string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		calls = append(calls, append([]string(nil), args...))
		// Primary egress: API-level block, advances immediately.
		if hasProxy(args, opts.Proxy) {
			return nil, []byte("ERROR: [youtube] Pu-Ny1L8yDU: Sign in to confirm you’re not a bot"), exec.ErrNotFound
		}
		// Fallback egress: extraction succeeds (the URL is reachable) but the
		// media body is rejected for this route. A bounded retry over the same
		// rotating egress must be possible, and the failure must be classified
		// egress-shaped so it never reads as a verdict on the video.
		return nil, []byte("ERROR: unable to download video data: HTTP Error 403: Forbidden"), exec.ErrNotFound
	})

	if _, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	}); err == nil {
		t.Fatal("expected the download to fail when every egress is rejected")
	}
	// The primary was tried, then the fallback: a body 403 must not fail on
	// the first route while a configured fallback is still untried.
	if len(calls) < 2 {
		t.Fatalf("expected the fallback egress to be reached, got %d call(s)", len(calls))
	}
	if !hasProxy(calls[len(calls)-1], opts.FallbackProxies[0]) {
		t.Fatalf("the last attempt must run over the fallback egress, argv: %q", calls[len(calls)-1])
	}
	// The registry must now carry BOTH routes so the media browser does not
	// open the YouTube tab through an egress the source just rejected.
	for _, p := range []string{opts.Proxy, opts.FallbackProxies[0]} {
		if !m.EgressBlocked(p) {
			t.Errorf("egress %s must be marked blocked after a source rejection", p)
		}
	}
}

// A genuine extractor verdict ("Requested format is not available") says the
// URL is the problem, not the route. It must NOT burn the fallbacks: three
// slow runs over proxies that cannot help are worse than one fast failure.
func TestYTDLPGenuineFailureDoesNotBurnFallbacks(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Proxy = "http://172.17.0.1:3339"
	opts.FallbackProxies = []string{"socks5://172.17.0.1:9050"}

	var calls int
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		calls++
		return nil, []byte("ERROR: [youtube] abcdefghijk: Requested format is not available. Use --list-formats for a list of available formats"), exec.ErrNotFound
	})

	if _, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	}); err == nil {
		t.Fatal("expected the download to fail on a genuine extractor verdict")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call (no fallback for a real verdict), got %d", calls)
	}
}

// Unit form of the classifier: these are the exact strings the field and the
// logs carry, so the ladder must recognise every one of them.
func TestProxyBlockedClassifier(t *testing.T) {
	blocked := []string{
		"ERROR: [youtube] DIjPGEQKbH8: Unable to download API page: 15036 bytes read (caused by <IncompleteRead: 15036 bytes read>)",
		"ERROR: [youtube] DIjPGEQKbH8: Unable to download API page: 2 bytes read (caused by <IncompleteRead: 2 bytes read>)",
		"ERROR: [youtube] DIjPGEQKbH8: Sign in to confirm you're not a bot. This helps protect our community.",
		"ERROR: [youtube] DIjPGEQKbH8: Sign in to confirm you’re not a bot. This helps protect our community.",
		"ERROR: [youtube] DIjPGEQKbH8: Unable to download API page: HTTP Error 429: Too Many Requests",
		"ERROR: [youtube] Pu-Ny1L8yDU: Unable to download API page: TLS/SSL connection has been closed (EOF) (_ssl.c:992) (caused by SSLError('TLS/SSL connection has been closed (EOF) (_ssl.c:992)'))",
		"WARNING: [youtube] Unable to download webpage: TLS/SSL connection has been closed (EOF)",
		// A mid-body rejection: YouTube throttles the googlevideo transfer for
		// this exact egress ("unable to download video data: HTTP Error 403").
		// That is a property of the route, not of the video, so the egress
		// ladder must treat it as egress-shaped and advance.
		"ERROR: [youtube] Pu-Ny1L8yDU: unable to download video data: HTTP Error 403: Forbidden",
	}
	for _, s := range blocked {
		if !proxyBlocked([]byte(s)) {
			t.Errorf("proxyBlocked(%q) = false, want true", s)
		}
	}
	real := []string{
		"ERROR: [youtube] abcdefghijk: Requested format is not available. Use --list-formats for a list of available formats",
		"ERROR: Unsupported URL: https://example.com/",
		"ERROR: [youtube] abcdefghijk: Video unavailable. This video is not available",
		"ERROR: [youtube] abcdefghijk: The page needs to be reloaded.",
		"",
	}
	for _, s := range real {
		if proxyBlocked([]byte(s)) {
			t.Errorf("proxyBlocked(%q) = true, want false", s)
		}
	}
}

// The empty string means "direct connection": an operator who configured no
// proxy at all keeps the historical single-route behaviour, and the argv must
// not carry an empty --proxy value.
func TestEgressesDirectWhenNoProxyConfigured(t *testing.T) {
	dir := t.TempDir()
	m := newYtdlpTestManager(t, DefaultOptions(dir), func(_ context.Context, args []string) ([]byte, []byte, error) {
		return nil, nil, exec.ErrNotFound
	})
	got := m.egresses()
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("egresses() = %q, want [\"\"] (direct only)", got)
	}
	for _, a := range m.ytdlpArgs("u", "id", "", "", "") {
		if a == "--proxy" {
			t.Fatal("--proxy must not appear for a direct connection")
		}
	}
}

// hasProxy reports whether argv carries the given proxy behind --proxy.
func hasProxy(argv []string, proxy string) bool {
	for i, a := range argv {
		if a == "--proxy" && i+1 < len(argv) && argv[i+1] == proxy {
			return true
		}
	}
	return false
}

// writeStub drops a minimal valid file so collectYTDLP has something to find.
func writeStub(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatalf("write stub: %v", err)
	}
}
