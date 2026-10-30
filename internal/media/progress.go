package media

import "sync"

// Pipeline stages a download moves through, in order. They ride on every
// Progress event so the Telegram card, the engine snapshot and the web
// dashboard can all show WHERE the download is, not only how many bytes
// moved.
const (
	// StageValidate covers URL validation, SSRF resolution and the disk
	// fence — everything before the first byte is requested.
	StageValidate = "validate"
	// StageConnect covers request dispatch up to the first response byte.
	StageConnect = "connect"
	// StageDownload is the body/fragment transfer.
	StageDownload = "download"
	// StageVerify is the post-fetch integrity check (size, sniffs).
	StageVerify = "verify"
	// StageFinalize covers rename/commit, manifest write and subtitles.
	StageFinalize = "finalize"
	// StageDone marks a committed download.
	StageDone = "done"
)

// Progress is one mid-download notification: bytes fetched so far, the total
// when the server reports it, current speed in bytes/sec, and Done when the
// file is committed (or failed).
//
// The richer fields (Stage, Percent, ETA, Kind) were added with the pipeline
// upgrade; Total-based percent is computed by reporters when the total is
// known, so a consumer never re-derives it.
type Progress struct {
	URL    string
	ID     string
	Bytes  int64
	Total  int64
	Speed  int64
	Done   bool
	Failed bool
	// Stage is the pipeline stage the download is in right now (one of the
	// Stage* constants). Empty on legacy events means "downloading".
	Stage string `json:",omitempty"`
	// Percent is 0-100 when Total is known, -1 when unknown.
	Percent float64 `json:",omitempty"`
	// ETA is seconds remaining, -1 when unknown.
	ETA int64 `json:",omitempty"`
	// Kind is the backend: "http" (direct fetch) or "yt-dlp".
	Kind string `json:",omitempty"`
	// Error is a short failure reason when Failed is true.
	Error string `json:",omitempty"`
}

// percentOf computes 0-100 from bytes/total, or -1 when the total is unknown.
func percentOf(bytes, total int64) float64 {
	if total <= 0 || bytes < 0 {
		return -1
	}
	p := float64(bytes) / float64(total) * 100
	if p > 100 {
		p = 100
	}
	return p
}

// etaOf estimates seconds remaining from speed, or -1 when it cannot be
// known (no total, no speed, or speed so low it would round to forever).
func etaOf(bytes, total, speed int64) int64 {
	if total <= 0 || speed <= 0 || bytes >= total {
		return -1
	}
	return (total - bytes) / speed
}

// progressBus fans progress events out to every subscriber. It is a fan-out
// only: the download path never blocks on a slow subscriber.
type progressBus struct {
	mu          sync.RWMutex
	subscribers map[int]func(Progress)
	next        int
}

func newProgressBus() *progressBus { return &progressBus{subscribers: map[int]func(Progress){}} }

// subscribe registers a listener and returns an unsubscribe function.
func (b *progressBus) subscribe(fn func(Progress)) func() {
	b.mu.Lock()
	id := b.next
	b.next++
	b.subscribers[id] = fn
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.subscribers, id)
		b.mu.Unlock()
	}
}

// notify publishes an event to every subscriber without blocking the fetch.
func (b *progressBus) notify(p Progress) {
	b.mu.RLock()
	subs := make([]func(Progress), 0, len(b.subscribers))
	for _, fn := range b.subscribers {
		subs = append(subs, fn)
	}
	b.mu.RUnlock()
	for _, fn := range subs {
		fn(p)
	}
}

// ---- loopback allow-list (tests and local fixtures) ------------------------

// allowLoopback, when true, permits 127.0.0.1/::1 destinations. Production
// leaves it false: the SSRF gate must be fail-closed, and no Telegram-supplied
// URL may ever point at the bot's own host. Tests set it so an httptest
// fixture server can stand in for a remote media host.
var allowLoopback bool

// loopbackAllowed reports whether loopback destinations are currently allowed.
func loopbackAllowed() bool { return allowLoopback }

// SetAllowLoopbackForTest toggles the loopback exemption. It exists so tests
// OUTSIDE the media package (engine integration tests) can run against a
// local fixture server. Production must never call it: a Telegram-supplied
// URL pointing at the bot's own host must always be refused.
func SetAllowLoopbackForTest(allowed bool) { allowLoopback = allowed }
