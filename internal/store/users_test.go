package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestUserStore(t *testing.T) *UserStore {
	t.Helper()
	us, err := NewUserStore(context.Background(), filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	t.Cleanup(func() { _ = us.Close() })
	return us
}

func TestUserSettingsDefaultsAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	// A user with no row gets the defaults, not zero values.
	got, err := us.Get(ctx, 42)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := DefaultUserSettings()
	if got.AudioLang != want.AudioLang || got.MediaMode != want.MediaMode ||
		got.Autoplay != want.Autoplay || got.RepeatMode != want.RepeatMode {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
	if _, has, _ := us.GetRaw(ctx, 42); has {
		t.Fatal("reading defaults created a row")
	}

	// Set every field and read it back.
	if _, err := us.SetAudioLang(ctx, 42, "en"); err != nil {
		t.Fatalf("set audio lang: %v", err)
	}
	if _, err := us.SetMediaMode(ctx, 42, "download"); err != nil {
		t.Fatalf("set media mode: %v", err)
	}
	if _, err := us.SetVolume(ctx, 42, 65); err != nil {
		t.Fatalf("set volume: %v", err)
	}
	if _, err := us.SetSpeed(ctx, 42, 1.5); err != nil {
		t.Fatalf("set speed: %v", err)
	}
	if _, err := us.SetAutoplay(ctx, 42, false); err != nil {
		t.Fatalf("set autoplay: %v", err)
	}
	if _, err := us.SetRepeatMode(ctx, 42, "all"); err != nil {
		t.Fatalf("set repeat: %v", err)
	}
	got, err = us.Get(ctx, 42)
	if err != nil {
		t.Fatalf("get after updates: %v", err)
	}
	if got.AudioLang != "en" || got.MediaMode != "download" || got.Volume != 65 ||
		got.Speed != 1.5 || got.Autoplay || got.RepeatMode != RepeatAll {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestUserSettingsValidation(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	if _, err := us.SetVolume(ctx, 42, 101); err == nil {
		t.Fatal("volume 101 accepted")
	}
	if _, err := us.SetVolume(ctx, 42, -1); err == nil {
		t.Fatal("volume -1 accepted")
	}
	if _, err := us.SetSpeed(ctx, 42, 5); err == nil {
		t.Fatal("speed 5 accepted")
	}
	if _, err := us.SetSpeed(ctx, 42, 0.1); err == nil {
		t.Fatal("speed 0.1 accepted")
	}
	// Invalid persisted values are normalized on read, so a corrupt row can
	// never leak into the runtime.
	if _, err := us.db.Exec(`INSERT INTO user_preferences (user_id, audio_lang, media_mode, repeat_mode, volume, created_at, updated_at)
		VALUES (7, 'klingon', 'teleport', 'sometimes', 9001, 0, 0)`); err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}
	got, err := us.Get(ctx, 7)
	if err != nil {
		t.Fatalf("get corrupt row: %v", err)
	}
	if got.AudioLang != DefaultAudioLang || got.MediaMode != DefaultMediaMode ||
		got.RepeatMode != RepeatOff || got.Volume != 0 {
		t.Fatalf("corrupt row not normalized: %+v", got)
	}
}

func TestUserSettingsIsolationBetweenUsers(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	if _, err := us.SetAudioLang(ctx, 1, "en"); err != nil {
		t.Fatalf("set user 1: %v", err)
	}
	if _, err := us.SetMediaMode(ctx, 2, "download"); err != nil {
		t.Fatalf("set user 2: %v", err)
	}
	a, _ := us.Get(ctx, 1)
	b, _ := us.Get(ctx, 2)
	if a.AudioLang != "en" || a.MediaMode != DefaultMediaMode {
		t.Fatalf("user 1 contaminated: %+v", a)
	}
	if b.AudioLang != DefaultAudioLang || b.MediaMode != "download" {
		t.Fatalf("user 2 contaminated: %+v", b)
	}
	if _, err := us.ResetUser(ctx, 1); err != nil {
		t.Fatalf("reset user 1: %v", err)
	}
	a2, _ := us.Get(ctx, 1)
	b2, _ := us.Get(ctx, 2)
	if a2.AudioLang != DefaultAudioLang {
		t.Fatal("user 1 not reset")
	}
	if b2.MediaMode != "download" {
		t.Fatal("resetting user 1 touched user 2")
	}
}

func TestUserSettingsPersistAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "users.db")

	us, err := NewUserStore(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := us.SetRepeatMode(ctx, 99, "one"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := us.SetVolume(ctx, 99, 40); err != nil {
		t.Fatalf("set volume: %v", err)
	}
	if err := us.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	us2, err := NewUserStore(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer us2.Close()
	got, err := us2.Get(ctx, 99)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.RepeatMode != RepeatOne || got.Volume != 40 {
		t.Fatalf("settings lost on reopen: %+v", got)
	}
	// The schema is at the latest recorded version, with no duplicate rows.
	var version int
	var rows int
	if err := us2.db.QueryRow(`SELECT MAX(version), COUNT(*) FROM schema_migrations`).Scan(&version, &rows); err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	if version != userSchemaVersion || rows != userSchemaVersion {
		t.Fatalf("schema_migrations version=%d rows=%d, want %d/%d", version, rows, userSchemaVersion, userSchemaVersion)
	}
}

func TestMigrateLegacyPrefsFromStateJSON(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	legacy := `{
	  "records": [{"id": "s1", "state": "ended", "created_at": "2024-01-01T00:00:00Z"}],
	  "user_prefs": {"111": {"audio_lang": "en"}, "222": {"audio_lang": "orig", "media_mode": "download"}}
	}`
	if err := os.WriteFile(statePath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	us := newTestUserStore(t)

	n, err := MigrateLegacyPrefs(ctx, us, statePath)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n != 2 {
		t.Fatalf("migrated = %d, want 2", n)
	}
	u1, _ := us.Get(ctx, 111)
	if u1.AudioLang != "en" {
		t.Fatalf("user 111 audio lang = %q", u1.AudioLang)
	}
	u2, _ := us.Get(ctx, 222)
	if u2.AudioLang != "orig" || u2.MediaMode != "download" {
		t.Fatalf("user 222 = %+v", u2)
	}

	// The JSON file keeps its records but loses the prefs, and a backup of
	// the original exists.
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read stripped state: %v", err)
	}
	var payload persistedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("stripped state invalid: %v", err)
	}
	if len(payload.Prefs) != 0 {
		t.Fatalf("prefs not stripped: %v", payload.Prefs)
	}
	if len(payload.Records) != 1 || payload.Records[0].ID != "s1" {
		t.Fatalf("records lost in migration: %+v", payload.Records)
	}
	entries, err := filepath.Glob(statePath + ".migrated-*")
	if err != nil || len(entries) != 1 {
		t.Fatalf("backup files = %v (err %v), want exactly one", entries, err)
	}

	// Re-running is safe and imports nothing new.
	n2, err := MigrateLegacyPrefs(ctx, us, statePath)
	if err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second migration imported %d, want 0", n2)
	}
}

func TestMigrateLegacyPrefsDoesNotDowngradeNewerSQLiteValues(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte(`{"records":[],"user_prefs":{"333":{"audio_lang":"fa"}}}`), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	us := newTestUserStore(t)
	if _, err := us.SetAudioLang(ctx, 333, "en"); err != nil {
		t.Fatalf("preset sqlite value: %v", err)
	}

	if _, err := MigrateLegacyPrefs(ctx, us, statePath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got, _ := us.Get(ctx, 333)
	if got.AudioLang != "en" {
		t.Fatalf("migration downgraded sqlite value: %+v", got)
	}
}

func TestMigrateLegacyPrefsToleratesMissingAndMalformedFiles(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	n, err := MigrateLegacyPrefs(ctx, us, filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || n != 0 {
		t.Fatalf("missing file: n=%d err=%v, want 0/nil", n, err)
	}

	malformed := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(malformed, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed: %v", err)
	}
	n, err = MigrateLegacyPrefs(ctx, us, malformed)
	if err != nil || n != 0 {
		t.Fatalf("malformed file: n=%d err=%v, want 0/nil", n, err)
	}
	if _, err := os.Stat(malformed); err != nil {
		t.Fatal("migration touched a malformed file")
	}
}

func TestSessionQueueSaveLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	snap := SessionQueueSnapshot{
		SessionID: "s-1",
		OwnerID:   55,
		State:     QueueStateActive,
		Cursor:    2,
		Items: []QueueItemSnapshot{
			{URL: "https://example.com/1.mp4", Title: "one", Kind: "direct"},
			{URL: "https://example.com/2.mp4", Title: "two"},
			{URL: "https://example.com/3.mp4"},
		},
	}
	if err := us.SaveSessionQueue(ctx, snap); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, found, err := us.LoadSessionQueue(ctx, 55, "s-1")
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if got.Cursor != 2 || len(got.Items) != 3 || got.State != QueueStateActive {
		t.Fatalf("snapshot = %+v", got)
	}
	for i, it := range got.Items {
		if it.Position != i {
			t.Fatalf("item %d position = %d", i, it.Position)
		}
	}

	// Ownership: another user (or a forged id) sees nothing.
	if _, found, _ := us.LoadSessionQueue(ctx, 56, "s-1"); found {
		t.Fatal("foreign owner read another user's queue")
	}

	// Overwrite replaces items atomically and keeps positions contiguous.
	snap.Items = snap.Items[:1]
	snap.Cursor = 0
	if err := us.SaveSessionQueue(ctx, snap); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _, _ = us.LoadSessionQueue(ctx, 55, "s-1")
	if len(got.Items) != 1 || got.Cursor != 0 {
		t.Fatalf("overwrite = %+v", got)
	}
}

func TestSessionQueueInterruptionLifecycle(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	if err := us.SaveSessionQueue(ctx, SessionQueueSnapshot{
		SessionID: "s-live", OwnerID: 1,
		Items: []QueueItemSnapshot{{URL: "https://example.com/a.mp4"}},
	}); err != nil {
		t.Fatalf("save live: %v", err)
	}
	if err := us.SaveSessionQueue(ctx, SessionQueueSnapshot{
		SessionID: "s-old", OwnerID: 1,
		Items: []QueueItemSnapshot{{URL: "https://example.com/b.mp4"}},
	}); err != nil {
		t.Fatalf("save old: %v", err)
	}
	// Simulate a graceful end: the row is deleted, nothing to interrupt.
	if err := us.DeleteSessionQueue(ctx, 1, "s-old"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	n, err := us.MarkQueuesInterrupted(ctx)
	if err != nil {
		t.Fatalf("mark interrupted: %v", err)
	}
	if n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	snap, found, err := us.LatestInterruptedQueue(ctx, 1)
	if err != nil || !found {
		t.Fatalf("latest interrupted: found=%v err=%v", found, err)
	}
	if snap.SessionID != "s-live" || snap.State != QueueStateInterrupted || len(snap.Items) != 1 {
		t.Fatalf("interrupted snapshot = %+v", snap)
	}
	// Marking again is a no-op.
	if n, _ := us.MarkQueuesInterrupted(ctx); n != 0 {
		t.Fatalf("second mark = %d, want 0", n)
	}
}

func TestPruneQueuesRemovesOnlyOldRows(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	if err := us.SaveSessionQueue(ctx, SessionQueueSnapshot{
		SessionID: "s-new", OwnerID: 1,
		Items: []QueueItemSnapshot{{URL: "https://example.com/a.mp4"}},
	}); err != nil {
		t.Fatalf("save new: %v", err)
	}
	// Backdate one row directly.
	if err := us.SaveSessionQueue(ctx, SessionQueueSnapshot{
		SessionID: "s-old2", OwnerID: 1,
		Items: []QueueItemSnapshot{{URL: "https://example.com/b.mp4"}},
	}); err != nil {
		t.Fatalf("save old: %v", err)
	}
	if _, err := us.db.Exec(`UPDATE session_queues SET updated_at = ? WHERE session_id = 's-old2'`, time.Now().Add(-48*time.Hour).Unix()); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := us.PruneQueues(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	if _, found, _ := us.LoadSessionQueue(ctx, 1, "s-new"); !found {
		t.Fatal("prune removed a fresh row")
	}
	if _, found, _ := us.LoadSessionQueue(ctx, 1, "s-old2"); found {
		t.Fatal("prune kept the stale row")
	}
}

func TestUserStoreConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				if _, err := us.SetAudioLang(ctx, id, "en"); err != nil {
					t.Errorf("set audio lang %d: %v", id, err)
					return
				}
				if _, err := us.SetVolume(ctx, id, int(n)%101); err != nil {
					t.Errorf("set volume %d: %v", id, err)
					return
				}
			}
		}(int64(i + 1))
	}
	wg.Wait()

	prefs, err := us.Preferences(ctx)
	if err != nil {
		t.Fatalf("preferences: %v", err)
	}
	if len(prefs) != 8 {
		t.Fatalf("preferences = %d users, want 8", len(prefs))
	}
	for id, u := range prefs {
		if u.AudioLang != "en" {
			t.Fatalf("user %d lost updates: %+v", id, u)
		}
	}
}

