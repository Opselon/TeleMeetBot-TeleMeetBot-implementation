package logx

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
)

// TagPrefix / TagSuffix delimit the "[PIPELINE:component]" family of
// prefixes already used across the codebase. Parsing them here means every
// existing log line gains a filterable Component with zero call-site churn.
const (
	TagPrefix = "[PIPELINE:"
	TagSuffix = "]"
)

// SplitComponent separates a leading "[PIPELINE:component]" tag from the
// rest of the message. The tag is returned bare ("download", "bot", ...).
// Un-tagged messages come back with an empty component.
func SplitComponent(msg string) (component, rest string) {
	if !strings.HasPrefix(msg, TagPrefix) {
		return "", msg
	}
	end := strings.Index(msg, TagSuffix)
	if end < 0 {
		return "", msg
	}
	component = strings.TrimSpace(msg[len(TagPrefix):end])
	rest = strings.TrimSpace(msg[end+len(TagSuffix):])
	return component, rest
}

// TeeHandler wraps another slog.Handler and mirrors every record it emits
// into a Recorder. It is the single seam where "better log catching" plugs
// into the application: main builds the real handler (stdout/file) and wraps
// it once; nothing else changes.
type TeeHandler struct {
	next   slog.Handler
	rec    *Recorder
	attrs  []slog.Attr
	groups []string
}

// NewTeeHandler wraps next so records also land in rec. next may be nil (a
// capture-only handler, used by tests).
func NewTeeHandler(next slog.Handler, rec *Recorder) *TeeHandler {
	return &TeeHandler{next: next, rec: rec}
}

// Enabled delegates to the wrapped handler; when there is none, everything
// the recorder could receive is enabled.
func (h *TeeHandler) Enabled(_ context.Context, l slog.Level) bool {
	if h.next != nil {
		return h.next.Enabled(context.Background(), l)
	}
	return true
}

// Handle tees the record into the recorder, then forwards it. The tee is
// best-effort and can never fail the real log write: a nil recorder is
// tolerated (handler degenerates to a pass-through).
func (h *TeeHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.rec != nil {
		lvl := LevelInfo
		switch {
		case r.Level >= slog.LevelError:
			lvl = LevelError
		case r.Level >= slog.LevelWarn:
			lvl = LevelWarn
		case r.Level < slog.LevelInfo:
			lvl = LevelDebug
		}
		component, rest := SplitComponent(r.Message)
		h.rec.Record(Entry{
			Time:      r.Time,
			Level:     lvl,
			Component: component,
			Message:   rest,
		})
	}
	if h.next != nil {
		return h.next.Handle(ctx, r)
	}
	return nil
}

// WithAttrs returns a copy carrying the attributes (kept for the wrapped
// handler; the recorder only captures messages).
func (h *TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var next slog.Handler
	if h.next != nil {
		next = h.next.WithAttrs(attrs)
	}
	return &TeeHandler{next: next, rec: h.rec, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...), groups: h.groups}
}

// WithGroup returns a copy carrying the group name.
func (h *TeeHandler) WithGroup(name string) slog.Handler {
	var next slog.Handler
	if h.next != nil {
		next = h.next.WithGroup(name)
	}
	return &TeeHandler{next: next, rec: h.rec, attrs: h.attrs, groups: append(append([]string{}, h.groups...), name)}
}

// sourceSeq generates unique panic-catch ids without a lock.
var sourceSeq atomic.Uint64

// CatchPanic recovers a panic into the recorder as an error entry with a
// stack digest. It returns the recovered value (nil when there was no
// panic) so callers that must re-panic can. The digest keeps the first line
// of each frame — enough to locate the crash in a support dump, small
// enough not to flood the ring.
func CatchPanic(rec *Recorder, component, where string, r interface{}) interface{} {
	if r == nil {
		return nil
	}
	msg := panicDigest(where, r)
	if rec != nil {
		rec.Record(Entry{
			Time:      now(),
			Level:     LevelError,
			Component: component,
			Message:   msg,
		})
	}
	return r
}

// CatchPanicFunc runs fn, catching and recording any panic. The existing
// call sites recover panics per-loop ("a frozen dashboard reads as an app
// crash"); this helper gives them the same behaviour with a structured,
// searchable log entry instead of a bare fmt of the recovered value.
func CatchPanicFunc(rec *Recorder, component, where string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			_ = CatchPanic(rec, component, where, r)
		}
	}()
	fn()
}
