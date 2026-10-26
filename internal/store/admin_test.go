package store

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	return Load(path)
}

func TestAdminStatsAndPreferences(t *testing.T) {
	st := newTestStore(t)

	// Set preferences
	st.SetUserAudioLang(1001, "es")
	st.SetUserMediaMode(1001, "download")
	st.SetUserAudioLang(1002, "fa")

	// Add records
	st.Add(Record{
		ID:        "sess-1",
		ChatID:    1001,
		State:     "streaming",
		CreatedAt: time.Now().Add(-10 * time.Minute),
	})
	st.Add(Record{
		ID:        "sess-2",
		ChatID:    1003,
		State:     "stopped",
		CreatedAt: time.Now().Add(-5 * time.Minute),
	})

	stats := st.Stats()
	if stats.Records != 2 {
		t.Fatalf("expected 2 records, got %d", stats.Records)
	}
	if stats.Active != 1 {
		t.Fatalf("expected 1 active, got %d", stats.Active)
	}
	if stats.Preferences != 2 {
		t.Fatalf("expected 2 preferences, got %d", stats.Preferences)
	}
	if stats.Users < 3 {
		t.Fatalf("expected at least 3 users, got %d", stats.Users)
	}

	// KnownUserIDs
	known := st.KnownUserIDs(1004)
	found1004 := false
	for _, id := range known {
		if id == 1004 {
			found1004 = true
			break
		}
	}
	if !found1004 {
		t.Fatal("expected 1004 in known user IDs")
	}

	// HistoryForUser
	hist := st.HistoryForUser(1001, 10)
	if len(hist) != 1 || hist[0].ID != "sess-1" {
		t.Fatalf("unexpected history: %+v", hist)
	}

	// ResetUser
	if !st.ResetUser(1001) {
		t.Fatal("expected ResetUser(1001) to succeed")
	}
	if _, ok := st.UserPreference(1001); ok {
		t.Fatal("expected user 1001 to have no preferences after reset")
	}

	// ClearHistory
	cleared := st.ClearHistory()
	if cleared != 2 {
		t.Fatalf("expected 2 cleared records, got %d", cleared)
	}
}
