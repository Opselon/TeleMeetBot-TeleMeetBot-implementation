package store

// Playlist persistence.
//
// A playlist is the long-lived counterpart of a session queue: a named,
// owned, ordered list of media links that survives bot restarts and can be
// replayed into any meeting. Session queues live in memory and die with the
// session; playlists live in SQLite next to the saved meetings.
//
// The store is deliberately small and boring:
//
//   - every write goes through one mutex and one database connection, so
//     concurrent Telegram updates cannot interleave a rename with a reorder;
//   - positions are contiguous (0..n-1) after every mutation, which keeps
//     the dashboard numbering stable and makes "move up" a simple swap;
//   - ownership is part of every lookup, so a forged callback id can never
//     read or mutate somebody else's playlist.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Playlist limits. They are enforced here rather than only in the Telegram
// layer so a future API or importer cannot grow the database without bound.
const (
	// MaxPlaylistsPerOwner bounds one user's library.
	MaxPlaylistsPerOwner = 100
	// MaxPlaylistItems bounds one playlist. A 500-item playlist is already
	// several days of continuous playback.
	MaxPlaylistItems = 500
	// MaxPlaylistNameRunes is the display-name ceiling.
	MaxPlaylistNameRunes = 60
	// MaxPlaylistDescriptionRunes is the description ceiling.
	MaxPlaylistDescriptionRunes = 240
	// MaxPlaylistTitleRunes bounds the stored per-item title.
	MaxPlaylistTitleRunes = 120
	// MaxPlaylistURLRunes bounds the stored URL.
	MaxPlaylistURLRunes = 2048
)

// Playlist is one named media library belonging to a Telegram user.
type Playlist struct {
	ID          int64
	OwnerID     int64
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// ItemCount and TotalDuration are derived on read; they are never
	// written back, so a stale value cannot leak into the database.
	ItemCount     int
	TotalDuration float64
}

// PlaylistItem is one media link inside a playlist. Position is zero-based
// and contiguous within a playlist.
type PlaylistItem struct {
	ID         int64
	PlaylistID int64
	Position   int
	URL        string
	Title      string
	Kind       string
	Duration   float64
	CreatedAt  time.Time
}

// PlaylistItemInput is the write shape used when adding media. Duration is
// optional metadata (probed titles/durations may arrive later).
type PlaylistItemInput struct {
	URL      string
	Title    string
	Kind     string
	Duration float64
}

// PlaylistStats is the aggregate used by the admin console.
type PlaylistStats struct {
	Playlists int
	Items     int
	Owners    int
}

var (
	// ErrPlaylistNotFound is returned for missing or foreign playlists.
	ErrPlaylistNotFound = errors.New("playlist not found")
	// ErrPlaylistItemNotFound is returned for missing or foreign items.
	ErrPlaylistItemNotFound = errors.New("playlist item not found")
	// ErrPlaylistNameTaken is returned when the owner already uses a name.
	ErrPlaylistNameTaken = errors.New("a playlist with this name already exists")
	// ErrTooManyPlaylists is returned when the owner hits the library cap.
	ErrTooManyPlaylists = errors.New("too many playlists")
	// ErrTooManyPlaylistItems is returned when one playlist hits its cap.
	ErrTooManyPlaylistItems = errors.New("too many items in this playlist")
	// ErrPlaylistEmpty is returned when an operation requires items.
	ErrPlaylistEmpty = errors.New("playlist is empty")
	// ErrPlaylistNameInvalid wraps name validation failures.
	ErrPlaylistNameInvalid = errors.New("invalid playlist name")
	// ErrPlaylistURLInvalid wraps media-link validation failures.
	ErrPlaylistURLInvalid = errors.New("invalid media link")
	// ErrPlaylistDescriptionInvalid wraps description validation failures.
	ErrPlaylistDescriptionInvalid = errors.New("invalid playlist description")
)

