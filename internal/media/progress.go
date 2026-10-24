package media

import "sync"

// Progress is one mid-download notification: bytes fetched so far, the total
// when the server reports it, and Done when the file is committed (or failed).
type Progress struct {
	URL    string
	ID     string
	Bytes  int64
	Total  int64
	Done   bool
	Failed bool
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
