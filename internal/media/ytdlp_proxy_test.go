package media

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// youtubeBotCheck stands in for yt-dlp's real exit status 1 on a bot wall.
var youtubeBotCheck = errors.New("exit status 1")

// newYtdlpTestManager builds a manager whose subprocess is faked, so the argv
// contract is testable on a host with no yt-dlp installed.
func newYtdlpTestManager(t *testing.T, opts Options, fn func(context.Context, []string) ([]byte, []byte, error)) *Manager {
	t.Helper()
	opts.TTL = time.Minute
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Same DNS pin as testManager: public hosts must resolve to a public
	// address regardless of the network the suite runs on.
	m.lookupFn = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	t.Cleanup(func() { m.Close() })
	m.opts.YTDLPPath = "yt-dlp-probe"
	m.ytdlpFn = fn
	m.retryDelay = func(int) time.Duration { return 0 }
	return m
}

// The proxy contract is per-ROUTE now: the ladder tries the direct route
// first, so the primary proxy is only present on the argv of the run that
// carries it. The fake returns an egress-shaped refusal for the direct route
// (exactly the shape that advances the ladder) and succeeds over the proxy.
func TestYTDLPProxyIsPassed(t *testing.T) {
	// The bot-check in this scenario is an account gate; a jar is set so the
	// ladder keeps advancing to the proxy leg.
	jar := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(jar, []byte("# jar\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.CookiesFile = jar
	opts.Proxy = "http://172.17.0.1:3339"
	var got []string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		got = append([]string(nil), args...)
		// Direct route: YouTube bot-checks the unproxied egress.
		if !hasProxy(args, opts.Proxy) {
			return nil, []byte("ERROR: [youtube] X: Sign in to confirm you're not a bot"), youtubeBotCheck
		}
		// Over the configured proxy: succeed.
		writeStub(t, dir+"/vid.mp4")
		return []byte("136,1280,720,30,avc1.64001f,mp4a.40.2,mp4,1500\n"), nil, nil
	})
	if _, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	}); err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	idx := -1
	for i, a := range got {
		if a == "--proxy" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("--proxy missing from argv: %q", got)
	}
	if idx+1 >= len(got) || got[idx+1] != opts.Proxy {
		t.Fatalf("--proxy value missing or wrong, want %q (argv %q)", opts.Proxy, got)
	}
}

// No proxy configured means no --proxy flag: never invent an empty value.
func TestYTDLPOmitsProxyWhenUnset(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	var got []string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		got = append([]string(nil), args...)
		return nil, nil, youtubeBotCheck
	})
	_, _ = m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
	for _, a := range got {
		if a == "--proxy" {
			t.Fatalf("unexpected --proxy in argv: %q", got)
		}
	}
}

// Defect 2: modern uploads are DASH-only, so a selector that needs one stream
// carrying both codecs fails with "Requested format is not available".
func TestFormatSelectorMergesDASHStreams(t *testing.T) {
	sel := FormatSelectorFor(QualityAuto, true)
	if !strings.Contains(sel, "bv*") {
		t.Fatalf("selector cannot pick a video-only stream: %q", sel)
	}
	if !strings.Contains(sel, "+ba") {
		t.Fatalf("selector cannot merge an audio-only stream: %q", sel)
	}
	if !strings.Contains(sel, "b[ext=mp4][acodec!=none]") {
		t.Fatalf("selector lost the progressive-mp4 fallback: %q", sel)
	}
	if !strings.Contains(sel, "vcodec^=avc1") {
		t.Fatalf("selector does not prefer h264: %q", sel)
	}
}

// Defect 3: the operator only saw "yt-dlp: exit status 1". yt-dlp's own
// diagnosis is on stderr and must travel with the error.
func TestYTDLPErrorCarriesStderr(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	m := newYtdlpTestManager(t, opts, func(_ context.Context, _ []string) ([]byte, []byte, error) {
		return nil, []byte("WARNING: something\nERROR: [youtube] abc: Sign in to confirm you are not a bot.\n"), youtubeBotCheck
	})
	_, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "not a bot") {
		t.Fatalf("error hides yt-dlp's diagnosis: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error lost the exec failure: %v", err)
	}
}

// Defect 4: --print-json implies --simulate, so yt-dlp printed metadata and
// wrote no file; the session died at ~3s with "no output file with the
// expected name" and an empty stderr.
func TestYTDLPNeverSimulates(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	var got []string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		got = append([]string(nil), args...)
		return nil, nil, youtubeBotCheck
	})
	_, _ = m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
	for _, a := range got {
		if a == "--print-json" || a == "--simulate" {
			t.Fatalf("argv simulates instead of downloading: %q", got)
		}
	}
	found := false
	for _, a := range got {
		if a == "--no-simulate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("--no-simulate missing from argv: %q", got)
	}
}