// playlistSchemaVersion is recorded in schema_migrations. Bump it together
// with a new idempotent step in playlistMigrations.
const playlistSchemaVersion = 1

// playlistMigrations maps a version to idempotent DDL. The playlists database
// is separate from meetings.db: a corrupt library can be deleted without
// touching saved meeting links, and vice versa.
var playlistMigrations = map[int]string{
	1: `CREATE TABLE IF NOT EXISTS playlists (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		owner_id    INTEGER NOT NULL,
		name        TEXT    NOT NULL,
		description TEXT    NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_playlists_owner_name
		ON playlists(owner_id, name COLLATE NOCASE);
	CREATE INDEX IF NOT EXISTS idx_playlists_owner_updated
		ON playlists(owner_id, updated_at DESC);
	CREATE TABLE IF NOT EXISTS playlist_items (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
		position    INTEGER NOT NULL,
		url         TEXT    NOT NULL,
		title       TEXT    NOT NULL DEFAULT '',
		kind        TEXT    NOT NULL DEFAULT '',
		duration    REAL    NOT NULL DEFAULT 0,
		created_at  INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_playlist_items_playlist_position
		ON playlist_items(playlist_id, position);
	CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	);`,
}

// PlaylistStore is the concurrency-safe playlist database.
type PlaylistStore struct {
	mu sync.Mutex
	db *sql.DB
}

// NewPlaylistStore opens (or creates) the SQLite database at path and applies
// idempotent migrations. The parent directory is created when missing.
func NewPlaylistStore(ctx context.Context, path string) (*PlaylistStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("playlist store path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create playlist db dir: %w", err)
	}
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("playlist db path %s is not a regular file", path)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open playlist sqlite: %w", err)
	}
	// One connection serializes writers; a WAL reader can still proceed
	// between transactions, and busy_timeout covers the short overlaps.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping playlist sqlite: %w", err)
	}
	ps := &PlaylistStore{db: db}
	if err := ps.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return ps, nil
}

// Close releases the database handle. It is safe on a nil or closed store.
func (ps *PlaylistStore) Close() error {
	if ps == nil {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.db == nil {
		return nil
	}
	err := ps.db.Close()
	ps.db = nil
	return err
}

// DB exposes the connection for diagnostics. Callers must not close it.
func (ps *PlaylistStore) DB() *sql.DB {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.db
}

// migrate applies the baseline DDL and records the schema version.
func (ps *PlaylistStore) migrate(ctx context.Context) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if _, err := ps.db.ExecContext(ctx, playlistMigrations[1]); err != nil {
		return fmt.Errorf("apply playlist migration 1: %w", err)
	}
	var version int
	err := ps.db.QueryRowContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := ps.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			playlistSchemaVersion, time.Now().Unix()); err != nil {
			return fmt.Errorf("record playlist schema version: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read playlist schema version: %w", err)
	}
	return nil
}

// ValidatePlaylistName trims and validates a display name. The returned value
// is the canonical form that is stored.
func ValidatePlaylistName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", fmt.Errorf("%w: name is empty", ErrPlaylistNameInvalid)
	}
	if utf8.RuneCountInString(name) > MaxPlaylistNameRunes {
		return "", fmt.Errorf("%w: too long (max %d characters)", ErrPlaylistNameInvalid, MaxPlaylistNameRunes)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: contains control characters", ErrPlaylistNameInvalid)
		}
	}
	return name, nil
}

// ValidatePlaylistDescription trims and validates a description. Empty is
// valid: descriptions are optional.
func ValidatePlaylistDescription(raw string) (string, error) {
	desc := strings.TrimSpace(raw)
	if utf8.RuneCountInString(desc) > MaxPlaylistDescriptionRunes {
		return "", fmt.Errorf("%w: too long (max %d characters)", ErrPlaylistDescriptionInvalid, MaxPlaylistDescriptionRunes)
	}
	for _, r := range desc {
		if r < 0x20 && r != '\n' && r != '\t' {
			return "", fmt.Errorf("%w: contains control characters", ErrPlaylistDescriptionInvalid)
		}
		if r == 0x7f {
			return "", fmt.Errorf("%w: contains control characters", ErrPlaylistDescriptionInvalid)
		}
	}
	return desc, nil
}

