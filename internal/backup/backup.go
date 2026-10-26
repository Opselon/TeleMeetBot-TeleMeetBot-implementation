// Package backup creates and lists consistent, self-describing backups of the
// bot's small state files and SQLite databases.
//
// A backup is a timestamped directory under a root (normally
// <data_dir>/backups) containing:
//
//	config.yml       copied verbatim
//	state.json       copied verbatim
//	meetings.db      produced with SQLite's VACUUM INTO
//	playlists.db     produced with SQLite's VACUUM INTO
//	manifest.json    names, sizes and the creation time
//
// VACUUM INTO is used for databases because a plain file copy of a live WAL
// database can miss committed pages sitting in the -wal file. VACUUM INTO
// writes a self-contained copy from a read transaction, so the backup is
// consistent even while the bot is serving traffic.
package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// manifestVersion lets a future release change the snapshot layout while
// still recognizing old directories.
const manifestVersion = 1

// Source describes one file to include.
type Source struct {
	// Name is the file name inside the snapshot. It must not contain a path
	// separator. Defaults to filepath.Base(Path).
	Name string
	// Path is the live file to back up.
	Path string
	// SQLite marks a database that must be copied with VACUUM INTO.
	SQLite bool
}

// Snapshot describes one completed backup directory.
type Snapshot struct {
	Dir       string    `json:"dir"`
	CreatedAt time.Time `json:"created_at"`
	Files     []string  `json:"files"`
	Bytes     int64     `json:"bytes"`
}

// manifest is the on-disk form of a snapshot.
type manifest struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Files     []string  `json:"files"`
	Bytes     int64     `json:"bytes"`
}

// Create writes a new snapshot under root. Missing source files are skipped:
// a fresh installation has no state.json or databases yet, and a backup of
// the config alone is still useful. At least one file must be copied,
// otherwise the empty directory is removed and an error is returned.
func Create(root string, sources []Source) (Snapshot, error) {
	if strings.TrimSpace(root) == "" {
		return Snapshot{}, errors.New("backup root is empty")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("create backup root: %w", err)
	}
	dir, err := uniqueDir(root, time.Now())
	if err != nil {
		return Snapshot{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("create backup dir: %w", err)
	}
	snap := Snapshot{Dir: dir, CreatedAt: time.Now()}
	cleanup := func() { _ = os.RemoveAll(dir) }

	used := make(map[string]bool, len(sources))
	for _, src := range sources {
		path := strings.TrimSpace(src.Path)
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			cleanup()
			return Snapshot{}, fmt.Errorf("stat %s: %w", path, err)
		}
		if info.IsDir() {
			continue
		}
		name := strings.TrimSpace(src.Name)
		if name == "" {
			name = filepath.Base(path)
		}
		name = filepath.Base(name)
		if name == "." || name == string(filepath.Separator) || name == "" {
			cleanup()
			return Snapshot{}, fmt.Errorf("invalid backup name for %s", path)
		}
		name = uniqueName(name, used)
		dest := filepath.Join(dir, name)
		if src.SQLite {
			if err := vacuumInto(path, dest); err != nil {
				cleanup()
				return Snapshot{}, fmt.Errorf("backup sqlite %s: %w", path, err)
			}
		} else if err := copyFile(path, dest); err != nil {
			cleanup()
			return Snapshot{}, fmt.Errorf("backup %s: %w", path, err)
		}
		st, err := os.Stat(dest)
		if err != nil {
			cleanup()
			return Snapshot{}, fmt.Errorf("stat backup %s: %w", dest, err)
		}
		snap.Files = append(snap.Files, name)
		snap.Bytes += st.Size()
	}
	if len(snap.Files) == 0 {
		cleanup()
		return Snapshot{}, errors.New("nothing to back up: no source files exist yet")
	}
	m := manifest{Version: manifestVersion, CreatedAt: snap.CreatedAt, Files: snap.Files, Bytes: snap.Bytes}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		cleanup()
		return Snapshot{}, fmt.Errorf("encode manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600); err != nil {
		cleanup()
		return Snapshot{}, fmt.Errorf("write manifest: %w", err)
	}
	return snap, nil
}

// List returns snapshots under root, newest first. Directories without a
// readable manifest are ignored: they are either unrelated or incomplete.
func List(root string) ([]Snapshot, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read backup root: %w", err)
	}
	var out []Snapshot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			continue
		}
		var m manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		snap := Snapshot{Dir: dir, CreatedAt: m.CreatedAt, Files: m.Files, Bytes: m.Bytes}
		if snap.CreatedAt.IsZero() {
			if info, err := e.Info(); err == nil {
				snap.CreatedAt = info.ModTime()
			}
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Latest returns the newest snapshot, if any.
func Latest(root string) (Snapshot, bool, error) {
	list, err := List(root)
	if err != nil || len(list) == 0 {
		return Snapshot{}, false, err
	}
	return list[0], true, nil
}

// Prune keeps the newest keep snapshots and deletes older ones. Non-snapshot
// entries are never touched. It returns how many directories were removed.
func Prune(root string, keep int) (int, error) {
	if keep < 1 {
		return 0, errors.New("keep must be at least 1")
	}
	list, err := List(root)
	if err != nil {
		return 0, err
	}
	if len(list) <= keep {
		return 0, nil
	}
	removed := 0
	for _, snap := range list[keep:] {
		if err := os.RemoveAll(snap.Dir); err != nil {
			return removed, fmt.Errorf("remove %s: %w", snap.Dir, err)
		}
		removed++
	}
	return removed, nil
}

// uniqueDir picks a timestamped directory, adding a numeric suffix when two
// backups happen inside the same second.
func uniqueDir(root string, now time.Time) (string, error) {
	base := now.Format("20060102-150405")
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		dir := filepath.Join(root, name)
		_, err := os.Stat(dir)
		if errors.Is(err, os.ErrNotExist) {
			return dir, nil
		}
		if err != nil {
			return "", fmt.Errorf("check backup dir: %w", err)
		}
	}
	return "", errors.New("could not allocate a unique backup directory")
}

// uniqueName avoids overwriting when two sources share a base name.
func uniqueName(name string, used map[string]bool) string {
	if !used[name] {
		used[name] = true
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

// copyFile copies src to dst with private permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// vacuumInto writes a consistent copy of a SQLite database. The source is
// opened read-only-ish (normal mode with a busy timeout, because VACUUM INTO
// needs a read transaction and may need to checkpoint).
func vacuumInto(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("destination already exists: %s", dst)
	}
	db, err := sql.Open("sqlite", src+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	// VACUUM INTO does not accept a bound parameter in every SQLite build;
	// quote the destination as a SQL string literal instead.
	quoted := "'" + strings.ReplaceAll(dst, "'", "''") + "'"
	if _, err := db.ExecContext(ctx, "VACUUM INTO "+quoted); err != nil {
		return err
	}
	return nil
}
