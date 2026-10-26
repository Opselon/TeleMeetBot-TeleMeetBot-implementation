package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestBackupCreateListPrune(t *testing.T) {
	tempDir := t.TempDir()
	backupRoot := filepath.Join(tempDir, "backups")

	// Create dummy plain file
	cfgPath := filepath.Join(tempDir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("telegram:\n  token: test\n"), 0o600); err != nil {
		t.Fatalf("WriteFile cfg: %v", err)
	}

	// Create dummy SQLite file
	dbPath := filepath.Join(tempDir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE sample (id INTEGER PRIMARY KEY, val TEXT); INSERT INTO sample (val) VALUES ('hello');"); err != nil {
		_ = db.Close()
		t.Fatalf("db.Exec: %v", err)
	}
	_ = db.Close()

	sources := []Source{
		{Name: "config.yml", Path: cfgPath, SQLite: false},
		{Name: "test.db", Path: dbPath, SQLite: true},
		{Name: "missing.txt", Path: filepath.Join(tempDir, "missing.txt"), SQLite: false}, // Should be ignored gracefully
	}

	// Create first backup
	snap1, err := Create(backupRoot, sources)
	if err != nil {
		t.Fatalf("Create snap1: %v", err)
	}
	if len(snap1.Files) != 2 {
		t.Fatalf("expected 2 files in snap1, got %d", len(snap1.Files))
	}
	if snap1.Bytes <= 0 {
		t.Fatalf("expected snap1.Bytes > 0, got %d", snap1.Bytes)
	}

	// Verify the backed up SQLite file is valid and readable
	backupDBPath := filepath.Join(snap1.Dir, "test.db")
	backedDB, err := sql.Open("sqlite", backupDBPath)
	if err != nil {
		t.Fatalf("open backed up DB: %v", err)
	}
	var val string
	if err := backedDB.QueryRowContext(context.Background(), "SELECT val FROM sample WHERE id = 1").Scan(&val); err != nil || val != "hello" {
		_ = backedDB.Close()
		t.Fatalf("query backed up DB: val=%q err=%v", val, err)
	}
	_ = backedDB.Close()

	// Sleep slightly to guarantee different timestamp directory
	time.Sleep(10 * time.Millisecond)

	// Create second backup
	snap2, err := Create(backupRoot, sources)
	if err != nil {
		t.Fatalf("Create snap2: %v", err)
	}

	// List
	list, err := List(backupRoot)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(list))
	}

	// Latest
	latest, ok, err := Latest(backupRoot)
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if latest.Dir != snap2.Dir {
		t.Fatalf("expected latest to be snap2 (%s), got %s", snap2.Dir, latest.Dir)
	}

	// Prune (keep 1)
	pruned, err := Prune(backupRoot, 1)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 pruned, got %d", pruned)
	}

	listAfterPrune, err := List(backupRoot)
	if err != nil {
		t.Fatalf("List after prune: %v", err)
	}
	if len(listAfterPrune) != 1 || listAfterPrune[0].Dir != snap2.Dir {
		t.Fatalf("expected only snap2 after prune, got %+v", listAfterPrune)
	}
}
