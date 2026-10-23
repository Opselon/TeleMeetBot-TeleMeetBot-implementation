package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAddUpdateCycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := Load(path)
	if len(s.Records()) != 0 {
		t.Fatal("fresh store must be empty")
	}
	s.Add(Record{ID: "s-1", MeetURL: "https://meet.google.com/abc-defg-hij", MediaURL: "https://x/y.mp4", Title: "TeleMeet Media", State: "starting"})
	s.Update("s-1", "streaming", "")
	s.Update("s-1", "ended", "")

	got := s.Records()
	if len(got) != 1 || got[0].State != "ended" {
		t.Fatalf("records = %+v", got)
	}
	if got[0].EndedAt.IsZero() {
		t.Fatal("EndedAt not set on close")
	}

	// Reload persists.
	s2 := Load(path)
	if len(s2.Records()) != 1 || s2.Records()[0].ID != "s-1" {
		t.Fatalf("reload lost records: %+v", s2.Records())
	}
}

func TestInterrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := Load(path)
	s.Add(Record{ID: "a", State: "streaming"})
	s.Add(Record{ID: "b", State: "ended"})
	interrupted := s.Interrupted()
	if len(interrupted) != 1 || interrupted[0].ID != "a" {
		t.Fatalf("interrupted = %+v", interrupted)
	}
	got := s.Records()[0]
	if got.State != "interrupted" || got.EndedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("record = %+v", got)
	}
}

func TestCorruptFileSurvives(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Load(path)
	if n := len(s.Records()); n != 0 {
		t.Fatalf("corrupt load should yield empty store, got %d", n)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "state.json.corrupt-") {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt file was not preserved for forensics")
	}
}

func TestUserPreferencesPersistAndIsolate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st := Load(path)

	// Defaults to "fa" (Persian)
	if got := st.AudioLang(1001); got != "fa" {
		t.Fatalf("expected default audio lang 'fa', got %q", got)
	}

	// Set English for user 1001
	if err := st.SetAudioLang(1001, "en"); err != nil {
		t.Fatalf("set audio lang: %v", err)
	}

	// Set Original for user 1002
	if err := st.SetAudioLang(1002, "orig"); err != nil {
		t.Fatalf("set audio lang: %v", err)
	}

	// User 1003 is unset, must still be default "fa"
	if got := st.AudioLang(1003); got != "fa" {
		t.Fatalf("user 1003 should be 'fa', got %q", got)
	}

	if got := st.AudioLang(1001); got != "en" {
		t.Fatalf("user 1001 should be 'en', got %q", got)
	}
	if got := st.AudioLang(1002); got != "orig" {
		t.Fatalf("user 1002 should be 'orig', got %q", got)
	}

	// Reload from disk and verify persistence across bot restart
	st2 := Load(path)
	if got := st2.AudioLang(1001); got != "en" {
		t.Fatalf("after reload, user 1001 should be 'en', got %q", got)
	}
	if got := st2.AudioLang(1002); got != "orig" {
		t.Fatalf("after reload, user 1002 should be 'orig', got %q", got)
	}
	if got := st2.AudioLang(1003); got != "fa" {
		t.Fatalf("after reload, user 1003 should be 'fa', got %q", got)
	}
}

func TestLegacyArrayStateMigratesCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	legacyJSON := `[{"id":"s-old","meet_url":"https://meet.google.com/xyz","media_url":"https://x","title":"T","state":"ended","created_at":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	st := Load(path)
	recs := st.Records()
	if len(recs) != 1 || recs[0].ID != "s-old" {
		t.Fatalf("legacy records not loaded: %+v", recs)
	}

	// Audio language defaults to Persian
	if got := st.AudioLang(555); got != "fa" {
		t.Fatalf("expected 'fa', got %q", got)
	}

	// Saving a preference migrates file to object format cleanly
	if err := st.SetAudioLang(555, "fa"); err != nil {
		t.Fatalf("set audio lang: %v", err)
	}
	st.Add(Record{ID: "s-new", State: "starting"})

	st2 := Load(path)
	if len(st2.Records()) != 2 {
		t.Fatalf("expected 2 records after migration, got %d", len(st2.Records()))
	}
	if got := st2.AudioLang(555); got != "fa" {
		t.Fatalf("expected 'fa' on st2, got %q", got)
	}
}
