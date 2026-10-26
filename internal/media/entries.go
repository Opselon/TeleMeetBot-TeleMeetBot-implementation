package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is the minimal metadata persisted for one managed file. It exists so
// expiry survives a restart and so the janitor never has to guess which files
// in the managed directory are the bot's responsibility.
type Entry struct {
	// ID is the stable handle (also the on-disk file name stem).
	ID string `json:"id"`
	// Path is the absolute path inside the managed directory.
	Path string `json:"path"`
	// URL is the source the file was fetched from (diagnostics only).
	URL string `json:"url"`
	// Kind is the classification of the source ("direct" | "youtube").
	Kind string `json:"kind"`
	// ContentType sniffed at fetch time; used to build the local player URL.
	ContentType string `json:"content_type,omitempty"`
	// Bytes is the on-disk size at completion.
	Bytes int64 `json:"bytes"`
	// FetchedAt is when the download completed; expiry counts from here.
	FetchedAt time.Time `json:"fetched_at"`
	// ExpiresAt is FetchedAt + TTL, materialised so the janitor and the
	// restart path agree even if the policy changed meanwhile. The deadline
	// governs orphan reclamation, not playback: while at least one lease
	// holds the entry, the janitor defers removal until every owning
	// session has released it (switch, stop or close).
	ExpiresAt time.Time `json:"expires_at"`
	// Transcoded records whether ffmpeg produced the final file.
	Transcoded bool `json:"transcoded,omitempty"`
}

// Expired reports whether the entry's deadline has passed.
func (e Entry) Expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt)
}

// Remaining is how long the file has left before expiry.
func (e Entry) Remaining(now time.Time) time.Duration {
	if e.ExpiresAt.IsZero() {
		return 0
	}
	if d := e.ExpiresAt.Sub(now); d > 0 {
		return d
	}
	return 0
}

// manifest is the persisted {version, entries} shape. Keeping it a distinct
// type (rather than dumping a slice) leaves room for schema evolution and
// makes a corrupt manifest easy to distinguish from an empty one.
type manifest struct {
	Version string  `json:"version"`
	Entries []Entry `json:"entries"`
}

const manifestVersion = "1"

// manifestName is the on-disk file the entryStore owns.
const manifestName = "downloads.json"

// entryStore is the concurrency-safe, file-backed bookkeeping for managed
// entries. It is the ONLY place that reads or writes downloads.json.
type entryStore struct {
	mu      sync.Mutex
	dir     string
	entries map[string]Entry
	loaded  bool
	// writes counts atomic file replacements (tests assert no-op churn).
	writes int
}

func newEntryStore(dir string) *entryStore {
	return &entryStore{dir: dir, entries: map[string]Entry{}}
}

func (s *entryStore) path() string { return filepath.Join(s.dir, manifestName) }

// load reads the manifest; a missing file is an empty store, and a
// structurally invalid one is quarantined (renamed aside) so the bot can
// still start and the directory still gets cleaned.
func (s *entryStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	s.loaded = true
	raw, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	var m manifest
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		_ = os.Rename(s.path(), fmt.Sprintf("%s.corrupt-%d", s.path(), time.Now().Unix()))
		return nil
	}
	for _, e := range m.Entries {
		if e.ID == "" || strings.TrimSpace(e.Path) == "" {
			continue
		}
		s.entries[e.ID] = e
	}
	return nil
}

// saveLocked persists every entry. Caller must hold s.mu.
func (s *entryStore) saveLocked() error {
	if strings.TrimSpace(s.dir) == "" {
		return errors.New("no download dir")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	entries := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].FetchedAt.Before(entries[j].FetchedAt) })
	out, err := json.MarshalIndent(manifest{Version: manifestVersion, Entries: entries}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		return err
	}
	s.writes++
	return nil
}

// Add records a completed download and persists.
func (s *entryStore) Add(e Entry) error {
	if e.ID == "" {
		return errors.New("entry has no id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[e.ID] = e
	return s.saveLocked()
}

// Get returns a copy of one entry.
func (s *entryStore) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	return e, ok
}

// All returns a snapshot copy of every entry.
func (s *entryStore) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	return out
}

// Remove deletes an entry from the manifest (the FILE is the janitor's job).
func (s *entryStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; !ok {
		return nil
	}
	delete(s.entries, id)
	return s.saveLocked()
}

// RemoveIf drops entries matching pred and returns the removed ones.
func (s *entryStore) RemoveIf(pred func(Entry) bool) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []Entry
	for id, e := range s.entries {
		if pred(e) {
			removed = append(removed, e)
			delete(s.entries, id)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	return removed, s.saveLocked()
}

// Writes returns the number of persisted writes (test instrumentation).
func (s *entryStore) Writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}
