package store

// User preferences and session-queue metadata, persisted in SQLite (users.db).
//
// users.db is the authoritative store for durable per-user configuration and
// for the queue metadata that must survive a restart. It follows the same
// architecture as meetings.db and playlists.db — one small database per
// concern, so a damaged file can be reset without touching the others:
//
//   - every write goes through one mutex and one database connection, so
//     concurrent Telegram callbacks serialize on the database itself;
//   - ownership is part of every lookup, so a forged id can never read or
//     mutate another user's settings or queue;
//   - migrations are idempotent and versioned in schema_migrations;
//   - session queue positions are contiguous (0..n-1) after every mutation.
//
// The legacy JSON state.json remains the store for the bounded session
// history; the one-shot migration below lifts its user_prefs into SQLite.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// Repeat modes for playback.
const (
	RepeatOff = "off"
	RepeatOne = "one"
	RepeatAll = "all"
)

// NormalizeRepeatMode validates a repeat-mode string, falling back to off.
func NormalizeRepeatMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "one", "single", "track":
		return RepeatOne
	case "all", "queue", "loop":
		return RepeatAll
	default:
		return RepeatOff
	}
}

// UserSettings is the durable per-user configuration. Zero values mean
// "use the application default" for optional numeric fields.
type UserSettings struct {
	AudioLang  string  `json:"audio_lang"`
	MediaMode  string  `json:"media_mode"`
	Volume     int     `json:"volume"` // 0 = unset (use global default)
	Speed      float64 `json:"speed"`  // 0 = unset (use global default)
	Autoplay   bool    `json:"autoplay"`
	RepeatMode string  `json:"repeat_mode"`
	// GuestName overrides the Meet display name when non-empty. Empty means
	// "use the verified Google identity or the configured default".
	GuestName string `json:"guest_name,omitempty"`
}

// MaxGuestNameRunes bounds the user-supplied Meet display name.
const MaxGuestNameRunes = 60

// SanitizeGuestName trims and validates a guest display name. Empty input is
// valid (it clears the override). Control characters are rejected: the name
// is typed into Google Meet's input, not into a Telegram message.
func SanitizeGuestName(raw string) (string, error) {
	name := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if name == "" {
		return "", nil
	}
	if utf8.RuneCountInString(name) > MaxGuestNameRunes {
		return "", fmt.Errorf("%w: guest name is too long (max %d characters)", ErrUserInvalid, MaxGuestNameRunes)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: guest name contains control characters", ErrUserInvalid)
		}
	}
	return name, nil
}

// DefaultUserSettings returns the built-in defaults applied when a user has
// no saved row. The audio language is Persian, as mandated.
func DefaultUserSettings() UserSettings {
	return UserSettings{
		AudioLang:  DefaultAudioLang,
		MediaMode:  DefaultMediaMode,
		Volume:     0,
		Speed:      0,
		Autoplay:   true,
		RepeatMode: RepeatOff,
	}
}

// normalize applies the store-level fences to a row read from disk so a
// malformed legacy value can never leak into the runtime.
func (u UserSettings) normalize() UserSettings {
	u.AudioLang = NormalizeAudioLang(u.AudioLang)
	u.MediaMode = NormalizeMediaMode(u.MediaMode)
	u.RepeatMode = NormalizeRepeatMode(u.RepeatMode)
	// A guest name that no longer validates is dropped rather than applied.
	if sanitized, err := SanitizeGuestName(u.GuestName); err != nil {
		u.GuestName = ""
	} else {
		u.GuestName = sanitized
	}
	if u.Volume < 0 || u.Volume > 100 {
		u.Volume = 0
	}
	if u.Speed < 0 || u.Speed != u.Speed { // negative or NaN
		u.Speed = 0
	}
	if u.Speed > 4 {
		u.Speed = 0
	}
	return u
}

// QueueItemSnapshot is one persisted entry of a session queue.
type QueueItemSnapshot struct {
	Position int
	URL      string
	Title    string
	Kind     string
	Duration float64
}

// SessionQueueSnapshot is the durable metadata of one session's queue.
type SessionQueueSnapshot struct {
	SessionID string
	OwnerID   int64
	// State is "active" while the session runs and "interrupted" when a
	// restart found no live session for the row.
	State  string
	Cursor int
	Items  []QueueItemSnapshot
	// UpdatedAt is the last write time.
	UpdatedAt time.Time
}

