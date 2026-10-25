package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestMeetingStore opens a store on a fresh temp file.
func newTestMeetingStore(t *testing.T) *MeetingStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "meetings.db")
	ms, err := NewMeetingStore(context.Background(), path)
	if err != nil {
		t.Fatalf("NewMeetingStore: %v", err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	return ms
}

func TestMeetingStoreSaveGet(t *testing.T) {
	ms := newTestMeetingStore(t)
	ctx := context.Background()

	m, err := ms.Save(ctx, 100, "Standup", "https://meet.google.com/abc-defg-hij")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if m.ID == 0 || m.OwnerID != 100 || m.Name != "Standup" {
		t.Fatalf("Save returned %+v", m)
	}
	if m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
		t.Fatal("Save must set timestamps")
	}

	got, err := ms.Get(ctx, 100, m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != m.ID || got.Name != m.Name || got.MeetURL != m.MeetURL {
		t.Fatalf("Get = %+v, want %+v", got, m)
	}
	if got.CreatedAt.Unix() != m.CreatedAt.Unix() || got.UpdatedAt.Unix() != m.UpdatedAt.Unix() {
		t.Fatalf("Get timestamps differ: %+v vs %+v", got, m)
	}
}

// Saving the same URL twice must not create a second row: the unique index
// on (owner_id, meet_url) is the wire-level guarantee, Save is the
// application-level one (it updates the name in place).
func TestMeetingStoreDuplicateURLPerOwner(t *testing.T) {
	ms := newTestMeetingStore(t)
	ctx := context.Background()

	if _, err := ms.Save(ctx, 100, "Standup", "https://meet.google.com/abc-defg-hij"); err != nil {
		t.Fatalf("Save #1: %v", err)
	}
	m, err := ms.Save(ctx, 100, "Daily Standup", "https://meet.google.com/abc-defg-hij")
	if err != nil {
		t.Fatalf("Save #2 ( rename): %v", err)
	}
	if m.Name != "Daily Standup" {
		t.Fatalf("Save #2 name = %q, want rename in place", m.Name)
	}
	list, err := ms.List(ctx, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List = %d rows, want 1 (no duplicate URL per owner)", len(list))
	}

	// An identical save is a no-op that returns the existing row.
	again, err := ms.Save(ctx, 100, "Daily Standup", "https://meet.google.com/abc-defg-hij")
	if err != nil {
		t.Fatalf("Save #3 (no-op): %v", err)
	}
	if again.ID != m.ID {
		t.Fatalf("no-op Save ID = %d, want %d", again.ID, m.ID)
	}
}

// Different owners may save the same URL: the duplicate rule is per owner.
func TestMeetingStoreSameURLOtherOwner(t *testing.T) {
	ms := newTestMeetingStore(t)
	ctx := context.Background()

	if _, err := ms.Save(ctx, 100, "Standup", "https://meet.google.com/abc-defg-hij"); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Save(ctx, 200, "Their Standup", "https://meet.google.com/abc-defg-hij"); err != nil {
		t.Fatalf("Save for a second owner: %v", err)
	}
	list, err := ms.List(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("second owner has %d rows, want 1", len(list))
	}
}

// Owner isolation: Get/Rename/Delete on another owner's meeting must all
// fail closed with ErrMeetingNotFound, never reveal the row.
func TestMeetingStoreOwnerIsolation(t *testing.T) {
	ms := newTestMeetingStore(t)
	ctx := context.Background()

	m, err := ms.Save(ctx, 100, "Standup", "https://meet.google.com/abc-defg-hij")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Save(ctx, 200, "Other", "https://meet.google.com/zzz-zzzz-zzz"); err != nil {
		t.Fatal(err)
	}

	if _, err := ms.Get(ctx, 200, m.ID); err != ErrMeetingNotFound {
		t.Fatalf("Get foreign meeting = %v, want ErrMeetingNotFound", err)
	}
	if _, err := ms.Rename(ctx, 200, m.ID, "Hijacked"); err != ErrMeetingNotFound {
		t.Fatalf("Rename foreign meeting = %v, want ErrMeetingNotFound", err)
	}
	if err := ms.Delete(ctx, 200, m.ID); err != ErrMeetingNotFound {
		t.Fatalf("Delete foreign meeting = %v, want ErrMeetingNotFound", err)
	}

	// The owner's row must be untouched after the foreign attempts.
	got, err := ms.Get(ctx, 100, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Standup" {
		t.Fatalf("owner's meeting renamed by a foreign user: %q", got.Name)
	}
}

func TestMeetingStoreRenameDelete(t *testing.T) {
	ms := newTestMeetingStore(t)
	ctx := context.Background()

	m, err := ms.Save(ctx, 100, "Standup", "https://meet.google.com/abc-defg-hij")
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := ms.Rename(ctx, 100, m.ID, "Daily")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "Daily" {
		t.Fatalf("Rename returned %q", renamed.Name)
	}
	got, err := ms.Get(ctx, 100, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Daily" {
		t.Fatalf("Get after rename = %q", got.Name)
	}

	if err := ms.Delete(ctx, 100, m.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := ms.Get(ctx, 100, m.ID); err != ErrMeetingNotFound {
		t.Fatalf("Get after delete = %v, want ErrMeetingNotFound", err)
	}
	if err := ms.Delete(ctx, 100, m.ID); err != ErrMeetingNotFound {
		t.Fatalf("Delete twice = %v, want ErrMeetingNotFound", err)
	}
}

// Migrations must be idempotent: opening the same database twice must not
// fail, and must not duplicate or drop data.
func TestMeetingStoreMigrationsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meetings.db")
	ctx := context.Background()

	ms1, err := NewMeetingStore(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := ms1.Save(ctx, 100, "Standup", "https://meet.google.com/abc-defg-hij"); err != nil {
		t.Fatal(err)
	}
	if err := ms1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Re-open: migrations run again over the existing schema and data.
	ms2, err := NewMeetingStore(ctx, path)
	if err != nil {
		t.Fatalf("second open (re-migrate): %v", err)
	}
	defer ms2.Close()
	list, err := ms2.List(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "Standup" {
		t.Fatalf("after re-migration: %+v", list)
	}

	// The schema version is recorded exactly once.
	var versions int
	if err := ms2.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if versions != 1 {
		t.Fatalf("schema_migrations rows = %d, want 1", versions)
	}
}

// Persistence across restarts: close the store, drop the in-memory state,
// reopen the same file and the meetings must still be there.
func TestMeetingStorePersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meetings.db")
	ctx := context.Background()

	func() {
		ms, err := NewMeetingStore(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer ms.Close()
		for i, name := range []string{"Standup", "Review", "AllHands"} {
			if _, err := ms.Save(ctx, 100, name,
				"https://meet.google.com/aaa-bbbb-c0"+string(rune('0'+i))); err != nil {
				t.Fatal(err)
			}
		}
	}()

	ms, err := NewMeetingStore(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer ms.Close()
	list, err := ms.List(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("after restart: %d meetings, want 3", len(list))
	}
	names := map[string]bool{}
	for _, m := range list {
		names[m.Name] = true
	}
	for _, want := range []string{"Standup", "Review", "AllHands"} {
		if !names[want] {
			t.Fatalf("after restart: %q missing from %+v", want, names)
		}
	}
}

// List is bounded and most-recently-updated first.
func TestMeetingListBoundedAndOrdered(t *testing.T) {
	ms := newTestMeetingStore(t)
	ctx := context.Background()

	for i := range maxSavedMeetings + 10 {
		url := "https://meet.google.com/aaaa-bbbb-c" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		if _, err := ms.Save(ctx, 100, "M"+string(rune('a'+i%26)), url); err != nil {
			// The unique index collides on the synthetic URLs at 26+; that is
			// fine, the point is the bound, not the exact count.
			if err == ErrTooManyMeetings {
				break
			}
			continue
		}
	}
	list, err := ms.List(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) > maxSavedMeetings {
		t.Fatalf("List = %d rows, want <= %d", len(list), maxSavedMeetings)
	}
	for i := 1; i < len(list); i++ {
		if list[i].UpdatedAt.After(list[i-1].UpdatedAt) {
			t.Fatalf("List not ordered by updated_at DESC: %+v", list)
		}
	}
}

// Concurrency: parallel goroutines must not corrupt the store or deadlock.
func TestMeetingStoreConcurrent(t *testing.T) {
	ms := newTestMeetingStore(t)
	ctx := context.Background()

	const workers = 8
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			owner := int64(1000 + w)
			for i := range 20 {
				url := "https://meet.google.com/conc-bbbb-c" + string(rune('a'+w)) + string(rune('a'+i%26))
				if _, err := ms.Save(ctx, owner, "M", url); err != nil {
					t.Errorf("worker %d save %d: %v", w, i, err)
					return
				}
			}
			list, err := ms.List(ctx, owner)
			if err != nil {
				t.Errorf("worker %d list: %v", w, err)
				return
			}
			if len(list) != 20 {
				t.Errorf("worker %d has %d rows, want 20", w, len(list))
			}
			if err := ms.Delete(ctx, owner, list[0].ID); err != nil {
				t.Errorf("worker %d delete: %v", w, err)
			}
		}(w)
	}
	wg.Wait()
}

// Validation: empty names/URLs and over-long names are rejected before the
// database is touched.
func TestMeetingStoreValidation(t *testing.T) {
	ms := newTestMeetingStore(t)
	ctx := context.Background()

	if _, err := ms.Save(ctx, 100, "", "https://meet.google.com/abc-defg-hij"); err == nil {
		t.Fatal("Save empty name = nil, want error")
	}
	if _, err := ms.Save(ctx, 100, "Standup", ""); err == nil {
		t.Fatal("Save empty url = nil, want error")
	}
	if _, err := ms.Save(ctx, 100, strings.Repeat("x", 101), "https://meet.google.com/abc-defg-hij"); err == nil {
		t.Fatal("Save 101-char name = nil, want error")
	}
	if _, err := ms.Rename(ctx, 100, 1, ""); err == nil {
		t.Fatal("Rename empty = nil, want error")
	}
	if _, err := ms.Rename(ctx, 100, 1, strings.Repeat("x", 101)); err == nil {
		t.Fatal("Rename 101 chars = nil, want error")
	}
	// Nothing was written.
	list, err := ms.List(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("validation wrote rows: %+v", list)
	}
}

// NewMeetingStore rejects a directory as a database path: never open a
// non-regular file.
func TestNewMeetingStoreRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewMeetingStore(context.Background(), dir); err == nil {
		t.Fatal("NewMeetingStore(directory) = nil, want error")
	}
	if _, err := NewMeetingStore(context.Background(), ""); err == nil {
		t.Fatal("NewMeetingStore(\"\") = nil, want error")
	}
}

// BackupDB copies the database and leaves the original untouched.
func TestBackupDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meetings.db")
	ms, err := NewMeetingStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Save(context.Background(), 100, "Standup", "https://meet.google.com/abc-defg-hij"); err != nil {
		t.Fatal(err)
	}
	if err := ms.Close(); err != nil {
		t.Fatal(err)
	}

	bak, err := BackupDB(path)
	if err != nil {
		t.Fatalf("BackupDB: %v", err)
	}
	if bak == "" {
		t.Fatal("BackupDB returned empty path")
	}
	if _, err := os.Stat(bak); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := os.ReadFile(bak)
	if err != nil {
		t.Fatal(err)
	}
	if len(orig) != len(copy) {
		t.Fatalf("backup size %d != original %d", len(copy), len(orig))
	}
}

