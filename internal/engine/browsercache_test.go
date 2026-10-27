package engine

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/config"
)

// TestBrowserCachePoolExclusiveCovered pins the core invariant: two concurrent
// acquires must never receive the same cache directory — two live Chromium
// processes writing one disk-cache dir corrupts the cache.
func TestBrowserCachePoolExclusive(t *testing.T) {
	pool := newBrowserCachePool(t.TempDir())

	const workers = 8
	held := make([]string, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]bool{}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir, err := pool.acquire()
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			if seen[dir] {
				t.Errorf("cache dir %s handed out twice concurrently", dir)
			}
			seen[dir] = true
			mu.Unlock()
			held[i] = dir
		}(i)
	}
	wg.Wait()

	if len(seen) != workers {
		t.Fatalf("got %d distinct dirs for %d workers", len(seen), workers)
	}
	for _, dir := range held {
		pool.release(dir)
	}
	live, _, _ := pool.stats()
	if live != 0 {
		t.Fatalf("live=%d after all releases, want 0", live)
	}
}

// TestBrowserCachePoolRecyclesWarmestFirst checks that a released dir is
// handed out again (warm cache reuse) rather than always creating new ones.
func TestBrowserCachePoolRecyclesWarmestFirst(t *testing.T) {
	pool := newBrowserCachePool(t.TempDir())
	dir1, err := pool.acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir1, "marker"), []byte("warm"), 0o600); err != nil {
		t.Fatal(err)
	}
	pool.release(dir1)

	dir2, err := pool.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.release(dir2)
	if dir1 != dir2 {
		t.Fatalf("pool created a new dir %s instead of recycling %s", dir2, dir1)
	}
	if _, err := os.Stat(filepath.Join(dir2, "marker")); err != nil {
		t.Fatalf("recycled dir lost its warm contents: %v", err)
	}
}

// TestBrowserCachePoolTrims checks the free-dir bound.
func TestBrowserCachePoolTrims(t *testing.T) {
	pool := newBrowserCachePool(t.TempDir())
	for i := 0; i < browserCacheMaxDirs+3; i++ {
		dir, err := pool.acquire()
		if err != nil {
			t.Fatal(err)
		}
		pool.release(dir)
	}
	_, free, _ := pool.stats()
	if free > browserCacheMaxDirs {
		t.Fatalf("free=%d exceeds bound %d", free, browserCacheMaxDirs)
	}
}

// TestSweepStaleCacheDirs checks crash recovery: dirs older than the cutoff
// are removed, recent ones survive.
func TestSweepStaleCacheDirs(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "ns-old")
	newDir := filepath.Join(root, "ns-new")
	for _, d := range []string{oldDir, newDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldDir, stale, stale); err != nil {
		t.Fatal(err)
	}
	SweepStaleCacheDirs(root, time.Hour)
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("stale cache dir was not swept")
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Fatalf("recent cache dir was swept: %v", err)
	}
}

// TestFromConfigCacheDirDefaults verifies backward compatibility: with no
// disk_cache key the persistent cache is enabled under <data_dir>/browser-cache,
// and disk_cache:false disables it.
func TestFromConfigCacheDirDefaults(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.DataDir = t.TempDir()
	b, _, _ := FromConfig(cfg)
	want := filepath.Join(cfg.Paths.DataDir, "browser-cache")
	if b.CacheDir != want {
		t.Fatalf("CacheDir = %q, want %q (cache must default on)", b.CacheDir, want)
	}

	off := false
	cfg.Browser.DiskCache = &off
	b2, _, _ := FromConfig(cfg)
	if b2.CacheDir != "" {
		t.Fatalf("disk_cache:false must disable the cache, got %q", b2.CacheDir)
	}
}

// TestFromConfigWarmUpFlag passes the warm_up knob through.
func TestFromConfigWarmUpFlag(t *testing.T) {
	cfg := config.Default()
	cfg.Browser.WarmUp = true
	b, _, _ := FromConfig(cfg)
	if !b.WarmUp {
		t.Fatal("warm_up: true did not reach the engine config")
	}
}