// ValidatePlaylistURL accepts only absolute http(s) links. The bot classifies
// links before they get here; this is the storage-level fence.
func ValidatePlaylistURL(raw string) (string, error) {
	link := strings.TrimSpace(raw)
	if link == "" {
		return "", fmt.Errorf("%w: link is empty", ErrPlaylistURLInvalid)
	}
	if utf8.RuneCountInString(link) > MaxPlaylistURLRunes {
		return "", fmt.Errorf("%w: too long (max %d characters)", ErrPlaylistURLInvalid, MaxPlaylistURLRunes)
	}
	u, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("%w: not a valid URL", ErrPlaylistURLInvalid)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("%w: must start with http:// or https://", ErrPlaylistURLInvalid)
	}
	if strings.TrimSpace(u.Host) == "" {
		return "", fmt.Errorf("%w: no host", ErrPlaylistURLInvalid)
	}
	return link, nil
}

// DefaultPlaylistItemTitle derives a readable title from a URL when the
// caller did not provide one. It never returns an empty string.
func DefaultPlaylistItemTitle(raw string) string {
	link := strings.TrimSpace(raw)
	if link == "" {
		return "Untitled media"
	}
	u, err := url.Parse(link)
	if err == nil {
		if q := u.Query().Get("v"); q != "" && strings.Contains(strings.ToLower(u.Host), "youtu") {
			return truncateRunes("youtube:"+q, MaxPlaylistTitleRunes)
		}
	}
	name := link
	if idx := strings.LastIndex(name, "/"); idx >= 0 && idx+1 < len(name) {
		name = name[idx+1:]
	}
	if q := strings.IndexAny(name, "?#"); q >= 0 {
		name = name[:q]
	}
	if decoded, err := url.QueryUnescape(name); err == nil {
		name = decoded
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return truncateRunes(link, MaxPlaylistTitleRunes)
	}
	return truncateRunes(name, MaxPlaylistTitleRunes)
}

