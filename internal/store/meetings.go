package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Meeting is one saved Google Meet link owned by a Telegram user.
type Meeting struct {
	ID        int64
	OwnerID   int64
	Name      string
	MeetURL   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MeetingStore persists saved meetings in SQLite. It is concurrency-safe:
// every method holds the store mutex and uses prepared statements, and the
// connection pool is capped at one connection so writers serialize on the
// database itself.
type MeetingStore struct {
	mu sync.Mutex
	db *sql.DB
}

// meetingSchemaVersion is the schema version recorded in schema_migrations.
// Bump together with meetingMigrations when adding a step.
const meetingSchemaVersion = 1

// maxSavedMeetings bounds the meetings one owner can keep, so a runaway
// client cannot grow the table without limit.
const maxSavedMeetings = 100

// meetingMigrations maps a schema version to its idempotent DDL. Each step
// is written to be re-runnable (CREATE ... IF NOT EXISTS).
var meetingMigrations = map[int]string{
	1: `CREATE TABLE IF NOT EXISTS saved_meetings (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		owner_id   INTEGER NOT NULL,
		name       TEXT    NOT NULL,
		meet_url   TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_saved_meetings_owner_url
		ON saved_meetings(owner_id, meet_url);
	CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);`,
}

var (
	// ErrMeetingNotFound is returned for missing or foreign meetings.
	ErrMeetingNotFound = errors.New("meeting not found")
	// ErrDuplicateURL is returned when an owner already saved the same URL.
	ErrDuplicateURL = errors.New("you already saved this meeting link")
	// ErrTooManyMeetings is returned when an owner hits maxSavedMeetings.
	ErrTooManyMeetings = errors.New("too many saved meetings")
)

// NewMeetingStore opens (or creates) the SQLite database at path and runs
// idempotent migrations. The parent directory is created when missing.
func NewMeetingStore(ctx context.Context, path string) (*MeetingStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("meeting store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	// Refuse a path that exists but is not a regular file: never open a
	// directory, device, or special file as a database.
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("db path %s is not a regular file", path)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection serializes writers; readers retry on busy_timeout.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	ms := &MeetingStore{db: db}
	if err := ms.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return ms, nil
}

// Close releases the database handle. It is safe to call on a nil or closed
// store.
func (ms *MeetingStore) Close() error {
	if ms == nil {
		return nil
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.db == nil {
		return nil
	}
	err := ms.db.Close()
	ms.db = nil
	return err
}

// DB returns the underlying connection, for diagnostics. Callers must not
// close it.
func (ms *MeetingStore) DB() *sql.DB {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.db
}

// migrate applies migrations sequentially and records the applied version.
// Migrations are idempotent and safe to re-run on an existing database.
func (ms *MeetingStore) migrate(ctx context.Context) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if _, err := ms.db.ExecContext(ctx, meetingMigrations[1]); err != nil {
		return fmt.Errorf("apply migration 1: %w", err)
	}
	// Record the baseline so future versions can diff upward.
	var version int
	err := ms.db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := ms.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			meetingSchemaVersion, time.Now().Unix()); err != nil {
			return fmt.Errorf("record schema version: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	return nil
}

// Save adds a meeting for owner, or refreshes the name when the owner already
// saved the same URL. Saving an identical (url, name) pair is a no-op that
// returns the existing row without touching the disk.
func (ms *MeetingStore) Save(ctx context.Context, ownerID int64, name, meetURL string) (Meeting, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	name = strings.TrimSpace(name)
	meetURL = strings.TrimSpace(meetURL)
	if name == "" {
		return Meeting{}, errors.New("meeting name is empty")
	}
	if meetURL == "" {
		return Meeting{}, errors.New("meeting url is empty")
	}
	if len(name) > 100 {
		return Meeting{}, errors.New("meeting name is too long (max 100 characters)")
	}

	var existing Meeting
	var created, updated int64
	row := ms.db.QueryRowContext(ctx,
		`SELECT id, owner_id, name, meet_url, created_at, updated_at
		 FROM saved_meetings WHERE owner_id = ? AND meet_url = ?`,
		ownerID, meetURL)
	err := row.Scan(&existing.ID, &existing.OwnerID, &existing.Name, &existing.MeetURL, &created, &updated)
	switch {
	case err == nil:
		existing.CreatedAt = time.Unix(created, 0)
		existing.UpdatedAt = time.Unix(updated, 0)
		// Same owner, same URL: refresh the name only.
		if existing.Name == name {
			return existing, nil
		}
		now := time.Now().Unix()
		if _, err := ms.db.ExecContext(ctx,
			`UPDATE saved_meetings SET name = ?, updated_at = ? WHERE id = ? AND owner_id = ?`,
			name, now, existing.ID, ownerID); err != nil {
			return Meeting{}, fmt.Errorf("update meeting: %w", err)
		}
		existing.Name = name
		existing.UpdatedAt = time.Unix(now, 0)
		return existing, nil
	case errors.Is(err, sql.ErrNoRows):
		// fall through to insert
	default:
		return Meeting{}, fmt.Errorf("lookup meeting: %w", err)
	}

	var count int
	if err := ms.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM saved_meetings WHERE owner_id = ?`, ownerID).Scan(&count); err != nil {
		return Meeting{}, fmt.Errorf("count meetings: %w", err)
	}
	if count >= maxSavedMeetings {
		return Meeting{}, ErrTooManyMeetings
	}

	now := time.Now()
	res, err := ms.db.ExecContext(ctx,
		`INSERT INTO saved_meetings (owner_id, name, meet_url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		ownerID, name, meetURL, now.Unix(), now.Unix())
	if err != nil {
		return Meeting{}, fmt.Errorf("insert meeting: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Meeting{}, fmt.Errorf("last insert id: %w", err)
	}
	return Meeting{ID: id, OwnerID: ownerID, Name: name, MeetURL: meetURL,
		CreatedAt: now, UpdatedAt: now}, nil
}

// List returns the meetings of owner, most recently updated first, bounded
// by maxSavedMeetings.
func (ms *MeetingStore) List(ctx context.Context, ownerID int64) ([]Meeting, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	rows, err := ms.db.QueryContext(ctx,
		`SELECT id, owner_id, name, meet_url, created_at, updated_at
		 FROM saved_meetings WHERE owner_id = ?
		 ORDER BY updated_at DESC, id DESC LIMIT ?`, ownerID, maxSavedMeetings)
	if err != nil {
		return nil, fmt.Errorf("list meetings: %w", err)
	}
	defer rows.Close()
	return scanMeetings(rows)
}

// Get returns one meeting, but only when it belongs to ownerID. A missing
// row and a foreign row are indistinguishable: both are ErrMeetingNotFound.
func (ms *MeetingStore) Get(ctx context.Context, ownerID, id int64) (Meeting, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	var m Meeting
	var created, updated int64
	err := ms.db.QueryRowContext(ctx,
		`SELECT id, owner_id, name, meet_url, created_at, updated_at
		 FROM saved_meetings WHERE id = ? AND owner_id = ?`, id, ownerID).
		Scan(&m.ID, &m.OwnerID, &m.Name, &m.MeetURL, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Meeting{}, ErrMeetingNotFound
	}
	if err != nil {
		return Meeting{}, fmt.Errorf("get meeting: %w", err)
	}
	m.CreatedAt = time.Unix(created, 0)
	m.UpdatedAt = time.Unix(updated, 0)
	return m, nil
}

// Rename renames a meeting owned by ownerID.
func (ms *MeetingStore) Rename(ctx context.Context, ownerID, id int64, name string) (Meeting, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return Meeting{}, errors.New("meeting name is empty")
	}
	if len(name) > 100 {
		return Meeting{}, errors.New("meeting name is too long (max 100 characters)")
	}

	now := time.Now().Unix()
	res, err := ms.db.ExecContext(ctx,
		`UPDATE saved_meetings SET name = ?, updated_at = ? WHERE id = ? AND owner_id = ?`,
		name, now, id, ownerID)
	if err != nil {
		return Meeting{}, fmt.Errorf("rename meeting: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Meeting{}, fmt.Errorf("rename meeting rows: %w", err)
	}
	if n == 0 {
		return Meeting{}, ErrMeetingNotFound
	}
	return Meeting{ID: id, OwnerID: ownerID, Name: name, MeetURL: "",
		CreatedAt: time.Unix(0, 0), UpdatedAt: time.Unix(now, 0)}, nil
}

// Delete removes a meeting owned by ownerID.
func (ms *MeetingStore) Delete(ctx context.Context, ownerID, id int64) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	res, err := ms.db.ExecContext(ctx,
		`DELETE FROM saved_meetings WHERE id = ? AND owner_id = ?`, id, ownerID)
	if err != nil {
		return fmt.Errorf("delete meeting: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete meeting rows: %w", err)
	}
	if n == 0 {
		return ErrMeetingNotFound
	}
	return nil
}

// scanMeetings converts sql.Rows into []Meeting.
func scanMeetings(rows *sql.Rows) ([]Meeting, error) {
	var out []Meeting
	for rows.Next() {
		var m Meeting
		var created, updated int64
		if err := rows.Scan(&m.ID, &m.OwnerID, &m.Name, &m.MeetURL, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan meeting: %w", err)
		}
		m.CreatedAt = time.Unix(created, 0)
		m.UpdatedAt = time.Unix(updated, 0)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate meetings: %w", err)
	}
	return out, nil
}

// BackupDB copies the database file to <path>.bak-<unix>. Call it before a
// destructive change (schema migration, bulk delete). Best-effort: a missing
// source file is not an error, since a freshly created database has no
// legacy data to protect yet.
func BackupDB(path string) (string, error) {
	src, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	dst := fmt.Sprintf("%s.bak-%d", path, time.Now().Unix())
	if err := os.WriteFile(dst, src, 0o600); err != nil {
		return "", err
	}
	return dst, nil
}