// Session queue states.
const (
	QueueStateActive      = "active"
	QueueStateInterrupted = "interrupted"
)

// userSchemaVersion is the latest schema version recorded in
// schema_migrations. Bump it together with a new step in userMigrations.
const userSchemaVersion = 2

// userMigrations maps a version to its DDL. Version 1 is idempotent
// (CREATE ... IF NOT EXISTS). Version 2 adds the guest-name preference and
// the workflow-draft table; its ALTER TABLE is column-guarded so a crash
// between the ALTER and the version insert cannot make the migration
// un-runnable. Steps are applied strictly in order, once per version.
var userMigrations = map[int]string{
	1: `CREATE TABLE IF NOT EXISTS user_preferences (
		user_id     INTEGER PRIMARY KEY,
		audio_lang  TEXT    NOT NULL DEFAULT 'fa',
		media_mode  TEXT    NOT NULL DEFAULT 'browser',
		volume      INTEGER NOT NULL DEFAULT 0,
		speed       REAL    NOT NULL DEFAULT 0,
		autoplay    INTEGER NOT NULL DEFAULT 1,
		repeat_mode TEXT    NOT NULL DEFAULT 'off',
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS session_queues (
		session_id TEXT    PRIMARY KEY,
		owner_id   INTEGER NOT NULL,
		state      TEXT    NOT NULL DEFAULT 'active',
		cursor     INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_session_queues_owner
		ON session_queues(owner_id, updated_at DESC);
	CREATE TABLE IF NOT EXISTS session_queue_items (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT    NOT NULL REFERENCES session_queues(session_id) ON DELETE CASCADE,
		position   INTEGER NOT NULL,
		url        TEXT    NOT NULL,
		title      TEXT    NOT NULL DEFAULT '',
		kind       TEXT    NOT NULL DEFAULT '',
		duration   REAL    NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_session_queue_items_position
		ON session_queue_items(session_id, position);
	CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);`,
	2: `ALTER TABLE user_preferences ADD COLUMN guest_name TEXT NOT NULL DEFAULT '';
	CREATE TABLE IF NOT EXISTS user_workflows (
		user_id    INTEGER PRIMARY KEY,
		state      TEXT    NOT NULL DEFAULT 'idle',
		meet_url   TEXT    NOT NULL DEFAULT '',
		media_url  TEXT    NOT NULL DEFAULT '',
		media_kind TEXT    NOT NULL DEFAULT '',
		extra_queue TEXT   NOT NULL DEFAULT '[]',
		updated_at INTEGER NOT NULL
	);`,
}

// ErrUserInvalid wraps invalid user-scoped input.
var ErrUserInvalid = errors.New("invalid user settings value")

// UserStore persists per-user preferences and session queue metadata in
// SQLite. It is concurrency-safe: every method holds the store mutex and the
// connection pool is capped at one connection so writers serialize.
type UserStore struct {
	mu sync.Mutex
	db *sql.DB
}

// NewUserStore opens (or creates) the SQLite database at path and runs the
// idempotent migrations. The parent directory is created when missing.
func NewUserStore(ctx context.Context, path string) (*UserStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("user store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create user db dir: %w", err)
	}
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("user db path %s is not a regular file", path)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open user sqlite: %w", err)
	}
	// One connection serializes writers, matching the other stores. The
	// setters are read-modify-write cycles, so they serialize in-process
	// through the store mutex instead of relying on transaction upgrade
	// semantics (see upsertSettings).
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping user sqlite: %w", err)
	}
	us := &UserStore{db: db}
	if err := us.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return us, nil
}

