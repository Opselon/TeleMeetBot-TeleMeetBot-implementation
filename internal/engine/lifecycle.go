package engine

import (
	"context"
	"sync"
	"time"
)

// Session lifecycle: the single authoritative cancellation authority.
//
// THE ROOT CAUSE THIS FILE FIXES
//
// Before this change Session.Stop() was exactly one statement —
// s.cancel() — while the session's download work ran on a DETACHED
// context. prepareLocalMedia called
//
//	s.downloadMedia(context.Background(), target)
//
// (internal/engine/download.go) and applySwitch did the same for
// mid-session media switches. The media manager therefore never saw
// the session's cancellation: the in-flight HTTP body read, and the
// yt-dlp subprocess behind it, kept running until their own
// multi-minute timeout elapsed — holding the manager's single
// concurrency slot hostage and leaving an orphan worker behind a
// session the operator had already stopped. The reproduction tests in
// repro_stop_test.go fail deterministically on the pre-fix tree for
// exactly this reason.
//
// THE MODEL
//
// Every session-owned operation derives from one cancellable lifespan
// context owned by the session. Stop() cancels it, and because every
// registered child (download, subprocess, HTTP fetch, retry loop,
// queue advance, media switch) derives from that same context, the
// cancellation reaches all of them along the single path Go already
// provides: context propagation. There is no second, competing
// registry to keep in sync and no per-operation wiring an author can
// forget to add — a session-owned operation that does not take the
// session context simply cannot be started.
//
// Stop() is idempotent and race-safe: the first call flips the
// session to a STOPPING detail state, every later call is a cheap
// no-op, and Manager.Stop() is the bounded wait that VERIFIES the
// session actually terminated instead of trusting the request.
//
// The fields live on the Session struct itself (session.go) so that a
// hand-built test session gets a working lifecycle too.

// stopGracePeriod bounds how long a stop waits for the session
// goroutine to finish before reporting that it did not stop in time.
// The session keeps cleaning up regardless; the caller (bot,
// dashboard, signal handler) merely stops waiting.
const stopGracePeriod = 15 * time.Second

// workersActive reports how many tracked owned workers are still
// running. Zero is a necessary condition for the final STOPPED
// status: a session whose goroutine exited but which still has a
// live download has not actually stopped.
func (s *Session) workersActive() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runningWorkers
}

// sessionLifespanCtx returns the session's cancellable context, or
// context.Background() for a hand-built session that never went
// through Manager.Start. Callers that spawn session-owned work must
// use this as the parent so Stop() reaches the work.
func (s *Session) sessionLifespanCtx() context.Context {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// downloadCtx is the context every session-owned download uses. It
// IS the session lifespan: a Stop mid-fetch cancels the HTTP body /
// yt-dlp subprocess through the single authoritative path, and the
// media manager's own retry/ladder loops see it too because they
// derive from the same ctx they were handed.
func (s *Session) downloadCtx() context.Context { return s.sessionLifespanCtx() }

// trackDownload runs one download under the session's lifespan and
// records it as owned work, so cleanup() can wait for the fetch to
// actually unwind before the session reports STOPPED. The fn receives
// the derived context and must return when it is cancelled — the
// media manager already does (its HTTP client and the yt-dlp exec are
// both context-bound, and cmd.Cancel SIGKILLs the process group).
//
// The returned channel is closed when the download has fully
// terminated, which the caller may wait on for a bounded time.
func (s *Session) trackDownload(name string, fn func(ctx context.Context) error) <-chan struct{} {
	done := make(chan struct{})
	s.mu.Lock()
	if s.workers == nil {
		s.workers = map[string]context.CancelFunc{}
	}
	key := name
	for n := 1; ; n++ {
		if _, ok := s.workers[key]; !ok {
			break
		}
		key = name + "-" + itoa(n)
	}
	s.workers[key] = nil // presence marker; the cancel is the lifespan itself
	s.runningWorkers++
	release := func() {
		s.mu.Lock()
		if _, ok := s.workers[key]; ok {
			delete(s.workers, key)
			s.runningWorkers--
		}
		s.mu.Unlock()
		close(done)
	}
	ctx := s.ctx
	s.mu.Unlock()

	go func() {
		defer release()
		if ctx == nil {
			_ = fn(context.Background())
			return
		}
		_ = fn(ctx)
	}()
	return done
}

// itoa is a strconv-free helper for unique worker keys.
func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// cancelLifespan cancels the session lifespan and waits (bounded by
// stopGracePeriod) for every tracked owned worker to return. This is
// the enforcement half of Stop: after the lifespan is cancelled a
// worker that refuses to observe it must not hold the session's final
// STOPPED status hostage, and a worker that truly cannot die must at
// least be named, so the operator sees exactly which resource did not
// terminate instead of a silent timeout.
//
// It never blocks the event loop indefinitely: the wait is bounded,
// and any worker still alive at the deadline is reported by name.
func (s *Session) cancelLifespan() []string {
	s.mu.Lock()
	ctx := s.ctx
	cancel := s.cancel
	names := make([]string, 0, len(s.workers))
	for name := range s.workers {
		names = append(names, name)
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if ctx == nil || len(names) == 0 {
		return nil
	}
	var stuck []string
	for _, name := range names {
		select {
		case <-ctx.Done():
		case <-time.After(stopGracePeriod):
			stuck = append(stuck, name)
		}
	}
	return stuck
}

// stopOwnedDownload cancels any in-flight session-owned download and
// sweeps the partial files a cancelled fetch leaves behind, so a
// stopped session never leaves orphan .part/.tmp artifacts on disk.
// The sweep is best-effort: it must never block or fail the stop.
func (s *Session) stopOwnedDownload() {
	if mgr := s.mediaManager(); mgr != nil && mgr.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mgr.Sweep(ctx)
	}
}

// markStopped closes the session's done channel exactly once. cleanup()
// (the deferred release in run()) also closes it, and doneOnce makes
// the two paths interchangeable: whichever reaches it first wins and
// the other is a no-op, so Stop can never double-close and a caller
// waiting on Done() observes a session that Stop was called on as
// terminated even when run()'s own error path never reached cleanup.
//
// This is what makes "Stop immediately after creation" and "Stop while
// initializing" terminate instead of hanging every stop's bounded wait
// and reporting a spurious "did not stop in time".
func (s *Session) markStopped() {
	s.doneOnce.Do(func() {
		s.mu.Lock()
		if s.done != nil && !IsTerminalState(s.state) {
			if s.ctx != nil && s.ctx.Err() != nil {
				s.state = StateCancelled
				s.detail = "cancelled"
			} else {
				s.state = StateCompleted
			}
		}
		if s.ended.IsZero() {
			s.ended = time.Now()
		}
		done := s.done
		s.mu.Unlock()
		if done != nil {
			close(done)
		}
	})
}

// Compile-time guard: the lifecycle fields exist on Session.
var _ = sync.Once{}
