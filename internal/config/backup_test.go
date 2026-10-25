package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestBackupRestoreRoundTrip proves the wizard's rollback pair actually
// round-trips: whatever Backup captured, RestoreBackup puts back. A rollback
// that silently drops keys is worse than no rollback at all.
func TestBackupRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")

	original := []byte("telegram:\n  token: \"111:ABC-original\"\nadmins: [42]\n")
	if err := os.WriteFile(cfgPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	bak, err := Backup(cfgPath)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if bak != cfgPath+".bak" {
		t.Fatalf("backup path = %q, want %q", bak, cfgPath+".bak")
	}

	// Simulate a wizard re-run that changed the config.
	if err := os.WriteFile(cfgPath, []byte("telegram:\n  token: \"222:CHANGED\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBackup(cfgPath); err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("restored config = %q, want the original %q", got, original)
	}
}

// TestBackupOverwritesPrevious proves a second backup does not stack .bak.bak
// files: the rollback target must always be the immediately previous config.
func TestBackupOverwritesPrevious(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(cfgPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cfgPath + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2\n" {
		t.Fatalf(".bak = %q, want the latest v2", got)
	}
}

// TestRestoreBackupMissing proves a missing backup is an error, never a
// silent truncate of the live config: RestoreBackup must not "succeed" by
// writing an empty file over a working install.
func TestRestoreBackupMissing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBackup(cfgPath); err == nil {
		t.Fatal("RestoreBackup on a missing backup must fail")
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "live\n" {
		t.Fatalf("live config was modified: %q", got)
	}
}

// TestBackupEmptyPath proves an empty path fails closed instead of creating
// a mystery .bak next to the process cwd.
func TestBackupEmptyPath(t *testing.T) {
	if _, err := Backup(""); err == nil {
		t.Fatal("Backup(\"\") must fail")
	}
	if err := RestoreBackup(""); err == nil {
		t.Fatal("RestoreBackup(\"\") must fail")
	}
}

// TestBackupPermissions proves the backup is only readable by its owner: it
// carries the bot token, so 0644 in a shared /etc is not acceptable. Windows
// has no portable permission bits, so the check is Unix-only.
func TestBackupPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not portable on Windows")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(cfgPath, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(cfgPath); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(cfgPath + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %o, want 0600 (it carries the bot token)", st.Mode().Perm())
	}
}
