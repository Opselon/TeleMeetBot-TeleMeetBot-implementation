package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUpdateCoalescesNoOpWrites is the regression for the state-file churn seen
// during a host-gated join: progress events repeat the current state every few
// hundred milliseconds, and each one used to marshal + rewrite the whole history
// file. A no-op update must not touch the disk.
func TestUpdateCoalescesNoOpWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st := Load(path)
	st.Add(Record{ID: "s-1", MeetURL: "https://meet.google.com/abc-defg-hij", State: "starting"})
	if st.writes != 1 {
		t.Fatalf("Add should write once, got %d", st.writes)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}

	// 50 progress updates with the same state: no disk writes.
	for i := 0; i < 50; i++ {
		st.Update("s-1", "starting", "")
	}
	if st.writes != 1 {
		t.Fatalf("no-op updates rewrote the file %d times (want 0 more)", st.writes-1)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("state file changed after no-op updates")
	}

	// A real state change still persists.
	st.Update("s-1", "streaming", "")
	if st.writes != 2 {
		t.Fatalf("state change should write, got %d writes", st.writes)
	}

	// Terminal states persist exactly once (EndedAt is stamped on the first).
	st.Update("s-1", "ended", "")
	if st.writes != 3 {
		t.Fatalf("terminal update should write, got %d writes", st.writes)
	}
	st.Update("s-1", "ended", "")
	if st.writes != 3 {
		t.Fatalf("repeated terminal update rewrote the file (%d writes)", st.writes)
	}
	recs := st.Records()
	if len(recs) != 1 || recs[0].EndedAt.IsZero() {
		t.Fatalf("terminal record not finalised: %+v", recs)
	}

	// A different error is a real change.
	st.Update("s-1", "ended", "boom")
	if st.writes != 4 {
		t.Fatalf("new error message should write, got %d writes", st.writes)
	}
}
