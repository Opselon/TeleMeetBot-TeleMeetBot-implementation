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