func TestQueueStats(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)
	if err := us.SaveSessionQueue(ctx, SessionQueueSnapshot{
		SessionID: "s-1", OwnerID: 1,
		Items: []QueueItemSnapshot{{URL: "https://example.com/a.mp4"}, {URL: "https://example.com/b.mp4"}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := us.MarkQueuesInterrupted(ctx); err != nil {
		t.Fatalf("mark: %v", err)
	}
	st, err := us.QueueStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Queues != 1 || st.Interrupted != 1 || st.Active != 0 || st.Items != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

// TestUserStoreRejectsBadPaths guards the open fence: a directory or a
// missing path must fail cleanly.
func TestUserStoreRejectsBadPaths(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewUserStore(context.Background(), ""); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := NewUserStore(context.Background(), dir); err == nil {
		t.Fatal("directory path accepted")
	}
	// A fresh database inside a fresh subdirectory works.
	us, err := NewUserStore(context.Background(), filepath.Join(dir, "sub", "users.db"))
	if err != nil {
		t.Fatalf("nested path rejected: %v", err)
	}
	_ = us.Close()
}

// TestUserStoreBusyTimeoutIsConfigured documents that concurrent access is
// configured for reliability (WAL + busy timeout), per the storage contract.
func TestUserStoreBusyTimeoutIsConfigured(t *testing.T) {
	us := newTestUserStore(t)
	var journalMode, busyTimeout string
	if err := us.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if err := us.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
	if busyTimeout == "0" {
		t.Fatal("busy_timeout is not set")
	}
	// Foreign keys must be on: queue items cascade with their header.
	var fk int
	if err := us.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d (err %v), want 1", fk, err)
	}
}

// TestUserStoreCascadeDelete pins the foreign key behaviour the bot relies
// on when cleaning up a finished session.
func TestUserStoreCascadeDelete(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)
	if err := us.SaveSessionQueue(ctx, SessionQueueSnapshot{
		SessionID: "s-cascade", OwnerID: 3,
		Items: []QueueItemSnapshot{{URL: "https://example.com/x.mp4"}, {URL: "https://example.com/y.mp4"}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := us.DeleteSessionQueue(ctx, 3, "s-cascade"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var items int
	if err := us.db.QueryRow(`SELECT COUNT(*) FROM session_queue_items WHERE session_id = 's-cascade'`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 0 {
		t.Fatalf("cascade left %d item rows", items)
	}
}

// compile-time guards: the SQL strings must stay parseable.
var _ = sql.ErrNoRows
var _ = fmt.Sprintf

// ---- workflow drafts and guest names (schema v2) ---------------------------

func TestWorkflowDraftRoundTrip(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	d := WorkflowDraft{
		UserID:     42,
		State:      WorkflowAwaitingMedia,
		MeetURL:    "https://meet.google.com/abc-defg-hij",
		MediaURL:   "https://example.com/a.mp4",
		MediaKind:  "direct",
		ExtraQueue: []string{"https://example.com/b.mp4", "https://example.com/c.mp4"},
	}
	if err := us.SaveWorkflowDraft(ctx, d); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, found, err := us.LoadWorkflowDraft(ctx, 42, 24*time.Hour)
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if got.State != WorkflowAwaitingMedia || got.MeetURL != d.MeetURL ||
		got.MediaURL != d.MediaURL || got.MediaKind != "direct" || len(got.ExtraQueue) != 2 {
		t.Fatalf("draft = %+v", got)
	}

	// Only one draft per user: saving replaces.
	d.State = WorkflowReviewing
	if err := us.SaveWorkflowDraft(ctx, d); err != nil {
		t.Fatalf("resave: %v", err)
	}
	got, found, _ = us.LoadWorkflowDraft(ctx, 42, 24*time.Hour)
	if !found || got.State != WorkflowReviewing {
		t.Fatalf("resave = %+v found=%v", got, found)
	}

	// Ownership: another user sees nothing.
	if _, found, _ := us.LoadWorkflowDraft(ctx, 43, 24*time.Hour); found {
		t.Fatal("foreign user read a draft")
	}

	if err := us.DeleteWorkflowDraft(ctx, 42); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := us.LoadWorkflowDraft(ctx, 42, 24*time.Hour); found {
		t.Fatal("draft survived deletion")
	}
}

func TestWorkflowDraftValidationAndExpiry(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	if err := us.SaveWorkflowDraft(ctx, WorkflowDraft{UserID: 42, State: "bogus"}); err == nil {
		t.Fatal("bogus state accepted")
	}
	if err := us.SaveWorkflowDraft(ctx, WorkflowDraft{UserID: -1, State: WorkflowReviewing}); err == nil {
		t.Fatal("bad user id accepted")
	}

	_ = us.SaveWorkflowDraft(ctx, WorkflowDraft{UserID: 42, State: WorkflowAwaitingMeet, MeetURL: "https://meet.google.com/abc-defg-hij"})
	// Backdate beyond the limit: the read drops the row.
	if err := us.BackdateWorkflowDraftForTest(42, 30*time.Minute); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, found, _ := us.LoadWorkflowDraft(ctx, 42, 15*time.Minute); found {
		t.Fatal("stale draft not expired")
	}
	if _, found, _ := us.LoadWorkflowDraft(ctx, 42, 15*time.Minute); found {
		t.Fatal("expired draft reappeared")
	}
	// maxAge 0 disables the expiry check (tests / recovery tooling).
	_ = us.SaveWorkflowDraft(ctx, WorkflowDraft{UserID: 42, State: WorkflowAwaitingMeet})
	if _, found, _ := us.LoadWorkflowDraft(ctx, 42, 0); !found {
		t.Fatal("maxAge=0 expired a fresh draft")
	}
}

func TestGuestNamePreference(t *testing.T) {
	ctx := context.Background()
	us := newTestUserStore(t)

	u, err := us.SetGuestName(ctx, 42, "  Ada   Lovelace ")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if u.GuestName != "Ada Lovelace" {
		t.Fatalf("guest name = %q", u.GuestName)
	}
	if u, _ := us.Get(ctx, 42); u.GuestName != "Ada Lovelace" {
		t.Fatalf("persisted guest name = %q", u.GuestName)
	}

	// Empty clears the override.
	if u, _ := us.SetGuestName(ctx, 42, "-"); true {
		_ = u
	}
	u, err = us.SetGuestName(ctx, 42, "")
	if err != nil || u.GuestName != "" {
		t.Fatalf("clear: %v %+v", err, u)
	}

	// Over-long names are rejected.
	if _, err := us.SetGuestName(ctx, 42, strings.Repeat("x", 100)); err == nil {
		t.Fatal("over-long guest name accepted")
	}
	// Control characters are rejected.
	if _, err := us.SetGuestName(ctx, 42, "badname"); err == nil {
		t.Fatal("control-character guest name accepted")
	}
	// A corrupt stored value is dropped on read.
	if _, err := us.db.Exec(`UPDATE user_preferences SET guest_name = ? WHERE user_id = 42`, strings.Repeat("y", 200)); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if u, _ := us.Get(ctx, 42); u.GuestName != "" {
		t.Fatalf("corrupt guest name not normalized: %q", u.GuestName)
	}
}

// TestUserStoreMigrationV2PreservesV1Data creates a database with only the
// version-1 schema and rows, then reopens it with the current code: the v2
// migration must run once and keep every existing value readable.
func TestUserStoreMigrationV2PreservesV1Data(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.db")

	// Build a v1 database by hand.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := db.Exec(userMigrations[1]); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO user_preferences (user_id, audio_lang, media_mode, volume, speed, autoplay, repeat_mode, created_at, updated_at)
		VALUES (7, 'en', 'download', 55, 1.25, 0, 'all', ?, ?)`, now, now); err != nil {
		t.Fatalf("seed v1 row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	// Reopen with the current store: v2 runs and preserves the row.
	us, err := NewUserStore(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer us.Close()
	u, err := us.Get(context.Background(), 7)
	if err != nil {
		t.Fatalf("get after migration: %v", err)
	}
	if u.AudioLang != "en" || u.MediaMode != "download" || u.Volume != 55 || u.Speed != 1.25 || u.Autoplay || u.RepeatMode != RepeatAll {
		t.Fatalf("v1 data lost in migration: %+v", u)
	}
	// The new column works on migrated rows.
	if _, err := us.SetGuestName(context.Background(), 7, "Grace Hopper"); err != nil {
		t.Fatalf("set guest name on migrated row: %v", err)
	}
	// And the drafts table exists.
	if err := us.SaveWorkflowDraft(context.Background(), WorkflowDraft{UserID: 7, State: WorkflowAwaitingMeet}); err != nil {
		t.Fatalf("draft on migrated db: %v", err)
	}
}
