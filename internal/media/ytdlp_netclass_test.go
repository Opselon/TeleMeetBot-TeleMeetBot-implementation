package media

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// Network failure classification. The task demands that TLS EOF, connection
// reset, timeout and blocked egress each be classified correctly: an
// egress-shaped failure advances the ladder (another route may work), a
// transient one retries in place, and a real verdict fails fast. Miss-classifying
// any of them either burns three slow runs over a dead proxy or kills a
// session whose download could have recovered.

func TestProxyBlockedClassifiesEgressShapedFailures(t *testing.T) {
	// Every one of these is a source-refused egress, not a verdict on the
	// video: the ladder must advance.
	blocked := []string{
		"ERROR: [youtube] X: Unable to download API page: TLS/SSL connection has been closed (EOF) (_ssl.c:992)",
		"ERROR: [youtube] X: Unable to download API page: 15036 bytes read (caused by <IncompleteRead: 15036 bytes read>)",
		"ERROR: [youtube] X: Sign in to confirm you're not a bot",
		"ERROR: [youtube] X: Sign in to confirm you’re not a bot",
		"ERROR: [youtube] X: unable to download video data: HTTP Error 403: Forbidden",
		"ERROR: [youtube] X: unable to download webpage: HTTP Error 429: Too Many Requests",
		"ERROR: [youtube] X: unable to connect to proxy",
		"ERROR: ProxyError: cannot connect to proxy",
		"ERROR: [youtube] X: SSLError: connection has been closed",
		"ERROR: [youtube] X: connection reset by peer",
	}
	for _, stderr := range blocked {
		if !proxyBlocked([]byte(stderr)) {
			t.Errorf("proxyBlocked() must classify as egress-shaped: %q", stderr)
		}
	}
}

func TestProxyBlockedKeepsRealVerdictsFastFailing(t *testing.T) {
	// A genuine extractor verdict never changes with the route: advancing the
	// ladder would only burn three slow runs over proxies that cannot help.
	verdicts := []string{
		"ERROR: [youtube] X: Requested format is not available",
		"ERROR: Unsupported URL: https://example.com/",
		"ERROR: [youtube] X: Video unavailable",
		"ERROR: [youtube] X: The page needs to be reloaded.",
		"ERROR: [youtube] X: Join this channel to get access to members-only content",
	}
	for _, stderr := range verdicts {
		if proxyBlocked([]byte(stderr)) {
			t.Errorf("proxyBlocked() must NOT classify a real verdict as egress-shaped: %q", stderr)
		}
	}
}

func TestTransientFailureClassification(t *testing.T) {
	transient := []string{
		"ERROR: [youtube] X: connection reset by peer",
		"ERROR: [youtube] X: broken pipe",
		"ERROR: [youtube] X: Unable to download API page: TLS/SSL connection has been closed (EOF)",
	}
	for _, stderr := range transient {
		if !transientFailure(exec.ErrNotFound, []byte(stderr)) {
			t.Errorf("transientFailure() must classify as transient: %q", stderr)
		}
	}
	if transientFailure(nil, []byte("ERROR: Unsupported URL")) {
		t.Error("a real verdict must not classify as transient")
	}
}

// Terminal: a TLS EOF on the direct route must not kill the download. The
// ladder advances to the configured proxy, which succeeds — a real recovery
// path on hosts whose unproxied route YouTube tears down mid-handshake.
func TestTLSEOFOnDirectAdvancesLadder(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Proxy = "http://172.17.0.1:3339"

	var order []string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		p := proxyOf(args)
		order = append(order, p)
		if p == "" {
			// A socket torn down mid-handshake on the direct route.
			return nil, []byte("ERROR: [youtube] X: Unable to download API page: TLS/SSL connection has been closed (EOF) (_ssl.c:992)"), exec.ErrNotFound
		}
		writeStub(t, dir+"/vid.mp4")
		return []byte("136,1280,720,30,avc1.64001f,mp4a.40.2,mp4,1500\n"), nil, nil
	})

	res, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
	if err != nil {
		t.Fatalf("a TLS EOF on the direct route must not fail the download: %v", err)
	}
	if res == nil || res.Path == "" {
		t.Fatal("the recovered download must produce a file")
	}
	if len(order) == 0 || order[0] != "" {
		t.Fatalf("the direct route must be tried first, order %q", order)
	}
	// The direct route is never recorded in the blocked registry: there is no
	// alternative to "direct", so a mark would only poison future lookups.
	// Asserting it stays unmarked guards that contract.
	if m.EgressBlocked("") {
		t.Error("the direct route must never be marked blocked (no alternative exists)")
	}
}

