package media

import (
	"context"
	"errors"
	"os/exec"
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
	t.Cleanup(func() { m.Close() })
	m.opts.YTDLPPath = "yt-dlp-probe"
	m.ytdlpFn = fn
	return m
}

// Defect 1: the download left the box on the bare datacenter IP, so YouTube
// answered "Sign in to confirm you're not a bot" and the session died with
// "yt-dlp: exit status 1". The configured proxy must reach the argv.
func TestYTDLPProxyIsPassed(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Proxy = "http://172.17.0.1:3339"
	var got []string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		got = append([]string(nil), args...)
		return nil, nil, exec.ErrNotFound
	})
	_, _ = m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
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
	m := &Manager{opts: DefaultOptions(t.TempDir())}
	sel := m.formatSelector()
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