// Defect 5: the size guard used --match-filter on `filesize`, which does not
// exist for DASH-merged selections (only filesize_approx), so every format was
// rejected silently. The guard must abort the download instead of skipping it.
func TestYTDLPSizeLimitUsesMaxFilesize(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.MaxBytes = 512 * 1024 * 1024
	var got []string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		got = append([]string(nil), args...)
		return nil, nil, youtubeBotCheck
	})
	_, _ = m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
	for _, a := range got {
		if a == "--match-filter" {
			t.Fatalf("--match-filter silently drops DASH formats: %q", got)
		}
	}
	idx := -1
	for i, a := range got {
		if a == "--max-filesize" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("--max-filesize missing from argv: %q", got)
	}
	if idx+1 >= len(got) || got[idx+1] != "536870912" {
		t.Fatalf("--max-filesize value wrong, want 536870912 (argv %q)", got)
	}
}

// The merge rung must stay cheap enough for a 2-vCPU host.
func TestFormatSelectorCapsResolution(t *testing.T) {
	if sel := FormatSelectorFor(QualityAuto, true); !strings.Contains(sel, "height<=720") {
		t.Fatalf("auto selector does not cap resolution: %q", sel)
	}
	if sel := FormatSelectorFor(QualityBalanced, true); !strings.Contains(sel, "height<=720") {
		t.Fatalf("balanced selector does not cap resolution: %q", sel)
	}
}

// Format selector must provide an ultimate fallback rung so yt-dlp never dies
// with "Requested format is not available" when only single-stream or unusual formats exist.
func TestFormatSelectorHasFallback(t *testing.T) {
	sel := FormatSelectorFor(QualityAuto, true)
	if !strings.HasSuffix(sel, "bv*/ba/b*") {
		t.Fatalf("selector missing final single-stream fallback rung: %q", sel)
	}
	// Without ffmpeg the ladder must degrade to progressive-only formats:
	// requesting a merge yt-dlp cannot perform would just fail the download.
	saver := FormatSelectorFor(QualitySaver, false)
	if strings.Contains(saver, "+ba") {
		t.Fatalf("ffmpeg-less ladder must not request merges: %q", saver)
	}
	if !strings.Contains(saver, "b[acodec!=none]") {
		t.Fatalf("ffmpeg-less ladder lost the progressive fallback: %q", saver)
	}
}

// Defect: transient network resets killed the download immediately. yt-dlp must
// be configured to retry extraction and fragments before aborting.
func TestYTDLPRetryArguments(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	var got []string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		got = append([]string(nil), args...)
		return nil, nil, youtubeBotCheck
	})
	_, _ = m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
	hasFlag := func(flag, val string) bool {
		for i, a := range got {
			if a == flag && i+1 < len(got) && got[i+1] == val {
				return true
			}
		}
		return false
	}
	hasAny := func(flag string) bool {
		for _, a := range got {
			if a == flag {
				return true
			}
		}
		return false
	}
	if !hasFlag("--extractor-retries", "3") {
		t.Fatalf("--extractor-retries 3 missing from argv: %q", got)
	}
	if !hasFlag("--file-access-retries", "3") {
		t.Fatalf("--file-access-retries 3 missing from argv: %q", got)
	}
	// Defect: a single dropped TLS handshake ("TLS/SSL connection has been
	// closed (EOF)") killed the whole Download & Stream session because the
	// general network retry count was left at its default and the sleep was
	// untyped. -R/--retries is what covers HTTP/TLS resets, and the sleeps
	// must be typed per retry kind.
	if !hasFlag("--retries", "10") {
		t.Fatalf("--retries 10 missing from argv: %q", got)
	}
	if !hasFlag("--retry-sleep", "http:1:4") {
		t.Fatalf("typed http retry-sleep missing from argv: %q", got)
	}
	if !hasFlag("--retry-sleep", "extractor:1") {
		t.Fatalf("typed extractor retry-sleep missing from argv: %q", got)
	}
	if !hasFlag("--retry-sleep", "file_access:1") {
		t.Fatalf("typed file_access retry-sleep missing from argv: %q", got)
	}
	// A hung socket must not pin the download until the process timeout.
	if !hasFlag("--socket-timeout", "30") {
		t.Fatalf("--socket-timeout 30 missing from argv: %q", got)
	}
	// --force-ipv4 must NOT be in the argv. YouTube signs each googlevideo
	// URL with the IP that extracted it (`ip=` param). Forcing the download
	// socket to IPv4 makes the body fetch arrive from a different family
	// (and, on a rotating egress like Tor, a different exit node entirely),
	// so googlevideo 403s the transfer mid-body — verified live: the same
	// URL streams cleanly at 4.7 MiB/s without the flag and dies at 94%
	// with it.
	if hasAny("--force-ipv4") {
		t.Fatalf("--force-ipv4 present in argv: %q", got)
	}
}

// Subprocess failures should attempt retries with backoff.
func TestYTDLPRetriesWithBackoff(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	attempts := 0
	var delays []int
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		attempts++
		return nil, []byte("ERROR: [youtube] connection reset"), youtubeBotCheck
	})
	m.retryDelay = func(attempt int) time.Duration {
		delays = append(delays, attempt)
		return 0
	}
	_, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
	if err == nil {
		t.Fatal("expected failure after exhausted retries")
	}
	if attempts != 3 { // initial attempt + 2 retries
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if len(delays) != 2 || delays[0] != 1 || delays[1] != 2 {
		t.Fatalf("expected backoff delays for attempts 1 and 2, got %v", delays)
	}
}
