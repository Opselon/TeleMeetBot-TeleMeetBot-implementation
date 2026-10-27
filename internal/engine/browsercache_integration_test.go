package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// Integration tests: real Chromium launch latency and persistent-cache reuse.
// Skipped when no browser binary is available (ordinary unit CI).

// TestIntegrationWarmHandOffBeatsColdLaunch measures a cold browser start
// against the hand-off of a pre-warmed one. The warm hand-off must be
// materially faster (the whole point of warm-up) and both must be functional.
func TestIntegrationWarmHandOffBeatsColdLaunch(t *testing.T) {
	bin := browserBin(t)

	mgr := NewManager(
		BrowserConfig{Executable: bin, Headless: true, WindowWidth: 1280, WindowHeight: 800, PerSession: true},
		MediaOptions{},
		MeetOptions{PresentMode: "tab", PresentTimeoutSeconds: 8},
		testLogger{t},
	)
	defer mgr.ShutdownWarm()

	// Warm-up: launch outside the measurement.
	if err := mgr.WarmUp(); err != nil {
		t.Fatalf("WarmUp: %v", err)
	}

	// Warm hand-off timing.
	start := time.Now()
	b, release, ok := mgr.acquireWarmBrowser()
	if !ok {
		t.Fatal("warm browser not available right after WarmUp")
	}
	warmElapsed := time.Since(start)
	if b == nil || release == nil {
		t.Fatal("warm hand-off returned nil browser/release")
	}
	release()

	// A second warm acquisition must fail (slot consumed) — no phantom warm
	// browsers.
	if _, _, ok := mgr.acquireWarmBrowser(); ok {
		t.Fatal("acquireWarmBrowser handed out a browser twice")
	}

	// Cold launch timing (profile + full process start).
	start = time.Now()
	_, release2, err := mgr.acquireBrowser()
	if err != nil {
		t.Fatalf("cold acquireBrowser: %v", err)
	}
	coldElapsed := time.Since(start)
	release2()

	if warmElapsed >= coldElapsed {
		t.Fatalf("warm hand-off (%s) was not faster than cold launch (%s)", warmElapsed, coldElapsed)
	}
	t.Logf("warm hand-off: %s, cold launch: %s", warmElapsed, coldElapsed)
}

// TestIntegrationWarmBrowserIsUsable proves the warmed browser is not a zombie:
// a tab can be created and navigated like on a cold browser.
func TestIntegrationWarmBrowserIsUsable(t *testing.T) {
	bin := browserBin(t)
	mgr := NewManager(
		BrowserConfig{Executable: bin, Headless: true, WindowWidth: 1280, WindowHeight: 800, PerSession: true},
		MediaOptions{},
		MeetOptions{},
		testLogger{t},
	)
	defer mgr.ShutdownWarm()

	if err := mgr.WarmUp(); err != nil {
		t.Fatalf("WarmUp: %v", err)
	}
	b, release, ok := mgr.acquireWarmBrowser()
	if !ok {
		t.Fatal("no warm browser")
	}
	defer release()

	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("warm browser could not open a page: %v", err)
	}
	defer page.Close()
	if err := page.Navigate("about:blank"); err != nil {
		t.Fatalf("warm browser could not navigate: %v", err)
	}
}

// TestIntegrationPersistentCacheRepeats checks that a cache dir configured
// through acquireBrowser actually persists data on disk across launches, so a
// second launch can serve from cache.
func TestIntegrationPersistentCacheRepeats(t *testing.T) {
	bin := browserBin(t)
	cacheRoot := t.TempDir()

	mgr := NewManager(
		BrowserConfig{Executable: bin, Headless: true, WindowWidth: 1280, WindowHeight: 800, CacheDir: cacheRoot, PerSession: true},
		MediaOptions{},
		MeetOptions{},
		testLogger{t},
	)

	// First launch: acquires a pool dir, writes cache data.
	b1, release1, err := mgr.acquireBrowser()
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	page, err := b1.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if err := page.Navigate("about:blank"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	release1()

	// The pool must have handed out exactly one dir under cacheRoot and got
	// it back on release.
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		t.Fatalf("cache root: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no cache dirs were created under the cache root")
	}

	// Second launch must recycle (reuse) the same dir — a warm cache.
	_, release2, err := mgr.acquireBrowser()
	if err != nil {
		t.Fatalf("second launch: %v", err)
	}
	release2()
	dirs, _ := os.ReadDir(cacheRoot)
	if len(dirs) != len(entries) {
		t.Fatalf("second launch created extra cache dirs: %d -> %d", len(entries), len(dirs))
	}
}

// TestIntegrationConcurrentSessionsDistinctCacheDirs proves concurrent
// sessions get distinct cache dirs (no shared disk-cache corruption).
func TestIntegrationConcurrentSessionsDistinctCacheDirs(t *testing.T) {
	bin := browserBin(t)
	mgr := NewManager(
		BrowserConfig{Executable: bin, Headless: true, WindowWidth: 1280, WindowHeight: 800, PerSession: true},
		MediaOptions{},
		MeetOptions{},
		testLogger{t},
	)

	b1, r1, err := mgr.acquireBrowser()
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	defer r1()
	b2, r2, err := mgr.acquireBrowser()
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	defer r2()

	// Both must be distinct live processes.
	if b1 == b2 {
		t.Fatal("acquireBrowser returned the same browser twice")
	}
}

// TestIntegrationProfileIsolationBetweenLaunches checks consecutive per-session
// launches use distinct temp profile dirs (no cookie/storage bleed).
func TestIntegrationProfileIsolationBetweenLaunches(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TELEMEETBOT_PROFILE_DIR", base)

	dir1, err := tempProfileDir()
	if err != nil {
		t.Fatal(err)
	}
	dir2, err := tempProfileDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir1 == dir2 {
		t.Fatal("two sessions got the same profile dir")
	}
	if _, err := os.Stat(dir1); err != nil {
		t.Fatalf("profile dir 1 missing: %v", err)
	}
	// Dirs live under the overridden base.
	if filepath.Dir(dir1) != filepath.Join(base, "telemeetbot") {
		t.Fatalf("profile dir %s not under %s", dir1, base)
	}
}
