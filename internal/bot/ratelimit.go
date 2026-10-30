package bot

import (
	"sync"
	"time"
)

// Per-user rate limiting.
//
// Every Telegram update costs work: a callback can start a session, edit a
// card or enqueue media. A stuck client (or a curious admin mashing a
// button) would otherwise multiply the load — goroutine per update, card
// edit storms, 429s from Telegram. The limiter is a token bucket per user:
// sustained rate with a small burst, tuned so normal dashboard use never
// trips it (the card refresh loop does not go through this path — only
// user-initiated updates do).

const (
	// userRatePerSecond is the sustained user action rate.
	userRatePerSecond = 4.0
	// userRateBurst is the bucket size (how many actions can land at once).
	userRateBurst = 12.0
	// userRateNoticeEvery bounds the "slow down" warnings per user so a
	// flooder cannot turn the limiter into a log flood.
	userRateNoticeEvery = 10 * time.Second
)

// userBucket is one user's token bucket.
type userBucket struct {
	tokens float64
	last   time.Time
	// lastNotice throttles the warning log.
	lastNotice time.Time
}

// rateLimiter tracks one bucket per Telegram user id.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[int64]*userBucket
	perSec  float64
	burst   float64
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		buckets: map[int64]*userBucket{},
		perSec:  userRatePerSecond,
		burst:   userRateBurst,
	}
}

// allow reports whether userID may act now. It lazily creates the bucket,
// refills by elapsed time, and consumes one token on success.
func (r *rateLimiter) allow(userID int64, now time.Time) bool {
	if userID == 0 {
		return true // system-originated (no From) updates are not throttled
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[userID]
	if !ok {
		b = &userBucket{tokens: r.burst, last: now}
		r.buckets[userID] = b
	}
	// Refill.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * r.perSec
		if b.tokens > r.burst {
			b.tokens = r.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// shouldNotice reports (and records) whether a rate-limited user deserves a
// warning right now — at most one per notice window.
func (r *rateLimiter) shouldNotice(userID int64, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[userID]
	if !ok {
		return false
	}
	if now.Sub(b.lastNotice) >= userRateNoticeEvery {
		b.lastNotice = now
		return true
	}
	return false
}

// gc prunes buckets idle for longer than ttl so the map cannot grow forever
// with one-off users.
func (r *rateLimiter) gc(now time.Time, ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, b := range r.buckets {
		if now.Sub(b.last) > ttl {
			delete(r.buckets, id)
		}
	}
}
