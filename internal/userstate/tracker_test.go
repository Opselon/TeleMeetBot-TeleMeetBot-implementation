package userstate

import (
	"testing"
	"time"
)

func TestSetTransitionAndIdentity(t *testing.T) {
	var transitions int
	tr := New(func(from, to Info) { transitions++ })
	tr.Set(1, StateWizardMeet, "", "alice", "Alice")
	if got := tr.Get(1); got.State != StateWizardMeet || got.Username != "alice" {
		t.Fatalf("Get = %+v", got)
	}
	// Same state: Since preserved, transitions not re-fired.
	first := tr.Get(1)
	tr.Set(1, StateWizardMeet, "edited", "", "")
	if got := tr.Get(1); !got.Since.Equal(first.Since) || got.Detail != "edited" {
		t.Fatalf("same-state Set must keep Since: %+v vs %+v", got, first)
	}
	// Detail-only update: no transition.
	if transitions != 1 {
		t.Fatalf("transitions = %d, want 1", transitions)
	}
	tr.Set(1, StateIdle, "", "", "")
	if transitions != 2 {
		t.Fatalf("transitions = %d, want 2", transitions)
	}
	if tr.Len() != 0 {
		t.Fatalf("idle user must be untracked, len = %d", tr.Len())
	}
}

func TestListOrderingOldestFirst(t *testing.T) {
	tr := New(nil)
	tr.Set(1, StateWizardMedia, "", "", "")
	time.Sleep(2 * time.Millisecond)
	tr.Set(2, StateLive, "ses-1", "", "")
	list := tr.List()
	if len(list) != 2 || list[0].UserID != 1 || list[1].UserID != 2 {
		t.Fatalf("List = %+v", list)
	}
	c := tr.Count()
	if c[StateWizardMedia] != 1 || c[StateLive] != 1 {
		t.Fatalf("Count = %+v", c)
	}
}

func TestPrune(t *testing.T) {
	tr := New(nil)
	tr.now = func() time.Time { return time.Unix(1000, 0) }
	tr.Set(1, StateLive, "", "", "")
	base := time.Unix(1000, 0)
	tr.now = func() time.Time { return base.Add(time.Hour) }
	if n := tr.Prune(30 * time.Minute); n != 1 {
		t.Fatalf("Prune = %d, want 1", n)
	}
	if tr.Len() != 0 {
		t.Fatalf("len after prune = %d", tr.Len())
	}
}

func TestZeroUserIgnored(t *testing.T) {
	tr := New(nil)
	tr.Set(0, StateLive, "", "", "")
	if tr.Len() != 0 {
		t.Fatalf("zero id must be ignored")
	}
}