// Blocked egress: a timeout must not be misclassified as a verdict.
func TestSocketTimeoutAdvancesLadder(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Proxy = "http://172.17.0.1:3339"

	var sawTimeout bool
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		if proxyOf(args) == "" {
			sawTimeout = true
			return nil, []byte("ERROR: [youtube] X: unable to download API page: <urlopen error timed out>"), exec.ErrNotFound
		}
		writeStub(t, dir+"/vid.mp4")
		return []byte("136,1280,720,30,avc1.64001f,mp4a.40.2,mp4,1500\n"), nil, nil
	})

	if _, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	}); err != nil {
		t.Fatalf("a socket timeout on the direct route must not fail the download: %v", err)
	}
	if !sawTimeout {
		t.Fatal("the fake never observed the direct route")
	}
}

// The retry counters must stay bounded: with transient failures on every
// route, the total number of subprocess runs must be routes x (1+maxExecRetries)
// — no infinite loop, no unbounded backoff.
func TestRetriesStayBoundedAcrossLadder(t *testing.T) {
	opts := DefaultOptions(t.TempDir())
	opts.Proxy = "http://172.17.0.1:3339"
	opts.FallbackProxies = []string{"socks5://172.17.0.1:9050"}

	var calls int
	m := newYtdlpTestManager(t, opts, func(context.Context, []string) ([]byte, []byte, error) {
		calls++
		return nil, []byte("ERROR: [youtube] X: connection reset by peer"), exec.ErrNotFound
	})

	_, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
	if err == nil {
		t.Fatal("expected failure when every route resets the connection")
	}
	// 3 routes x (1 initial + maxExecRetries) = 9 bounded runs.
	max := 3 * (1 + maxExecRetries)
	if calls > max {
		t.Fatalf("retries are unbounded: %d subprocess runs, expected at most %d", calls, max)
	}
	if calls < 3 {
		t.Fatalf("every route must be visited at least once, got %d runs", calls)
	}
	// The failure must name the ladder so the operator can diagnose it.
	for _, want := range []string{"direct", opts.Proxy, "egress ladder tried"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q, got: %s", want, err.Error())
		}
	}
}

// A missing yt-dlp binary must fail fast across the whole ladder, not retry
// per route: there is no route that makes a missing binary work.
func TestMissingYTDLPFailsFastAcrossLadder(t *testing.T) {
	opts := DefaultOptions(t.TempDir())
	opts.Proxy = "http://172.17.0.1:3339"
	opts.FallbackProxies = []string{"socks5://172.17.0.1:9050"}

	var calls int
	m := newYtdlpTestManager(t, opts, func(context.Context, []string) ([]byte, []byte, error) {
		calls++
		return nil, nil, exec.ErrNotFound
	})
	// Make YTDLP() report the binary as explicit-but-missing, exactly the
	// historical "no yt-dlp installed" failure.
	m.opts.YTDLPPath = "yt-dlp-probe"

	_, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
	if err == nil {
		t.Fatal("a missing yt-dlp must fail the download")
	}
	// ErrMissingTool aborts before the ladder runs, so exactly one run.
	if calls > 1 {
		t.Fatalf("a missing binary must fail fast, got %d runs", calls)
	}
}