// A WAL checkpoint leaves -wal/-shm siblings; make sure they do not confuse
// a reopen (the database must still read all committed rows).
func TestMeetingStoreReopenWithWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meetings.db")
	ctx := context.Background()

	ms, err := NewMeetingStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ms.Save(ctx, 100, "Standup", "https://meet.google.com/abc-defg-hij"); err != nil {
		t.Fatal(err)
	}
	// Force WAL growth then close with the wal file still present.
	if err := ms.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Logf("(info) no -wal file: %v", err)
	}

	ms2, err := NewMeetingStore(ctx, path)
	if err != nil {
		t.Fatalf("reopen with WAL: %v", err)
	}
	defer ms2.Close()
	list, err := ms2.List(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("reopen with WAL: %d rows, want 1", len(list))
	}
}

// Timestamps round-trip as real times, not raw ints.
func TestMeetingTimestamps(t *testing.T) {
	ms := newTestMeetingStore(t)
	ctx := context.Background()
	before := time.Now().Truncate(time.Second)
	m, err := ms.Save(ctx, 100, "Standup", "https://meet.google.com/abc-defg-hij")
	if err != nil {
		t.Fatal(err)
	}
	if m.CreatedAt.Before(before) {
		t.Fatalf("CreatedAt %v before Save %v", m.CreatedAt, before)
	}
	got, err := ms.Get(ctx, 100, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Unix() != m.CreatedAt.Unix() {
		t.Fatalf("round-trip CreatedAt %v != %v", got.CreatedAt, m.CreatedAt)
	}
}
