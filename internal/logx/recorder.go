// Package logx is the centralized log-catch layer of TeleMeetBot.
//
// Every log line the application emits is teed into a Recorder: a bounded
// ring buffer that (a) keeps the most recent entries in memory for the web
// dashboard and support dumps, and (b) fans each entry out to live
// subscribers (the dashboard's SSE tail) without ever blocking the caller
// that produced the log line.
//
// The design goals, in order:
//
//   - Never block: a log call on the download or engine hot path must not
//     wait on a slow dashboard connection. Subscribers get a buffered
//     channel; a full subscriber drops entries rather than stalling the app
//     (a dashboard tail that lags a few entries is honest; a stalled
//     download pipeline is not).
//   - Bounded memory: the ring keeps at most N entries (default 2000,
//     roughly a few hundred kilobytes) regardless of how chatty the
//     pipeline gets.
//   - Structure for free: the recorder parses the existing
//     "[PIPELINE:component]" prefixes into a Component field so callers did
//     not have to change a single log statement to become filterable.
package logx

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level mirrors the four severities the app already logs with. It is a
// distinct type (not slog.Level) so this package stays dependency-free and
// usable from packages that log through their own small interfaces.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String is the short lowercase wire form used in JSON and the dashboard.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

// ParseLevel maps a config string ("debug", "info", ...) to a Level. Empty
// and unknown input is Info: the same fail-open default the app logger uses.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// Entry is one captured log line.
type Entry struct {
	Time      time.Time `json:"time"`
	Level     Level     `json:"level"`
	Component string    `json:"component,omitempty"`
	Message   string    `json:"message"`
}

// Component returns e's component with the "PIPELINE:" family prefix kept
// verbatim ("download", "bot", "session", ...); "" when the line carried no
// tag. Callers filter with it.
func (e Entry) ComponentName() string { return e.Component }

// Recorder is the in-memory log capture. It is safe for concurrent use.
type Recorder struct {
	mu      sync.RWMutex
	entries []Entry // ring storage
	next    int     // next write position
	full    bool    // ring has wrapped at least once
	max     int

	// subscriber fan-out. Each subscriber is a buffered channel; the
	// slow-consumer policy is drop-oldest (see publish).
	subMu  sync.RWMutex
	subs   map[int]chan Entry
	subSeq int

	// counters kept for the health surface without scanning the ring
	counts  [4]int
	dropped uint64
}

// NewRecorder builds a ring that keeps up to max entries. max <= 0 selects
// the default of 2000 — enough for a full session start sequence plus a
// download, small enough to ignore on a 512 MB VPS.
func NewRecorder(max int) *Recorder {
	if max <= 0 {
		max = 2000
	}
	return &Recorder{
		entries: make([]Entry, max),
		max:     max,
		subs:    map[int]chan Entry{},
	}
}

// redactors are applied to every recorded message: secret material (bot
// token, OAuth secrets, API hashes) must never land in the ring buffer or on
// a dashboard screen. Set once at startup; never mutated afterwards.
var redactors atomic.Value // []string

// SetRedactors registers literal strings to redact from captured logs.
// Empty/short values are ignored (a 3-char secret would corrupt messages).
// Safe to call before or while recording: readers pick the new list up
// atomically on the next entry.
func SetRedactors(secrets ...string) {
	var keep []string
	for _, s := range secrets {
		if len(strings.TrimSpace(s)) >= 8 {
			keep = append(keep, s)
		}
	}
	if len(keep) == 0 {
		return
	}
	redactors.Store(keep)
}

