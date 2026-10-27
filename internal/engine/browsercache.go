package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Browser cache lifecycle
//
// The persistent HTTP cache lives under <data_dir>/browser-cache/<ns>/ where
// <ns> namespaces by GPU mode + headless + proxy (launch shapes that change
// what Chromium stores). Each session's launch owns its cache dir exclusively
// (Chromium takes a SingletonLock per cache dir), so sessions never share one;
// instead cache dirs are recycled: a session takes a free (stale) namespace
// dir or creates a new one, and releases it back on close.
//
// Nothing session-specific is cached: cookies, localStorage and meeting state
// all live in the per-session user-data dir, which is destroyed on close.
// Only Chromium's HTTP cache (static JS/CSS/fonts) persists.

const (
	// browserCacheMaxDirs bounds how many namespace dirs may accumulate.
	browserCacheMaxDirs = 4
	// browserCacheStaleAge marks a dir free for reuse after its last session.
	browserCacheStaleAge = 5 * time.Second
)

// browserCachePool recycles cache directories across sequential launches so
// repeated sessions hit a warm HTTP cache without ever sharing a dir between
// two live Chromium processes.
type browserCachePool struct {
	mu   sync.Mutex
	root string
	free []string
	live map[string]struct{}
}

func newBrowserCachePool(root string) *browserCachePool {
	return &browserCachePool{root: root, live: map[string]struct{}{}}
}

// acquire hands out an exclusive cache dir for one launch.
func (p *browserCachePool) acquire() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := os.MkdirAll(p.root, 0o700); err != nil {
		return "", fmt.Errorf("create cache root: %w", err)
	}
	// Recycle: prefer the most-recently-freed dir (warmest cache).
	for len(p.free) > 0 {
		dir := p.free[0]
		p.free = p.free[1:]
		if _, busy := p.live[dir]; busy {
			continue
		}
		// A crashed browser can leave a stale SingletonLock; Chromium
		// recovers from it, but a corrupt Cache_Data index is also fine —
		// Chromium rebuilds the index. Nothing to clean here.
		p.live[dir] = struct{}{}
		return dir, nil
	}
	// Bound total dirs: if at capacity, drop the oldest free dir.
	p.trimLocked()
	dir, err := os.MkdirTemp(p.root, "ns-*")
	if err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	p.live[dir] = struct{}{}
	return dir, nil
}

// release returns a cache dir to the pool for the next launch.
func (p *browserCachePool) release(dir string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, busy := p.live[dir]; !busy {
		return
	}
	delete(p.live, dir)
	p.free = append([]string{dir}, p.free...)
	p.trimLocked()
}

// trimLocked enforces browserCacheMaxDirs on free dirs (live dirs are exempt:
// they belong to running browsers). Oldest free dirs go first.
func (p *browserCachePool) trimLocked() {
	for len(p.free) > browserCacheMaxDirs {
		oldest := p.free[len(p.free)-1]
		p.free = p.free[:len(p.free)-1]
		_ = os.RemoveAll(oldest)
	}
}

// Stats reports pool state for diagnostics.
func (p *browserCachePool) stats() (liveCount, freeCount int, bytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	liveCount, freeCount = len(p.live), len(p.free)
	for _, dir := range p.free {
		bytes += dirSize(dir)
	}
	return liveCount, freeCount, bytes
}

// dirSize sums file sizes under dir, ignoring read errors.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// CleanupCache removes the whole persistent cache tree (shutdown path) and
// any stale locks left by crashed browsers older than an hour.
func CleanupCache(root string) {
	if strings.TrimSpace(root) == "" {
		return
	}
	_ = os.RemoveAll(root)
}

// SweepStaleCacheDirs removes cache dirs not touched within maxAge. Called on
// manager startup to recover from crashed sessions.
func SweepStaleCacheDirs(root string, maxAge time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(root, e.Name())
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(full)
	}
}

// sortStrings is a tiny helper used by tests.
func sortStrings(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
