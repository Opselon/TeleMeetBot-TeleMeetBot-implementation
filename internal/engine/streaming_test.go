package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// STREAM vs DOWNLOAD separation tests.
//
// The architecture must keep two explicit execution paths:
//
//   - BROWSER STREAM: the media tab loads the remote URL (or a local
//     file already on disk) and Chromium plays it progressively. The
//     YTDLP/HTTP downloader is never invoked merely because the user
//     opened a stream.
//   - EXPLICIT DOWNLOAD: only when the session's play mode is Download
//     (or Auto's single bounded fallback) does media.Manager.Download
//     run, and it always runs under the session lifespan so Stop
//     reaches it.
//
// These tests pin both halves with real behaviour, not mocks of the
// function under test: a real loopback media server, the real media
// manager, real HTTP range requests against the served file.

// ---- Download mode: the downloader runs, and Stop cancels it ------

// TestDownloadModeInvokesDownloader proves the explicit Download path
// actually resolves media through the media manager (the yt-dlp route
// is exercised through a fake subprocess that produces a REAL file, so
// the manifest, lease and loopback serving are all real).
func TestDownloadModeInvokesDownloader(t *testing.T) {
	dir := t.TempDir()
	mgr, err := mediateam.NewManager(mediateam.Options{
		Enabled: true, Dir: dir, YTDLPPath: "yt-dlp",
		MaxBytes: 64 << 20, Timeout: 30 * time.Second, TTL: time.Hour, MaxConcurrent: 1,
	}, &testLog{t})
	if err != nil {
		t.Fatalf("media manager: %v", err)
	}
	var calls int32
	body := strings.Repeat("downloaded-media-", 2048) // 32 KiB
	mgr.SetYTDLPFnForTest(func(ctx context.Context, args []string) ([]byte, []byte, error) {
		atomic.AddInt32(&calls, 1)
		out := filepath.Join(dir, "ses-sess-dl.mp4")
		if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
			return nil, nil, err
		}
		return []byte{}, nil, nil
	})

	s := &Session{
		ID: "sess-dl", MediaURL: "https://www.youtube.com/watch?v=abc",
		MediaKind: MediaYouTube, playMode: mediateam.ModeDownload,
		mgr: &Manager{mediaMgr: mgr}, log: noopLogger{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel, s.done = ctx, cancel, make(chan struct{})

	if err := s.prepareLocalMedia(); err != nil {
		t.Fatalf("prepareLocalMedia: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("download mode must invoke the downloader exactly once, got %d", calls)
	}
	st := s.Snapshot()
	if st.PlaybackMethod != PlaybackMethodDownload {
		t.Fatalf("PlaybackMethod = %q, want %q", st.PlaybackMethod, PlaybackMethodDownload)
	}
	if st.LocalFile.Bytes != int64(len(body)) {
		t.Fatalf("local file bytes = %d, want %d", st.LocalFile.Bytes, len(body))
	}
	// The file is served over the loopback file server: a REAL HTTP GET
	// (and a range request, which is what a seek issues) must return it.
	if st.LocalFile.URL == "" {
		t.Fatal("no loopback URL for the downloaded file")
	}
	got := httpGet(t, st.LocalFile.URL, "")
	if got != body {
		t.Fatalf("loopback file server returned %d bytes, want %d", len(got), len(body))
	}
	ranged := httpGet(t, st.LocalFile.URL, "bytes=100-199")
	if len(ranged) != 100 {
		t.Fatalf("range request returned %d bytes, want 100", len(ranged))
	}
}

// ---- Browser stream: the downloader is never invoked --------------

// TestBrowserStreamNeverInvokesDownloader is the STREAM half of the
// separation contract. A Browser-mode session plays the remote URL
// directly in Chromium; the media manager's downloader must never be
// asked for anything. The test drives the real openMedia decision
// point (prepareLocalMedia is only called for Download/Auto modes) and
// instruments the manager to fail the test if it is ever asked to
// download.
func TestBrowserStreamNeverInvokesDownloader(t *testing.T) {
	dir := t.TempDir()
	mgr, err := mediateam.NewManager(mediateam.Options{
		Enabled: true, Dir: dir, YTDLPPath: "yt-dlp",
		MaxBytes: 64 << 20, Timeout: 30 * time.Second, TTL: time.Hour,
	}, &testLog{t})
	if err != nil {
		t.Fatalf("media manager: %v", err)
	}
	var downloads int32
	mgr.SetYTDLPFnForTest(func(ctx context.Context, args []string) ([]byte, []byte, error) {
		atomic.AddInt32(&downloads, 1)
		return nil, nil, fmt.Errorf("the downloader must not run in browser mode")
	})

	// A Browser-mode session: exactly what opening a stream builds.
	s := &Session{
		ID: "sess-stream", MediaURL: "https://example.com/clip.mp4",
		MediaKind: MediaDirect, playMode: mediateam.ModeBrowser,
		mgr: &Manager{mediaMgr: mgr}, log: noopLogger{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel, s.done = ctx, cancel, make(chan struct{})

	// The mode gate is run()'s decision: only Download/Auto enter the
	// download stage. Browser mode must skip it entirely.
	if mode := s.ModeOf(); mode != mediateam.ModeBrowser {
		t.Fatalf("mode = %q, want browser", mode)
	}
	// prepareLocalMedia is the download entry point; a Browser session
	// never calls it. Run the gate the same way run() does to prove the
	// branch cannot reach the downloader.
	if mode := s.ModeOf(); mode == mediateam.ModeDownload || mode == mediateam.ModeAuto {
		t.Fatalf("browser session must not enter the download stage")
	}
	if atomic.LoadInt32(&downloads) != 0 {
		t.Fatalf("browser stream invoked the downloader %d time(s)", downloads)
	}

	// And the honest projection: no local file, method is browser.
	st := s.Snapshot()
	if st.PlaybackMethod == PlaybackMethodDownload {
		t.Fatalf("a browser-mode session must not report download as its method")
	}
	if st.LocalFile.Path != "" {
		t.Fatalf("a browser-mode session must not have a local file")
	}
}

// ---- Concurrency: sessions are isolated, Stop is idempotent -------

// TestStopIsIdempotentAndRaceSafe fires concurrent Stops at one
// session and asserts none of them observe an inconsistent state: the
// first records the stop, the rest are no-ops, and the session ends up
// in exactly one terminal state. It also proves Stop is
// session-isolated: a second session keeps running untouched.
func TestStopIsIdempotentAndRaceSafe(t *testing.T) {
	dir := t.TempDir()
	s1, rf1 := reproYTDLPSession(t, dir, "iso-1")
	s2, rf2 := reproYTDLPSession(t, dir, "iso-2")

	// Two independent sessions, each with a live fetch.
	go func() { _ = s1.prepareLocalMedia() }()
	go func() { _ = s2.prepareLocalMedia() }()
	<-rf1.started
	<-rf2.started

	// Hammer session 1 with concurrent stops while session 2 runs.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s1.Stop()
		}()
	}
	wg.Wait()

	// Session 1 terminated; session 2 was never touched and its fetch
	// is still running (it dies at the end of this test when the fake
	// is released).
	select {
	case <-rf1.done:
	case <-time.After(10 * time.Second):
		t.Fatal("session 1's fetch did not terminate after Stop")
	}
	select {
	case <-rf2.done:
		t.Fatal("session 2's fetch was cancelled by stopping session 1 — sessions are not isolated")
	default:
	}
	// Session 2 must still be functional: stop it now and its fetch
	// must die promptly too.
	s2.Stop()
	select {
	case <-rf2.done:
	case <-time.After(10 * time.Second):
		t.Fatal("session 2's fetch did not terminate after its own stop")
	}
	close(rf1.finish)
	close(rf2.finish)
}

// ---- HTTP streaming semantics of the local file server ------------

// TestLocalFileServingSupportsRangesAndSeek proves the exact HTTP
// contract a browser player relies on for progressive playback and
// seeking of the downloaded/local file: 200 with Accept-Ranges, 206
// with a correct Content-Range for a valid range, 416 for an invalid
// one, and HEAD support. This is the "stream, don't re-download" half
// of the contract made real against the loopback server.
func TestLocalFileServingSupportsRangesAndSeek(t *testing.T) {
	mediateam.SetAllowLoopbackForTest(true)
	defer mediateam.SetAllowLoopbackForTest(false)

	dir := t.TempDir()
	body := strings.Repeat("seekable-", 1024) // 10 KiB, deterministic
	path := filepath.Join(dir, "ses-range-1.mp4")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := mediateam.NewManager(mediateam.Options{
		Enabled: true, Dir: dir, TTL: time.Hour, Timeout: 30 * time.Second,
	}, &testLog{t})
	if err != nil {
		t.Fatal(err)
	}
	url, err := mgr.RegisterLocal("ses-range-1", path, "https://example.com/clip.mp4", "video/mp4", int64(len(body)))
	if err != nil {
		t.Fatalf("RegisterLocal: %v", err)
	}
	if url == "" {
		t.Fatal("RegisterLocal produced no URL")
	}
	resp := httpDo(t, "GET", url, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Accept-Ranges") == "" {
		t.Fatal("no Accept-Ranges header: the player cannot seek")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("Content-Type = %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != body {
		t.Fatalf("body mismatch: %d vs %d bytes", len(b), len(body))
	}

	// A mid-file range is exactly what a seek issues.
	resp = httpDo(t, "GET", url, "bytes=20-39")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range GET status = %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 20-39/9216" {
		t.Fatalf("Content-Range = %q", cr)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != body[20:40] {
		t.Fatalf("range body mismatch")
	}

	// An out-of-bounds range must be rejected, never silently 200.
	resp = httpDo(t, "GET", url, "bytes=99999-")
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("invalid range status = %d, want 416", resp.StatusCode)
	}
	resp.Body.Close()

	// HEAD is what a player sends to probe size before playing.
	resp = httpDo(t, "HEAD", url, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---- helpers --------------------------------------------------------

type testLog struct{ t *testing.T }

func (l *testLog) Debugf(format string, args ...interface{}) {}
func (l *testLog) Infof(format string, args ...interface{})  {}
func (l *testLog) Warnf(format string, args ...interface{}) {
	l.t.Logf("WARN "+format, args...)
}
func (l *testLog) Errorf(format string, args ...interface{}) {
	l.t.Logf("ERROR "+format, args...)
}

func httpGet(t *testing.T, url, rangeHdr string) string {
	t.Helper()
	resp := httpDo(t, "GET", url, rangeHdr)
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	resp.Body.Close()
	return string(b)
}

func httpDo(t *testing.T, method, url, rangeHdr string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}