// redact applies every registered redactor to msg.
func redact(msg string) string {
	v := redactors.Load()
	if v == nil {
		return msg
	}
	for _, secret := range v.([]string) {
		if strings.Contains(msg, secret) {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	return msg
}

// Record appends one entry to the ring and fans it out. It never blocks:
// fan-out runs on the caller's goroutine but every send is non-blocking with
// drop-on-full per subscriber.
func (r *Recorder) Record(e Entry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Message = redact(e.Message)
	r.mu.Lock()
	r.entries[r.next] = e
	r.next = (r.next + 1) % r.max
	if !r.full && r.next == 0 {
		r.full = true
	}
	if int(e.Level) >= 0 && int(e.Level) < len(r.counts) {
		r.counts[e.Level]++
	}
	r.mu.Unlock()
	r.publish(e)
}

// publish delivers the entry to every live subscriber without blocking. A
// subscriber whose buffer is full loses the entry (its consumer is too slow
// — the tail UI copes; the producer must not stall).
func (r *Recorder) publish(e Entry) {
	r.subMu.RLock()
	subs := make([]chan Entry, 0, len(r.subs))
	for _, ch := range r.subs {
		subs = append(subs, ch)
	}
	r.subMu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
			r.mu.Lock()
			r.dropped++
			r.mu.Unlock()
		}
	}
}

// Subscribe returns a channel that receives every subsequent entry plus a
// cancel function. The channel is buffered (256): a dashboard tab parked in
// a background window must not degrade the app. Cancelling drains and closes
// the channel; cancelling twice is safe.
func (r *Recorder) Subscribe(buffer int) (<-chan Entry, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	ch := make(chan Entry, buffer)
	r.subMu.Lock()
	id := r.subSeq
	r.subSeq++
	r.subs[id] = ch
	r.subMu.Unlock()
	cancel := func() {
		r.subMu.Lock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
		r.subMu.Unlock()
	}
	return ch, cancel
}

// Recent returns the newest n entries in chronological order. n <= 0 means
// "everything the ring still holds". The returned slice is a copy: callers
// may retain or sort it freely.
func (r *Recorder) Recent(n int) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := r.lenLocked()
	if n <= 0 || n > all {
		n = all
	}
	out := make([]Entry, 0, n)
	// Walk from the oldest retained entry, taking only the last n.
	start := r.next // oldest when full; 0 when not yet wrapped
	if !r.full {
		start = 0
	}
	skip := all - n
	for i := 0; i < all; i++ {
		idx := (start + i) % r.max
		if i < skip {
			continue
		}
		out = append(out, r.entries[idx])
	}
	return out
}

// Since returns the entries recorded at or after t, oldest first. The web
// dashboard uses it to bootstrap a live tail without missing the gap between
// page load and the first streamed event.
func (r *Recorder) Since(t time.Time) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := r.lenLocked()
	start := r.next
	if !r.full {
		start = 0
	}
	out := make([]Entry, 0, 64)
	for i := 0; i < all; i++ {
		idx := (start + i) % r.max
		if !r.entries[idx].Time.Before(t) {
			out = append(out, r.entries[idx])
		}
	}
	return out
}

// RecentFiltered returns the newest n entries at or above minLevel, in
// chronological order.
func (r *Recorder) RecentFiltered(n int, minLevel Level) []Entry {
	all := r.Recent(0)
	var out []Entry
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Level < minLevel {
			continue
		}
		out = append(out, all[i])
		if n > 0 && len(out) >= n {
			break
		}
	}
	// reverse into chronological order
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Stats is a point-in-time summary for the health surface.
type Stats struct {
	Captured     int    `json:"captured"`       // entries currently retained
	Capacity     int    `json:"capacity"`       // ring size
	Errors       uint64 `json:"errors"`         // lifetime error count
	Warnings     uint64 `json:"warnings"`       // lifetime warn count
	DroppedToSub uint64 `json:"dropped_to_sub"` // entries dropped on slow subscribers
}

// Stats summarizes the recorder without racing the writers.
func (r *Recorder) Stats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return Stats{
		Captured:     r.lenLocked(),
		Capacity:     r.max,
		Errors:       uint64(r.counts[LevelError]),
		Warnings:     uint64(r.counts[LevelWarn]),
		DroppedToSub: r.dropped,
	}
}

func (r *Recorder) lenLocked() int {
	if r.full {
		return r.max
	}
	return r.next
}
