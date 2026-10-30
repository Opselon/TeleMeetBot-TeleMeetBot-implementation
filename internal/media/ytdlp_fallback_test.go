package media

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// The defect: a YouTube download died with
// "Unable to download API page: 15036 bytes read (caused by <IncompleteRead>)"
// and the session went straight to error. That failure is a per-IP rate limit
// on the egress, not a verdict on the video: the same URL extracts fine over a
// different route. A configured fallback must be used.
func TestYTDLPFallsBackToAlternateEgress(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Proxy = "http://172.17.0.1:3339"
	opts.FallbackProxies = []string{"socks5://172.17.0.1:9050"}

	var calls [][]string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		calls = append(calls, append([]string(nil), args...))
		// First egress: YouTube truncates the innertube response mid-body.
		if hasProxy(args, opts.Proxy) {
			return nil, []byte("ERROR: [youtube] DIjPGEQKbH8: Unable to download API page: 15036 bytes read (caused by <IncompleteRead: 15036 bytes read>); please report this issue"), exec.ErrNotFound
		}
		// Second egress: extraction succeeds and the file lands on disk.
		writeStub(t, dir+"/vid.mp4")
		return []byte("136,1280,720,30,avc1.64001f,mp4a.40.2,mp4,1500\n"), nil, nil
	})

	if _, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	}); err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if len(calls) < 2 {
		t.Fatalf("expected the fallback egress to be tried, got %d call(s)", len(calls))
	}
	if !hasProxy(calls[0], opts.Proxy) {
		t.Fatalf("first run must use the primary proxy, argv: %q", calls[0])
	}
	if !hasProxy(calls[len(calls)-1], opts.FallbackProxies[0]) {
		t.Fatalf("last run must use the fallback proxy, argv: %q", calls[len(calls)-1])
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