// truncateRunes shortens s to at most n runes, preserving valid UTF-8.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// Create inserts a playlist for ownerID.
func (ps *PlaylistStore) Create(ctx context.Context, ownerID int64, name, description string) (Playlist, error) {
	name, err := ValidatePlaylistName(name)
	if err != nil {
		return Playlist{}, err
	}
	desc, err := ValidatePlaylistDescription(description)
	if err != nil {
		return Playlist{}, err
	}
	if ownerID <= 0 {
		return Playlist{}, errors.New("invalid owner id")
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()

	var count int
	if err := ps.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playlists WHERE owner_id = ?`, ownerID).Scan(&count); err != nil {
		return Playlist{}, fmt.Errorf("count playlists: %w", err)
	}
	if count >= MaxPlaylistsPerOwner {
		return Playlist{}, ErrTooManyPlaylists
	}

	now := time.Now()
	res, err := ps.db.ExecContext(ctx,
		`INSERT INTO playlists (owner_id, name, description, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		ownerID, name, desc, now.Unix(), now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return Playlist{}, ErrPlaylistNameTaken
		}
		return Playlist{}, fmt.Errorf("insert playlist: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Playlist{}, fmt.Errorf("playlist insert id: %w", err)
	}
	return Playlist{ID: id, OwnerID: ownerID, Name: name, Description: desc,
		CreatedAt: now, UpdatedAt: now}, nil
}

// List returns the owner's playlists, most recently updated first.
func (ps *PlaylistStore) List(ctx context.Context, ownerID int64) ([]Playlist, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	rows, err := ps.db.QueryContext(ctx,
		`SELECT p.id, p.owner_id, p.name, p.description, p.created_at, p.updated_at,
		        COUNT(i.id), COALESCE(SUM(i.duration), 0)
		 FROM playlists p
		 LEFT JOIN playlist_items i ON i.playlist_id = p.id
		 WHERE p.owner_id = ?
		 GROUP BY p.id
		 ORDER BY p.updated_at DESC, p.id DESC`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list playlists: %w", err)
	}
	defer rows.Close()

	var out []Playlist
	for rows.Next() {
		var p Playlist
		var created, updated int64
		if err := rows.Scan(&p.ID, &p.OwnerID, &p.Name, &p.Description,
			&created, &updated, &p.ItemCount, &p.TotalDuration); err != nil {
			return nil, fmt.Errorf("scan playlist: %w", err)
		}
		p.CreatedAt = time.Unix(created, 0)
		p.UpdatedAt = time.Unix(updated, 0)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate playlists: %w", err)
	}
	return out, nil
}

// Get returns one playlist owned by ownerID.
func (ps *PlaylistStore) Get(ctx context.Context, ownerID, id int64) (Playlist, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	var p Playlist
	var created, updated int64
	err := ps.db.QueryRowContext(ctx,
		`SELECT p.id, p.owner_id, p.name, p.description, p.created_at, p.updated_at,
		        (SELECT COUNT(*) FROM playlist_items WHERE playlist_id = p.id),
		        (SELECT COALESCE(SUM(duration), 0) FROM playlist_items WHERE playlist_id = p.id)
		 FROM playlists p WHERE p.id = ? AND p.owner_id = ?`, id, ownerID).
		Scan(&p.ID, &p.OwnerID, &p.Name, &p.Description, &created, &updated,
			&p.ItemCount, &p.TotalDuration)
	if errors.Is(err, sql.ErrNoRows) {
		return Playlist{}, ErrPlaylistNotFound
	}
	if err != nil {
		return Playlist{}, fmt.Errorf("get playlist: %w", err)
	}
	p.CreatedAt = time.Unix(created, 0)
	p.UpdatedAt = time.Unix(updated, 0)
	return p, nil
}

// Rename changes a playlist's display name.
func (ps *PlaylistStore) Rename(ctx context.Context, ownerID, id int64, name string) (Playlist, error) {
	name, err := ValidatePlaylistName(name)
	if err != nil {
		return Playlist{}, err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()

	now := time.Now().Unix()
	res, err := ps.db.ExecContext(ctx,
		`UPDATE playlists SET name = ?, updated_at = ? WHERE id = ? AND owner_id = ?`,
		name, now, id, ownerID)
	if err != nil {
		if isUniqueViolation(err) {
			return Playlist{}, ErrPlaylistNameTaken
		}
		return Playlist{}, fmt.Errorf("rename playlist: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Playlist{}, fmt.Errorf("rename playlist rows: %w", err)
	}
	if n == 0 {
		return Playlist{}, ErrPlaylistNotFound
	}
	return Playlist{ID: id, OwnerID: ownerID, Name: name, UpdatedAt: time.Unix(now, 0)}, nil
}

// Describe changes a playlist's optional description.
func (ps *PlaylistStore) Describe(ctx context.Context, ownerID, id int64, description string) error {
	desc, err := ValidatePlaylistDescription(description)
	if err != nil {
		return err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()

	res, err := ps.db.ExecContext(ctx,
		`UPDATE playlists SET description = ?, updated_at = ? WHERE id = ? AND owner_id = ?`,
		desc, time.Now().Unix(), id, ownerID)
	if err != nil {
		return fmt.Errorf("describe playlist: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("describe playlist rows: %w", err)
	}
	if n == 0 {
		return ErrPlaylistNotFound
	}
	return nil
}

// Delete removes a playlist and, through the foreign key cascade, its items.
func (ps *PlaylistStore) Delete(ctx context.Context, ownerID, id int64) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	res, err := ps.db.ExecContext(ctx,
		`DELETE FROM playlists WHERE id = ? AND owner_id = ?`, id, ownerID)
	if err != nil {
		return fmt.Errorf("delete playlist: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete playlist rows: %w", err)
	}
	if n == 0 {
		return ErrPlaylistNotFound
	}
	return nil
}

// Duplicate clones a playlist and all of its items. The copy gets a free
// "name (copy)" style name so the unique index is never violated.
func (ps *PlaylistStore) Duplicate(ctx context.Context, ownerID, id int64) (Playlist, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	var name, desc string
	if err := ps.db.QueryRowContext(ctx,
		`SELECT name, description FROM playlists WHERE id = ? AND owner_id = ?`, id, ownerID).
		Scan(&name, &desc); errors.Is(err, sql.ErrNoRows) {
		return Playlist{}, ErrPlaylistNotFound
	} else if err != nil {
		return Playlist{}, fmt.Errorf("read playlist to duplicate: %w", err)
	}

	var total int
	if err := ps.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playlists WHERE owner_id = ?`, ownerID).Scan(&total); err != nil {
		return Playlist{}, fmt.Errorf("count playlists: %w", err)
	}
	if total >= MaxPlaylistsPerOwner {
		return Playlist{}, ErrTooManyPlaylists
	}

	now := time.Now()
	tx, err := ps.db.BeginTx(ctx, nil)
	if err != nil {
		return Playlist{}, fmt.Errorf("begin duplicate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	copyName := ""
	for attempt := 1; attempt <= 50; attempt++ {
		candidate := name + " (copy)"
		if attempt > 1 {
			candidate = fmt.Sprintf("%s (copy %d)", name, attempt)
		}
		if candidate, err = ValidatePlaylistName(candidate); err != nil {
			// A very long source name may not leave room for a suffix.
			// Trim it until it does.
			base := truncateRunes(name, MaxPlaylistNameRunes-12)
			candidate = fmt.Sprintf("%s (copy %d)", base, attempt)
		}
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM playlists WHERE owner_id = ? AND name = ? COLLATE NOCASE`,
			ownerID, candidate).Scan(&exists); err != nil {
			return Playlist{}, fmt.Errorf("check copy name: %w", err)
		}
		if exists == 0 {
			copyName = candidate
			break
		}
	}
	if copyName == "" {
		return Playlist{}, ErrPlaylistNameTaken
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO playlists (owner_id, name, description, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		ownerID, copyName, desc, now.Unix(), now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return Playlist{}, ErrPlaylistNameTaken
		}
		return Playlist{}, fmt.Errorf("insert playlist copy: %w", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return Playlist{}, fmt.Errorf("copy insert id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO playlist_items (playlist_id, position, url, title, kind, duration, created_at)
		 SELECT ?, position, url, title, kind, duration, ? FROM playlist_items
		 WHERE playlist_id = ? ORDER BY position, id`,
		newID, now.Unix(), id); err != nil {
		return Playlist{}, fmt.Errorf("copy playlist items: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Playlist{}, fmt.Errorf("commit duplicate: %w", err)
	}
	return ps.getLocked(ctx, ownerID, newID)
}

// Items returns the ordered items of a playlist owned by ownerID.
func (ps *PlaylistStore) Items(ctx context.Context, ownerID, id int64) ([]PlaylistItem, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.itemsLocked(ctx, ownerID, id)
}

// itemsLocked is Items without the mutex; callers already hold it.
func (ps *PlaylistStore) itemsLocked(ctx context.Context, ownerID, id int64) ([]PlaylistItem, error) {
	// Ownership check first: an empty foreign playlist must be
	// indistinguishable from a missing one.
	var exists int
	err := ps.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playlists WHERE id = ? AND owner_id = ?`, id, ownerID).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("check playlist owner: %w", err)
	}
	if exists == 0 {
		return nil, ErrPlaylistNotFound
	}
	rows, err := ps.db.QueryContext(ctx,
		`SELECT id, playlist_id, position, url, title, kind, duration, created_at
		 FROM playlist_items WHERE playlist_id = ? ORDER BY position, id`, id)
	if err != nil {
		return nil, fmt.Errorf("list playlist items: %w", err)
	}
	defer rows.Close()

	var out []PlaylistItem
	for rows.Next() {
		var it PlaylistItem
		var created int64
		if err := rows.Scan(&it.ID, &it.PlaylistID, &it.Position, &it.URL,
			&it.Title, &it.Kind, &it.Duration, &created); err != nil {
			return nil, fmt.Errorf("scan playlist item: %w", err)
		}
		it.CreatedAt = time.Unix(created, 0)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate playlist items: %w", err)
	}
	return out, nil
}

// AddItem appends one media link to a playlist.
func (ps *PlaylistStore) AddItem(ctx context.Context, ownerID, id int64, in PlaylistItemInput) (PlaylistItem, error) {
	items, err := ps.AddItems(ctx, ownerID, id, []PlaylistItemInput{in})
	if err != nil {
		return PlaylistItem{}, err
	}
	return items[0], nil
}

// AddItems appends media links in submission order. The whole batch is
// inserted in one transaction: either every valid item lands or none does.
// Invalid entries are rejected before the transaction starts, so the caller
// gets a precise error instead of a silently shorter playlist.
func (ps *PlaylistStore) AddItems(ctx context.Context, ownerID, id int64, in []PlaylistItemInput) ([]PlaylistItem, error) {
	if len(in) == 0 {
		return nil, errors.New("no media links to add")
	}
	if len(in) > MaxPlaylistItems {
		return nil, ErrTooManyPlaylistItems
	}
	normalized := make([]PlaylistItemInput, 0, len(in))
	for _, raw := range in {
		link, err := ValidatePlaylistURL(raw.URL)
		if err != nil {
			return nil, err
		}
		title := strings.TrimSpace(raw.Title)
		if title == "" {
			title = DefaultPlaylistItemTitle(link)
		}
		title = truncateRunes(title, MaxPlaylistTitleRunes)
		kind := strings.TrimSpace(raw.Kind)
		if utf8.RuneCountInString(kind) > 24 {
			kind = truncateRunes(kind, 24)
		}
		duration := raw.Duration
		if duration < 0 || duration != duration { // NaN guard
			duration = 0
		}
		normalized = append(normalized, PlaylistItemInput{URL: link, Title: title, Kind: kind, Duration: duration})
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()

	var exists int
	if err := ps.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playlists WHERE id = ? AND owner_id = ?`, id, ownerID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check playlist owner: %w", err)
	}
	if exists == 0 {
		return nil, ErrPlaylistNotFound
	}
	var count int
	if err := ps.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playlist_items WHERE playlist_id = ?`, id).Scan(&count); err != nil {
		return nil, fmt.Errorf("count playlist items: %w", err)
	}
	if count+len(normalized) > MaxPlaylistItems {
		return nil, ErrTooManyPlaylistItems
	}

	now := time.Now()
	tx, err := ps.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin add items: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var maxPos sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(position) FROM playlist_items WHERE playlist_id = ?`, id).Scan(&maxPos); err != nil {
		return nil, fmt.Errorf("read last position: %w", err)
	}
	pos := 0
	if maxPos.Valid {
		pos = int(maxPos.Int64) + 1
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO playlist_items (playlist_id, position, url, title, kind, duration, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare add items: %w", err)
	}
	defer stmt.Close()

	out := make([]PlaylistItem, 0, len(normalized))
	for _, item := range normalized {
		res, err := stmt.ExecContext(ctx, id, pos, item.URL, item.Title, item.Kind, item.Duration, now.Unix())
		if err != nil {
			return nil, fmt.Errorf("insert playlist item: %w", err)
		}
		itemID, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("playlist item id: %w", err)
		}
		out = append(out, PlaylistItem{
			ID: itemID, PlaylistID: id, Position: pos, URL: item.URL,
			Title: item.Title, Kind: item.Kind, Duration: item.Duration, CreatedAt: now,
		})
		pos++
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlists SET updated_at = ? WHERE id = ? AND owner_id = ?`,
		now.Unix(), id, ownerID); err != nil {
		return nil, fmt.Errorf("touch playlist: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit add items: %w", err)
	}
	return out, nil
}

// RemoveItem deletes one item and closes the position gap.
func (ps *PlaylistStore) RemoveItem(ctx context.Context, ownerID, playlistID, itemID int64) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	tx, err := ps.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin remove item: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var pos int
	err = tx.QueryRowContext(ctx,
		`SELECT i.position FROM playlist_items i
		 JOIN playlists p ON p.id = i.playlist_id
		 WHERE i.id = ? AND i.playlist_id = ? AND p.owner_id = ?`,
		itemID, playlistID, ownerID).Scan(&pos)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPlaylistItemNotFound
	}
	if err != nil {
		return fmt.Errorf("find playlist item: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_items WHERE id = ? AND playlist_id = ?`,
		itemID, playlistID); err != nil {
		return fmt.Errorf("delete playlist item: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlist_items SET position = position - 1
		 WHERE playlist_id = ? AND position > ?`, playlistID, pos); err != nil {
		return fmt.Errorf("close playlist gap: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlists SET updated_at = ? WHERE id = ? AND owner_id = ?`,
		time.Now().Unix(), playlistID, ownerID); err != nil {
		return fmt.Errorf("touch playlist: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit remove item: %w", err)
	}
	return nil
}

// MoveItem swaps an item with its neighbor. delta must be -1 or +1; a move
// past either end is reported as an error rather than silently clamped, so
// the UI can explain why nothing happened.
func (ps *PlaylistStore) MoveItem(ctx context.Context, ownerID, playlistID, itemID int64, delta int) error {
	if delta != -1 && delta != 1 {
		return errors.New("move delta must be -1 or 1")
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()

	tx, err := ps.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin move item: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var pos int
	err = tx.QueryRowContext(ctx,
		`SELECT i.position FROM playlist_items i
		 JOIN playlists p ON p.id = i.playlist_id
		 WHERE i.id = ? AND i.playlist_id = ? AND p.owner_id = ?`,
		itemID, playlistID, ownerID).Scan(&pos)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPlaylistItemNotFound
	}
	if err != nil {
		return fmt.Errorf("find item to move: %w", err)
	}
	target := pos + delta
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playlist_items WHERE playlist_id = ?`, playlistID).Scan(&count); err != nil {
		return fmt.Errorf("count items for move: %w", err)
	}
	if target < 0 || target >= count {
		if delta < 0 {
			return errors.New("item is already first")
		}
		return errors.New("item is already last")
	}
	var neighborID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM playlist_items WHERE playlist_id = ? AND position = ?`,
		playlistID, target).Scan(&neighborID); err != nil {
		return fmt.Errorf("find neighbor item: %w", err)
	}
	// Swap through a temporary position so the two updates cannot collide
	// if a future schema adds a uniqueness constraint.
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlist_items SET position = -1 WHERE id = ?`, itemID); err != nil {
		return fmt.Errorf("park moved item: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlist_items SET position = ? WHERE id = ?`, pos, neighborID); err != nil {
		return fmt.Errorf("move neighbor item: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlist_items SET position = ? WHERE id = ?`, target, itemID); err != nil {
		return fmt.Errorf("place moved item: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playlists SET updated_at = ? WHERE id = ? AND owner_id = ?`,
		time.Now().Unix(), playlistID, ownerID); err != nil {
		return fmt.Errorf("touch playlist: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit move item: %w", err)
	}
	return nil
}

// ClearItems removes every item from a playlist. The playlist row stays.
func (ps *PlaylistStore) ClearItems(ctx context.Context, ownerID, playlistID int64) (int, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	var exists int
	if err := ps.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playlists WHERE id = ? AND owner_id = ?`, playlistID, ownerID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("check playlist owner: %w", err)
	}
	if exists == 0 {
		return 0, ErrPlaylistNotFound
	}
	res, err := ps.db.ExecContext(ctx,
		`DELETE FROM playlist_items WHERE playlist_id = ?`, playlistID)
	if err != nil {
		return 0, fmt.Errorf("clear playlist items: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := ps.db.ExecContext(ctx,
		`UPDATE playlists SET updated_at = ? WHERE id = ? AND owner_id = ?`,
		time.Now().Unix(), playlistID, ownerID); err != nil {
		return 0, fmt.Errorf("touch playlist: %w", err)
	}
	return int(n), nil
}

// Stats returns global, read-only counters for the admin console.
func (ps *PlaylistStore) Stats(ctx context.Context) (PlaylistStats, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	var st PlaylistStats
	if err := ps.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playlists`).Scan(&st.Playlists); err != nil {
		return st, fmt.Errorf("count all playlists: %w", err)
	}
	if err := ps.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playlist_items`).Scan(&st.Items); err != nil {
		return st, fmt.Errorf("count all playlist items: %w", err)
	}
	if err := ps.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT owner_id) FROM playlists`).Scan(&st.Owners); err != nil {
		return st, fmt.Errorf("count playlist owners: %w", err)
	}
	return st, nil
}

