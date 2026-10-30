package media

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"
)

// Pipeline orchestration helpers: stage reporting and bounded transient
// retries. They are shared by every backend (plain HTTP, yt-dlp) so the
// download ladder behaves uniformly no matter which path a URL takes.

// reportStage emits a stage transition with no byte counts attached. The
// percent is -1 ("unknown") rather than 0 so a dashboard never renders a
// regressing bar when only the stage changed.
func (m *Manager) reportStage(kind, url, id, stage string) {
	m.reportProgress(Progress{URL: url, ID: id, Stage: stage, Kind: kind, Percent: -1, ETA: -1})
}

// transientRetryAttempts is the number of total attempts for the direct HTTP
// backend. One initial attempt plus two bounded retries: a flaky CDN or a
// reset connection is worth re-trying, a dead link is not.
const transientRetryAttempts = 3

// transientRetryBaseDelay is the first backoff; each subsequent attempt
// doubles it (1s, 2s). Jitter is unnecessary at this scale — downloads are
// per-session and already concurrency-gated.
const transientRetryBaseDelay = 1 * time.Second

// isTransientErr classifies a download failure as worth retrying. Permanent
// verdicts — validation, SSRF, size limits, unsupported kinds, missing
// tools, cancellation — fail fast: retrying them can never succeed and only
// delays the honest error the user sees.
func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, errSizeLimit) || errors.Is(err, ErrUnsupported) ||
		errors.Is(err, ErrUnsafeURL) || errors.Is(err, ErrDisabled) {
		return false
	}
	var missing *ErrMissingTool
	if errors.As(err, &missing) {
		return false
	}
	// Network-level transients.
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	// DNS hiccups surface as "server misbehaving" / "no such host" strings
	// from the Go resolver.
	msg := err.Error()
	for _, marker := range []string{"server misbehaving", "connection reset", "broken pipe", "incomplete read"} {
		if strings.Contains(strings.ToLower(msg), marker) {
			return true
		}
	}
	// HTTP 5xx from the fetch path ("fetch host: HTTP 503"). A 5xx is the
	// server's own "try again" verdict; 4xx is a permanent client error.
	if i := strings.LastIndex(msg, "HTTP 5"); i >= 0 && i+6 <= len(msg) {
		return true
	}
	return false
}

// retryTransient runs fn up to attempts times, retrying only transient
// failures with exponential backoff. ctx cancellation aborts immediately
// between attempts. The last error is returned when every attempt fails.
func (m *Manager) retryTransient(ctx context.Context, attempts int, base time.Duration, tag, id string, fn func(attempt int) error) error {
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn(attempt)
		if err == nil {
			return nil
		}
		last = err
		if attempt == attempts || !isTransientErr(err) {
			return err
		}
		delay := base << (attempt - 1)
		m.log.Warnf("[PIPELINE:download] %s: attempt %d/%d failed (%v) — retrying in %s", id, attempt, attempts, err, delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return last
}