// Close releases the database handle. It is safe on a nil or closed store.
func (us *UserStore) Close() error {
	if us == nil {
		return nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	if us.db == nil {
		return nil
	}
	err := us.db.Close()
	us.db = nil
	return err
}

// migrate applies migrations strictly in version order and records each
// applied version. Safe to re-run: version rows gate every step, and the
// version-2 ALTER TABLE is additionally column-guarded against a crash
// between the DDL and its version bookkeeping.
func (us *UserStore) migrate(ctx context.Context) error {
	us.mu.Lock()
	defer us.mu.Unlock()

	// Step 1 is the baseline and stays unconditionally idempotent, so a
	// database created before versions were recorded upgrades cleanly.
	if _, err := us.db.ExecContext(ctx, userMigrations[1]); err != nil {
		return fmt.Errorf("apply user migration 1: %w", err)
	}
	var version int
	err := us.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return fmt.Errorf("read user schema version: %w", err)
	}
	for v := version + 1; v <= userSchemaVersion; v++ {
		switch v {
		case 2:
			// Column-guarded ALTER: a partially applied v2 must not fail
			// the rerun.
			var haveGuest int
			if err := us.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM pragma_table_info('user_preferences') WHERE name = 'guest_name'`).
				Scan(&haveGuest); err != nil {
				return fmt.Errorf("inspect user_preferences columns: %w", err)
			}
			if haveGuest == 0 {
				if _, err := us.db.ExecContext(ctx,
					`ALTER TABLE user_preferences ADD COLUMN guest_name TEXT NOT NULL DEFAULT ''`); err != nil {
					return fmt.Errorf("apply user migration 2 (alter): %w", err)
				}
			}
			if _, err := us.db.ExecContext(ctx,
				`CREATE TABLE IF NOT EXISTS user_workflows (
					user_id     INTEGER PRIMARY KEY,
					state       TEXT    NOT NULL DEFAULT 'idle',
					meet_url    TEXT    NOT NULL DEFAULT '',
					media_url   TEXT    NOT NULL DEFAULT '',
					media_kind  TEXT    NOT NULL DEFAULT '',
					extra_queue TEXT    NOT NULL DEFAULT '[]',
					updated_at  INTEGER NOT NULL
				)`); err != nil {
				return fmt.Errorf("apply user migration 2 (table): %w", err)
			}
		default:
			if _, err := us.db.ExecContext(ctx, userMigrations[v]); err != nil {
				return fmt.Errorf("apply user migration %d: %w", v, err)
			}
		}
		if _, err := us.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)
			 ON CONFLICT(version) DO NOTHING`, v, time.Now().Unix()); err != nil {
			return fmt.Errorf("record user schema version %d: %w", v, err)
		}
	}
	return nil
}

// ---- user preferences ------------------------------------------------------

// userPrefDataColumns lists the value columns (no user_id): the single-row
// readers filter by user_id and scan only these.
const userPrefDataColumns = `audio_lang, media_mode, volume, speed, autoplay, repeat_mode, guest_name`

// userPrefColumns adds user_id for the map-shaped listing query.
const userPrefColumns = `user_id, ` + userPrefDataColumns

func scanUserSettings(row *sql.Row) (UserSettings, error) {
	var u UserSettings
	var autoplay int
	err := row.Scan(&u.AudioLang, &u.MediaMode, &u.Volume, &u.Speed, &autoplay, &u.RepeatMode, &u.GuestName)
	if err != nil {
		return UserSettings{}, err
	}
	u.Autoplay = autoplay != 0
	return u.normalize(), nil
}

// Get returns one user's saved settings with the storage-level defaults
// applied. A user with no row gets DefaultUserSettings.
func (us *UserStore) Get(ctx context.Context, userID int64) (UserSettings, error) {
	if userID <= 0 {
		return DefaultUserSettings(), nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()

	row := us.db.QueryRowContext(ctx,
		`SELECT `+userPrefDataColumns+` FROM user_preferences WHERE user_id = ?`, userID)
	u, err := scanUserSettings(row)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultUserSettings(), nil
	}
	if err != nil {
		return UserSettings{}, fmt.Errorf("get user preferences: %w", err)
	}
	return u, nil
}