// Vacuum compacts the database file. It is intentionally not run on every
// delete: it rewrites the whole file and needs exclusive access.
func (ps *PlaylistStore) Vacuum(ctx context.Context) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if _, err := ps.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum playlist db: %w", err)
	}
	return nil
}

// Owners returns the distinct Telegram ids that own at least one playlist.
// The admin console merges this with the session history to build a complete
// user list.
func (ps *PlaylistStore) Owners(ctx context.Context) ([]int64, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	rows, err := ps.db.QueryContext(ctx,
		`SELECT DISTINCT owner_id FROM playlists ORDER BY owner_id`)
	if err != nil {
		return nil, fmt.Errorf("list playlist owners: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan playlist owner: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate playlist owners: %w", err)
	}
	return out, nil
}

// CountForOwner returns how many playlists one user owns.
func (ps *PlaylistStore) CountForOwner(ctx context.Context, ownerID int64) (int, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	var n int
	if err := ps.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM playlists WHERE owner_id = ?`, ownerID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count owner playlists: %w", err)
	}
	return n, nil
}

// getLocked reads one playlist without taking the mutex. Callers hold it.
func (ps *PlaylistStore) getLocked(ctx context.Context, ownerID, id int64) (Playlist, error) {
	var p Playlist
	var created, updated int64
	err := ps.db.QueryRowContext(ctx,
		`SELECT p.id, p.owner_id, p.name, p.description, p.created_at, p.updated_at,
		        (SELECT COUNT(*) FROM playlist_items WHERE playlist_id = p.id),
		        (SELECT COALESCE(SUM(duration), 0) FROM playlist_items WHERE playlist_id = p.id)
		 FROM playlists p WHERE p.id = ? AND p.owner_id = ?`, id, ownerID).
		Scan(&p.ID, &p.OwnerID, &p.Name, &p.Description, &created, &updated,
			&p.ItemCount, &p.TotalDuration)
	if errors.Is(err, sql.ErrNoRows) {
		return Playlist{}, ErrPlaylistNotFound
	}
	if err != nil {
		return Playlist{}, fmt.Errorf("get playlist: %w", err)
	}
	p.CreatedAt = time.Unix(created, 0)
	p.UpdatedAt = time.Unix(updated, 0)
	return p, nil
}

// isUniqueViolation recognizes modernc.org/sqlite's constraint error without
// importing its driver-specific error type into every call site.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}
