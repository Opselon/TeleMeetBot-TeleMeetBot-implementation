package logx

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRecorderRingOrder(t *testing.T) {
	r := NewRecorder(4)
	for i := 0; i < 6; i++ {
		r.Record(Entry{Level: LevelInfo, Message: strings.Repeat("x", 0) + string(rune('a'+i))})
	}
	got := r.Recent(0)
	if len(got) != 4 {
		t.Fatalf("want 4 retained entries, got %d", len(got))
	}
	want := "cdef"
	for i, e := range got {
		if e.Message != string(want[i]) {
			t.Fatalf("entry %d = %q, want %q (oldest-first order broken)", i, e.Message, string(want[i]))
		}
	}
}

func TestSplitComponent(t *testing.T) {
	for _, tc := range []struct {
		in, comp, rest string
	}{
		{"[PIPELINE:download] abc: ready", "download", "abc: ready"},
		{"[PIPELINE:bot] edit card failed: x", "bot", "edit card failed: x"},
		{"plain line", "", "plain line"},
		{"[PIPELINE:unterminated", "", "[PIPELINE:unterminated"},
	} {
		comp, rest := SplitComponent(tc.in)
		if comp != tc.comp || rest != tc.rest {
			t.Errorf("SplitComponent(%q) = (%q,%q), want (%q,%q)", tc.in, comp, rest, tc.comp, tc.rest)
		}
	}
}

func TestTeeHandlerCapturesAndForwards(t *testing.T) {
	rec := NewRecorder(16)
	inner := slog.NewTextHandler(&strings.Builder{}, nil)
	h := NewTeeHandler(inner, rec)
	l := slog.New(h)
	l.Info("[PIPELINE:download] hello")
	l.Error("[PIPELINE:bot] boom")
	entries := rec.Recent(0)
	if len(entries) != 2 {
		t.Fatalf("want 2 captured, got %d", len(entries))
	}
	if entries[0].Component != "download" || entries[0].Message != "hello" {
		t.Errorf("entry0 = %+v", entries[0])
	}
	if entries[1].Level != LevelError || entries[1].Component != "bot" {
		t.Errorf("entry1 = %+v", entries[1])
	}
	st := rec.Stats()
	if st.Errors != 1 || st.Warnings != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestSubscribeDropOnSlowConsumer(t *testing.T) {
	r := NewRecorder(8)
	ch, cancel := r.Subscribe(1)
	cancel()
	// After cancel the channel is closed; publishing must not block or panic.
	r.Record(Entry{Message: "x"})
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel should be closed after cancel")
		}
	default:
	}
}

func TestSinceAndFiltered(t *testing.T) {
	base := time.Now()
	r := NewRecorder(8)
	old := Entry{Time: base.Add(-time.Hour), Level: LevelWarn, Message: "old"}
	r.Record(old)
	r.Record(Entry{Time: base, Level: LevelError, Message: "new"})
	if got := r.Since(base); len(got) != 1 || got[0].Message != "new" {
		t.Fatalf("Since = %+v", got)
	}
	if got := r.RecentFiltered(10, LevelWarn); len(got) != 2 {
		t.Fatalf("filtered = %+v", got)
	}
	if got := r.RecentFiltered(10, LevelError); len(got) != 1 || got[0].Message != "new" {
		t.Fatalf("filtered err = %+v", got)
	}
}

func TestCatchPanicDigest(t *testing.T) {
	r := NewRecorder(4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		CatchPanicFunc(r, "test", "unit", func() { panic("kaboom") })
	}()
	<-done
	entries := r.RecentFiltered(1, LevelError)
	if len(entries) != 1 {
		t.Fatalf("want 1 error entry, got %+v", entries)
	}
	if !strings.Contains(entries[0].Message, "kaboom") || !strings.Contains(entries[0].Message, "panic in unit") {
		t.Fatalf("digest = %q", entries[0].Message)
	}
}

func TestRedaction(t *testing.T) {
	SetRedactors("123456:AAHsupersecrettoken", "short")
	defer func() { redactors.Store([]string(nil)) }()
	r := NewRecorder(8)
	r.Record(Entry{Level: LevelWarn, Message: "auth failed for bot 123456:AAHsupersecrettoken at line short-x"})
	entries := r.Recent(0)
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	msg := entries[0].Message
	if strings.Contains(msg, "AAHsupersecrettoken") {
		t.Fatalf("secret leaked: %q", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Fatalf("redaction marker missing: %q", msg)
	}
	// The short value (len < 8) must NOT be registered as a redactor —
	// otherwise every message containing the word "short" would be mangled.
	if !strings.Contains(msg, "short-x") {
		t.Fatalf("short redactor must be ignored: %q", msg)
	}
}