// GetRaw returns one user's row and whether it exists, without applying
// defaults. The management console uses it to show who has saved anything.
func (us *UserStore) GetRaw(ctx context.Context, userID int64) (UserSettings, bool, error) {
	if userID <= 0 {
		return UserSettings{}, false, nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()

	row := us.db.QueryRowContext(ctx,
		`SELECT `+userPrefDataColumns+` FROM user_preferences WHERE user_id = ?`, userID)
	u, err := scanUserSettings(row)
	if errors.Is(err, sql.ErrNoRows) {
		return UserSettings{}, false, nil
	}
	if err != nil {
		return UserSettings{}, false, fmt.Errorf("get raw user preferences: %w", err)
	}
	return u, true, nil
}

// ensureRowLocked inserts an empty row for the user when missing, inside the
// caller's transaction when tx is non-nil (used by the migration so a legacy
// import stays atomic).
func (us *UserStore) upsertSettings(ctx context.Context, userID int64, apply func(*UserSettings)) (UserSettings, error) {
	if userID <= 0 {
		return UserSettings{}, fmt.Errorf("%w: user id", ErrUserInvalid)
	}
	us.mu.Lock()
	defer us.mu.Unlock()

	tx, err := us.db.BeginTx(ctx, nil)
	if err != nil {
		return UserSettings{}, fmt.Errorf("begin update preferences: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read-or-create inside the transaction so two concurrent setters cannot
	// both create the row and lose one of the updates.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO user_preferences (user_id, created_at, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(user_id) DO NOTHING`,
		userID, time.Now().Unix(), time.Now().Unix())
	if err != nil {
		return UserSettings{}, fmt.Errorf("ensure preferences row: %w", err)
	}
	row := tx.QueryRowContext(ctx,
		`SELECT `+userPrefDataColumns+` FROM user_preferences WHERE user_id = ?`, userID)
	u, err := scanUserSettings(row)
	if err != nil {
		return UserSettings{}, fmt.Errorf("read preferences for update: %w", err)
	}
	apply(&u)
	u = u.normalize()
	autoplay := 0
	if u.Autoplay {
		autoplay = 1
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE user_preferences SET audio_lang = ?, media_mode = ?, volume = ?, speed = ?, autoplay = ?, repeat_mode = ?, guest_name = ?, updated_at = ?
		 WHERE user_id = ?`,
		u.AudioLang, u.MediaMode, u.Volume, u.Speed, autoplay, u.RepeatMode, u.GuestName, time.Now().Unix(), userID); err != nil {
		return UserSettings{}, fmt.Errorf("update preferences: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return UserSettings{}, fmt.Errorf("commit preferences: %w", err)
	}
	return u, nil
}

// SetAudioLang persists the audio language preference.
func (us *UserStore) SetAudioLang(ctx context.Context, userID int64, lang string) (UserSettings, error) {
	return us.upsertSettings(ctx, userID, func(u *UserSettings) { u.AudioLang = NormalizeAudioLang(lang) })
}

// SetMediaMode persists the Download & Stream mode preference.
func (us *UserStore) SetMediaMode(ctx context.Context, userID int64, mode string) (UserSettings, error) {
	return us.upsertSettings(ctx, userID, func(u *UserSettings) { u.MediaMode = NormalizeMediaMode(mode) })
}

// SetVolume persists the preferred default volume (0-100; 0 keeps the global
// default when unset is desired, any out-of-range value is rejected).
func (us *UserStore) SetVolume(ctx context.Context, userID int64, volume int) (UserSettings, error) {
	if volume < 0 || volume > 100 {
		return UserSettings{}, fmt.Errorf("%w: volume must be 0-100", ErrUserInvalid)
	}
	return us.upsertSettings(ctx, userID, func(u *UserSettings) { u.Volume = volume })
}

// SetSpeed persists the preferred playback speed (0.25-4.0; 0 unsets).
func (us *UserStore) SetSpeed(ctx context.Context, userID int64, speed float64) (UserSettings, error) {
	if speed != 0 && (speed < 0.25 || speed > 4) {
		return UserSettings{}, fmt.Errorf("%w: speed must be 0.25-4.0", ErrUserInvalid)
	}
	return us.upsertSettings(ctx, userID, func(u *UserSettings) { u.Speed = speed })
}

// SetAutoplay persists whether queued media should auto-advance.
func (us *UserStore) SetAutoplay(ctx context.Context, userID int64, on bool) (UserSettings, error) {
	return us.upsertSettings(ctx, userID, func(u *UserSettings) { u.Autoplay = on })
}

// SetRepeatMode persists the repeat preference.
func (us *UserStore) SetRepeatMode(ctx context.Context, userID int64, mode string) (UserSettings, error) {
	return us.upsertSettings(ctx, userID, func(u *UserSettings) { u.RepeatMode = NormalizeRepeatMode(mode) })
}

// SetGuestName persists the Meet display-name override. Empty input clears
// the override so the verified identity or server default applies again.
func (us *UserStore) SetGuestName(ctx context.Context, userID int64, name string) (UserSettings, error) {
	sanitized, err := SanitizeGuestName(name)
	if err != nil {
		return UserSettings{}, err
	}
	return us.upsertSettings(ctx, userID, func(u *UserSettings) { u.GuestName = sanitized })
}

// ResetUser deletes one user's preferences row. It returns true when a row
// existed.
func (us *UserStore) ResetUser(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	res, err := us.db.ExecContext(ctx, `DELETE FROM user_preferences WHERE user_id = ?`, userID)
	if err != nil {
		return false, fmt.Errorf("reset user preferences: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reset user preferences rows: %w", err)
	}
	return n > 0, nil
}

// ResetAllPreferences deletes every preferences row and returns the count.
// Session queue metadata is left alone.
func (us *UserStore) ResetAllPreferences(ctx context.Context) (int, error) {
	us.mu.Lock()
	defer us.mu.Unlock()
	res, err := us.db.ExecContext(ctx, `DELETE FROM user_preferences`)
	if err != nil {
		return 0, fmt.Errorf("reset all preferences: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reset all preferences rows: %w", err)
	}
	return int(n), nil
}

// Preferences returns every saved settings row keyed by user id.
func (us *UserStore) Preferences(ctx context.Context) (map[int64]UserSettings, error) {
	us.mu.Lock()
	defer us.mu.Unlock()
	rows, err := us.db.QueryContext(ctx, `SELECT `+userPrefColumns+` FROM user_preferences`)
	if err != nil {
		return nil, fmt.Errorf("list preferences: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]UserSettings)
	for rows.Next() {
		var id int64
		var u UserSettings
		var autoplay int
		if err := rows.Scan(&id, &u.AudioLang, &u.MediaMode, &u.Volume, &u.Speed, &autoplay, &u.RepeatMode, &u.GuestName); err != nil {
			return nil, fmt.Errorf("scan preference: %w", err)
		}
		u.Autoplay = autoplay != 0
		out[id] = u.normalize()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate preferences: %w", err)
	}
	return out, nil
}

// KnownUserIDs returns the union of preference owners and queue owners,
// sorted ascending, merged with any extra ids supplied by the caller.
func (us *UserStore) KnownUserIDs(ctx context.Context, extra ...int64) ([]int64, error) {
	us.mu.Lock()
	defer us.mu.Unlock()
	seen := make(map[int64]struct{})
	rows, err := us.db.QueryContext(ctx,
		`SELECT user_id FROM user_preferences
		 UNION SELECT owner_id FROM session_queues`)
	if err != nil {
		return nil, fmt.Errorf("list known users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan known user: %w", err)
		}
		seen[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate known users: %w", err)
	}
	for _, id := range extra {
		if id > 0 {
			seen[id] = struct{}{}
		}
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	for i := 1; i < len(out); i++ { // small n; insertion sort keeps deps minimal
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// ---- session queue metadata ------------------------------------------------

// SaveSessionQueue durably records one session's queue and playback cursor.
// The write is a single transaction: the old rows are replaced atomically, so
// a concurrent reader never sees a half-updated queue. Positions are stored
// contiguously starting at 0.
func (us *UserStore) SaveSessionQueue(ctx context.Context, snap SessionQueueSnapshot) error {
	if strings.TrimSpace(snap.SessionID) == "" {
		return fmt.Errorf("%w: session id", ErrUserInvalid)
	}
	if snap.OwnerID <= 0 {
		return fmt.Errorf("%w: owner id", ErrUserInvalid)
	}
	state := snap.State
	if state == "" {
		state = QueueStateActive
	}
	if state != QueueStateActive && state != QueueStateInterrupted {
		return fmt.Errorf("%w: queue state %q", ErrUserInvalid, state)
	}
	cursor := snap.Cursor
	if cursor < -1 {
		cursor = -1
	}

	us.mu.Lock()
	defer us.mu.Unlock()

	tx, err := us.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin save queue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Upsert the queue header first so the items' foreign key resolves even
	// on the very first save.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_queues (session_id, owner_id, state, cursor, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET owner_id = excluded.owner_id,
			state = excluded.state, cursor = excluded.cursor, updated_at = excluded.updated_at`,
		snap.SessionID, snap.OwnerID, state, cursor, time.Now().Unix()); err != nil {
		return fmt.Errorf("upsert queue header: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session_queue_items WHERE session_id = ?`, snap.SessionID); err != nil {
		return fmt.Errorf("clear queue items: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO session_queue_items (session_id, position, url, title, kind, duration)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare queue item insert: %w", err)
	}
	defer stmt.Close()
	for i, it := range snap.Items {
		if strings.TrimSpace(it.URL) == "" {
			continue
		}
		duration := it.Duration
		if duration < 0 || duration != duration {
			duration = 0
		}
		kind := it.Kind
		if len(kind) > 24 {
			kind = truncateRunes(kind, 24)
		}
		if _, err := stmt.ExecContext(ctx, snap.SessionID, i, it.URL,
			truncateRunes(it.Title, MaxPlaylistTitleRunes), kind, duration); err != nil {
			return fmt.Errorf("insert queue item %d: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit save queue: %w", err)
	}
	return nil
}

// LoadSessionQueue returns one session's persisted queue metadata. A missing
// row and a foreign row are both "found = false", so a forged session id
// cannot read another user's queue.
func (us *UserStore) LoadSessionQueue(ctx context.Context, ownerID int64, sessionID string) (SessionQueueSnapshot, bool, error) {
	if ownerID <= 0 || strings.TrimSpace(sessionID) == "" {
		return SessionQueueSnapshot{}, false, nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()

	var state string
	var cursor int
	var updated int64
	err := us.db.QueryRowContext(ctx,
		`SELECT state, cursor, updated_at FROM session_queues WHERE session_id = ? AND owner_id = ?`,
		sessionID, ownerID).Scan(&state, &cursor, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionQueueSnapshot{}, false, nil
	}
	if err != nil {
		return SessionQueueSnapshot{}, false, fmt.Errorf("load session queue: %w", err)
	}
	snap := SessionQueueSnapshot{
		SessionID: sessionID,
		OwnerID:   ownerID,
		State:     state,
		Cursor:    cursor,
		UpdatedAt: time.Unix(updated, 0),
	}
	rows, err := us.db.QueryContext(ctx,
		`SELECT position, url, title, kind, duration FROM session_queue_items
		 WHERE session_id = ? ORDER BY position, id LIMIT ?`, sessionID, MaxPlaylistItems)
	if err != nil {
		return SessionQueueSnapshot{}, false, fmt.Errorf("load session queue items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var it QueueItemSnapshot
		if err := rows.Scan(&it.Position, &it.URL, &it.Title, &it.Kind, &it.Duration); err != nil {
			return SessionQueueSnapshot{}, false, fmt.Errorf("scan session queue item: %w", err)
		}
		snap.Items = append(snap.Items, it)
	}
	if err := rows.Err(); err != nil {
		return SessionQueueSnapshot{}, false, fmt.Errorf("iterate session queue items: %w", err)
	}
	return snap, true, nil
}

// LatestInterruptedQueue returns the owner's most recent interrupted queue,
// used by /queue to show what a restart cut short. Only "interrupted" rows
// are returned: an "active" row means the session is still live and the
// engine is the source of truth.
func (us *UserStore) LatestInterruptedQueue(ctx context.Context, ownerID int64) (SessionQueueSnapshot, bool, error) {
	if ownerID <= 0 {
		return SessionQueueSnapshot{}, false, nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()

	var sessionID string
	var cursor int
	var updated int64
	err := us.db.QueryRowContext(ctx,
		`SELECT session_id, cursor, updated_at FROM session_queues
		 WHERE owner_id = ? AND state = ?
		 ORDER BY updated_at DESC LIMIT 1`, ownerID, QueueStateInterrupted).
		Scan(&sessionID, &cursor, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionQueueSnapshot{}, false, nil
	}
	if err != nil {
		return SessionQueueSnapshot{}, false, fmt.Errorf("latest interrupted queue: %w", err)
	}
	snap := SessionQueueSnapshot{
		SessionID: sessionID,
		OwnerID:   ownerID,
		State:     QueueStateInterrupted,
		Cursor:    cursor,
		UpdatedAt: time.Unix(updated, 0),
	}
	rows, err := us.db.QueryContext(ctx,
		`SELECT position, url, title, kind, duration FROM session_queue_items
		 WHERE session_id = ? ORDER BY position, id LIMIT ?`, sessionID, MaxPlaylistItems)
	if err != nil {
		return SessionQueueSnapshot{}, false, fmt.Errorf("latest interrupted queue items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var it QueueItemSnapshot
		if err := rows.Scan(&it.Position, &it.URL, &it.Title, &it.Kind, &it.Duration); err != nil {
			return SessionQueueSnapshot{}, false, fmt.Errorf("scan interrupted queue item: %w", err)
		}
		snap.Items = append(snap.Items, it)
	}
	if err := rows.Err(); err != nil {
		return SessionQueueSnapshot{}, false, fmt.Errorf("iterate interrupted queue items: %w", err)
	}
	return snap, true, nil
}

// MarkQueuesInterrupted flips every queue row still marked active to
// interrupted. It is called once at startup: no live session can exist yet,
// so any surviving "active" row belongs to a session the previous process
// lost. It returns the number of rows marked.
func (us *UserStore) MarkQueuesInterrupted(ctx context.Context) (int, error) {
	us.mu.Lock()
	defer us.mu.Unlock()
	res, err := us.db.ExecContext(ctx,
		`UPDATE session_queues SET state = ?, updated_at = ? WHERE state = ?`,
		QueueStateInterrupted, time.Now().Unix(), QueueStateActive)
	if err != nil {
		return 0, fmt.Errorf("mark interrupted queues: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mark interrupted queues rows: %w", err)
	}
	return int(n), nil
}

// DeleteSessionQueue removes one session's persisted queue metadata. It is a
// no-op for unknown ids (history cleanup may run after the row is gone).
func (us *UserStore) DeleteSessionQueue(ctx context.Context, ownerID int64, sessionID string) error {
	if ownerID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	// The ON DELETE CASCADE removes the items; the owner filter keeps one
	// user's cleanup from deleting another user's row that predates a
	// session-id collision.
	_, err := us.db.ExecContext(ctx,
		`DELETE FROM session_queues WHERE session_id = ? AND owner_id = ?`, sessionID, ownerID)
	if err != nil {
		return fmt.Errorf("delete session queue: %w", err)
	}
	return nil
}

// PruneQueues removes queue metadata rows older than maxAge. The bounded
// history in state.json is the long-term record; queues only need to outlive
// a restart. It returns the number of rows removed.
func (us *UserStore) PruneQueues(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	res, err := us.db.ExecContext(ctx,
		`DELETE FROM session_queues WHERE updated_at < ?`, time.Now().Add(-maxAge).Unix())
	if err != nil {
		return 0, fmt.Errorf("prune session queues: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune session queues rows: %w", err)
	}
	return int(n), nil
}

// ---- workflow drafts --------------------------------------------------------

// Workflow states for the session-creation draft.
const (
	WorkflowAwaitingMeet  = "awaiting_meet_url"
	WorkflowAwaitingMedia = "awaiting_media_url"
	WorkflowReviewing     = "reviewing_configuration"
)

// WorkflowDraft is the resumable draft of a session under construction.
// It is intentionally tiny: two URLs, the validated media kind and the
// state the wizard stopped at. Abandoned drafts expire by updated_at.
type WorkflowDraft struct {
	UserID     int64
	State      string
	MeetURL    string
	MediaURL   string
	MediaKind  string
	ExtraQueue []string
	UpdatedAt  time.Time
}

// ValidateWorkflowDraft fences a draft before it is stored: the state must
// be one of the known creation states and URLs stay within the playlist
// URL length bound.
func ValidateWorkflowDraft(d WorkflowDraft) (WorkflowDraft, error) {
	switch d.State {
	case WorkflowAwaitingMeet, WorkflowAwaitingMedia, WorkflowReviewing:
	default:
		return WorkflowDraft{}, fmt.Errorf("%w: workflow state %q", ErrUserInvalid, d.State)
	}
	if d.UserID <= 0 {
		return WorkflowDraft{}, fmt.Errorf("%w: user id", ErrUserInvalid)
	}
	if utf8.RuneCountInString(d.MeetURL) > MaxPlaylistURLRunes ||
		utf8.RuneCountInString(d.MediaURL) > MaxPlaylistURLRunes {
		return WorkflowDraft{}, fmt.Errorf("%w: workflow URL too long", ErrUserInvalid)
	}
	if len(d.ExtraQueue) > MaxPlaylistItems {
		return WorkflowDraft{}, fmt.Errorf("%w: workflow queue too large", ErrUserInvalid)
	}
	if utf8.RuneCountInString(d.MediaKind) > 24 {
		d.MediaKind = truncateRunes(d.MediaKind, 24)
	}
	return d, nil
}

// SaveWorkflowDraft upserts the user's creation draft. The draft replaces
// any previous one: a user has at most one wizard in flight.
func (us *UserStore) SaveWorkflowDraft(ctx context.Context, d WorkflowDraft) error {
	d, err := ValidateWorkflowDraft(d)
	if err != nil {
		return err
	}
	queue, err := json.Marshal(d.ExtraQueue)
	if err != nil {
		return fmt.Errorf("encode workflow queue: %w", err)
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	_, err = us.db.ExecContext(ctx,
		`INSERT INTO user_workflows (user_id, state, meet_url, media_url, media_kind, extra_queue, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET state = excluded.state, meet_url = excluded.meet_url,
			media_url = excluded.media_url, media_kind = excluded.media_kind,
			extra_queue = excluded.extra_queue, updated_at = excluded.updated_at`,
		d.UserID, d.State, d.MeetURL, d.MediaURL, d.MediaKind, string(queue), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("save workflow draft: %w", err)
	}
	return nil
}

// LoadWorkflowDraft returns the user's draft, or found=false. Stale drafts
// (older than maxAge) are treated as absent and deleted on read, so a
// restart never resurrects an abandoned wizard.
func (us *UserStore) LoadWorkflowDraft(ctx context.Context, userID int64, maxAge time.Duration) (WorkflowDraft, bool, error) {
	if userID <= 0 {
		return WorkflowDraft{}, false, nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()

	var d WorkflowDraft
	var queue string
	var updated int64
	err := us.db.QueryRowContext(ctx,
		`SELECT user_id, state, meet_url, media_url, media_kind, extra_queue, updated_at
		 FROM user_workflows WHERE user_id = ?`, userID).
		Scan(&d.UserID, &d.State, &d.MeetURL, &d.MediaURL, &d.MediaKind, &queue, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowDraft{}, false, nil
	}
	if err != nil {
		return WorkflowDraft{}, false, fmt.Errorf("load workflow draft: %w", err)
	}
	if maxAge > 0 && time.Since(time.Unix(updated, 0)) > maxAge {
		_, _ = us.db.ExecContext(ctx, `DELETE FROM user_workflows WHERE user_id = ?`, userID)
		return WorkflowDraft{}, false, nil
	}
	d.UpdatedAt = time.Unix(updated, 0)
	if err := json.Unmarshal([]byte(queue), &d.ExtraQueue); err != nil {
		// A malformed queue payload must not lose the URLs.
		d.ExtraQueue = nil
	}
	d, err = ValidateWorkflowDraft(d)
	if err != nil {
		// Unknown persisted state (older schema, corrupt row): drop it.
		_, _ = us.db.ExecContext(ctx, `DELETE FROM user_workflows WHERE user_id = ?`, userID)
		return WorkflowDraft{}, false, nil
	}
	return d, true, nil
}

// DeleteWorkflowDraft removes the user's draft. It is a no-op when none
// exists, so cancel paths can call it unconditionally.
func (us *UserStore) DeleteWorkflowDraft(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	if _, err := us.db.ExecContext(ctx, `DELETE FROM user_workflows WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete workflow draft: %w", err)
	}
	return nil
}

// BackdateWorkflowDraftForTest ages a draft by the given duration so the
// expiry path can be exercised without sleeping. Test-only helper.
func (us *UserStore) BackdateWorkflowDraftForTest(userID int64, age time.Duration) error {
	if userID <= 0 {
		return nil
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	_, err := us.db.ExecContext(context.Background(),
		`UPDATE user_workflows SET updated_at = updated_at - ? WHERE user_id = ?`,
		int64(age.Seconds()), userID)
	if err != nil {
		return fmt.Errorf("backdate workflow draft: %w", err)
	}
	return nil
}

// QueueStats is the aggregate shown on the management storage screen.
type QueueStats struct {
	Queues      int
	Active      int
	Interrupted int
	Items       int
}

// QueueStats returns global counters over the persisted queue metadata.
func (us *UserStore) QueueStats(ctx context.Context) (QueueStats, error) {
	us.mu.Lock()
	defer us.mu.Unlock()
	var st QueueStats
	if err := us.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_queues`).Scan(&st.Queues); err != nil {
		return st, fmt.Errorf("count session queues: %w", err)
	}
	if err := us.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_queues WHERE state = ?`, QueueStateActive).Scan(&st.Active); err != nil {
		return st, fmt.Errorf("count active queues: %w", err)
	}
	if err := us.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_queues WHERE state = ?`, QueueStateInterrupted).Scan(&st.Interrupted); err != nil {
		return st, fmt.Errorf("count interrupted queues: %w", err)
	}
	if err := us.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_queue_items`).Scan(&st.Items); err != nil {
		return st, fmt.Errorf("count queue items: %w", err)
	}
	return st, nil
}
