package media

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Result is what a successful download gives back to the engine.
type Result struct {
	// URL is the loopback URL the media tab should load.
	URL string
	// Path is the absolute local path of the managed file.
	Path string
	// ContentType sniffed at fetch time.
	ContentType string
	// Bytes on disk.
	Bytes int64
	// ExpiresAt is the absolute deadline after which the file MUST be gone.
	ExpiresAt time.Time
	// ID of the entry (also the file stem).
	ID string
	// FetchedVia names the backend that produced the file ("http" | "yt-dlp"
	// | "yt-dlp+ffmpeg").
	FetchedVia string
	// Transcoded reports whether ffmpeg produced the final file.
	Transcoded bool
}

// ErrDisabled means the feature is off; callers must keep Browser mode.
var ErrDisabled = errors.New("download & stream is disabled")

// ErrUnsupported means the link is not fetchable as media (pages, unknown
// types); callers fall back or fail, never retry forever.
var ErrUnsupported = errors.New("media is not downloadable")

// ErrMissingTool means an optional backend binary is absent. Distinct from a
// generic failure so the UI can tell the operator what to install without
// breaking Browser mode.
type ErrMissingTool struct{ Name, Path, Hint string }

func (e *ErrMissingTool) Error() string {
	return fmt.Sprintf("optional dependency %s is not available (looked for %s): %s", e.Name, e.Path, e.Hint)
}
func (e *ErrMissingTool) Unwrap() error { return ErrUnsupported }

// Logger is the minimal logging surface (mirrors engine.Logger).
type Logger interface {
	Debugf(format string, args ...interface{})
	Infof(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

type noopLogger struct{}

func (noopLogger) Debugf(string, ...interface{}) {}
func (noopLogger) Infof(string, ...interface{})  {}
func (noopLogger) Warnf(string, ...interface{})  {}
func (noopLogger) Errorf(string, ...interface{}) {}

// Manager owns downloads, their manifest and their expiry. It is safe for
// concurrent use; the engine constructs one per process.
type Manager struct {
	opts Options
	log  Logger

	entries *entryStore

	// gate bounds concurrent downloads (MaxConcurrent).
	gate chan struct{}
	// gateMu protects gate construction for the zero-value case.
	gateMu sync.Mutex

	client *http.Client
	// fileServer serves managed files over loopback HTTP.
	fileServer *fileServer
	// progress fans download progress events to subscribers.
	progress *progressBus
	// lookupFn overrides DNS resolution (tests). nil = net.LookupIP.
	lookupFn func(string) ([]net.IP, error)
	// ytdlpFn/ffmpegFn override the subprocess invocation (tests).
	ytdlpFn  func(ctx context.Context, args []string) ([]byte, []byte, error)
	ffmpegFn func(ctx context.Context, args []string) ([]byte, []byte, error)
	// retryDelay overrides the sleep between yt-dlp attempts (tests).
	retryDelay func(attempt int) time.Duration

	// janitorOnce guards janitor startup; stopJanitor ends the loop.
	janitorOnce sync.Once
	stopJanitor chan struct{}

	// diskFree returns the bytes free on the download filesystem. Overridable
	// in tests.
	diskFree func(path string) (int64, bool)

	mu       sync.Mutex
	inflight map[string]struct{}
}

// NewManager builds a download manager. opts must be resolved and validated
// by the caller (see Options.Validate); NewManager itself is forgiving so a
// misconfiguration degrades to "downloads disabled" rather than a panic.
func NewManager(opts Options, log Logger) (*Manager, error) {
	if log == nil {
		log = noopLogger{}
	}
	if !opts.Enabled {
		return &Manager{opts: opts, log: log, entries: newEntryStore(opts.Dir)}, nil
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	m := &Manager{
		opts:     opts,
		log:      log,
		entries:  newEntryStore(opts.Dir),
		client:   clientWithDialer(opts.HTTPTimeout, nil),
		inflight: map[string]struct{}{},
	}
	// Load the manifest eagerly: a restart must know what it manages (and
	// quarantine a corrupt file) before the first download or sweep runs.
	if err := m.entries.load(); err != nil {
		log.Warnf("[PIPELINE:download] cannot read manifest: %v", err)
	}
	m.client.CheckRedirect = m.checkRedirect
	m.fileServer = newFileServer()
	m.progress = newProgressBus()
	if m.diskFree == nil {
		m.diskFree = diskFreeBytes
	}
	return m, nil
}

// Opts returns a copy of the options.
func (m *Manager) Opts() Options { return m.opts }

// SetTTL overrides the expiry lifetime. Test seam: production reads the TTL
// from the config (default one hour) and never changes it at runtime.
func (m *Manager) SetTTL(d time.Duration) { m.opts.TTL = d }

// Enabled reports whether downloads are on.
func (m *Manager) Enabled() bool { return m.opts.Enabled }

// gateSlot returns the concurrency semaphore, creating it lazily.
func (m *Manager) gateSlot() chan struct{} {
	m.gateMu.Lock()
	defer m.gateMu.Unlock()
	if m.gate == nil {
		n := m.opts.MaxConcurrent
		if n <= 0 {
			n = 1
		}
		if n > 8 {
			n = 8
		}
		m.gate = make(chan struct{}, n)
	}
	return m.gate
}

// SubscribeProgress registers a listener for download progress events. The
// returned function unsubscribes. Never blocks the download path.
func (m *Manager) SubscribeProgress(fn func(Progress)) func() {
	if m.progress == nil {
		return func() {}
	}
	return m.progress.subscribe(fn)
}

// reportProgress publishes one event. Total is zero when unknown.
func (m *Manager) reportProgress(p Progress) {
	if m.progress == nil {
		return
	}
	m.progress.notify(p)
}

// acquireSlot takes one concurrency slot and returns a releaser. It blocks
// until a slot is free: bounded parallelism, no queue explosion.
func (m *Manager) acquireSlot() func() {
	g := m.gateSlot()
	g <- struct{}{}
	return func() { <-g }
}

// YTDLP returns the effective yt-dlp path. An explicit operator setting wins
// (reported as available even when it does not exist, so the failure names the
// configured path); otherwise the PATH is searched.
func (m *Manager) YTDLP() (string, bool) {
	if p := strings.TrimSpace(m.opts.YTDLPPath); p != "" {
		return p, true
	}
	path, err := exec.LookPath("yt-dlp")
	return path, err == nil
}

// FFmpeg returns the effective ffmpeg path (explicit config, then PATH).
func (m *Manager) FFmpeg() (string, bool) {
	if p := strings.TrimSpace(m.opts.FFmpegPath); p != "" {
		if path, err := exec.LookPath(p); err == nil {
			return path, true
		}
	}
	path, err := exec.LookPath("ffmpeg")
	return path, err == nil
}

// Start launches the expiry janitor and performs the startup sweep. It is
// idempotent.
func (m *Manager) Start(ctx context.Context) {
	if !m.opts.Enabled {
		return
	}
	// Startup sweep FIRST: expired files must be gone before the first
	// download can land, regardless of what the janitor ticker does next.
	if swept := m.Sweep(context.Background()); swept > 0 {
		m.log.Infof("[PIPELINE:download] startup sweep removed %d expired file(s)", swept)
	}
	m.janitorOnce.Do(func() {
		m.stopJanitor = make(chan struct{})
		go m.janitorLoop(ctx)
	})
}

// Stop ends the janitor. Downloads in flight are cancelled by their own ctx.
func (m *Manager) Stop() {
	m.janitorOnce.Do(func() {}) // never start after a stop
	if m.stopJanitor != nil {
		close(m.stopJanitor)
		m.stopJanitor = nil
	}
}

// Close stops the janitor and the loopback file server.
func (m *Manager) Close() {
	m.Stop()
	if m.fileServer != nil {
		m.fileServer.close()
	}
}

// janitorInterval picks the sweep cadence: at least three sweeps per TTL, so
// expiry always lands within the mandate even if one sweep is delayed.
func janitorInterval(ttl time.Duration) time.Duration {
	d := ttl / 3
	if d < 30*time.Second {
		d = 30 * time.Second
	}
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// janitorLoop removes expired files on a ticker until Stop or ctx cancel.
func (m *Manager) janitorLoop(ctx context.Context) {
	t := time.NewTicker(janitorInterval(m.opts.TTL))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopJanitor:
			return
		case <-t.C:
			if n := m.Sweep(ctx); n > 0 {
				m.log.Infof("[PIPELINE:download] janitor removed %d expired file(s)", n)
			}
		}
	}
}

// Sweep deletes every expired entry and its file. It runs on the janitor
// ticker, on startup and on demand; it is also the crash-recovery path. It
// never deletes a file outside the managed directory and never throws.
func (m *Manager) Sweep(ctx context.Context) int {
	if m == nil || !m.opts.Enabled {
		return 0
	}
	if err := m.entries.load(); err != nil {
		m.log.Warnf("[PIPELINE:download] sweep: cannot read manifest: %v", err)
		return 0
	}
	now := time.Now()
	removed, err := m.entries.RemoveIf(func(e Entry) bool { return e.Expired(now) })
	if err != nil {
		m.log.Warnf("[PIPELINE:download] sweep: cannot persist manifest: %v", err)
	}
	n := 0
	for _, e := range removed {
		if m.deleteManaged(e.Path, "expired") {
			n++
		}
	}
	// Partial downloads left by a crash or a cancel are nameless tmp files;
	// the manifest never lists them, so reap them separately.
	n += m.sweepTempFiles()
	return n
}

// sweepTempFiles removes stale .tmp-* files older than the download timeout.
// They are the residue of a crashed or cancelled fetch; leaving them leaks
// disk without bound.
func (m *Manager) sweepTempFiles() int {
	ents, err := os.ReadDir(m.opts.Dir)
	if err != nil {
		return 0
	}
	n := 0
	cutoff := time.Now().Add(-2 * m.opts.Timeout)
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasPrefix(ent.Name(), ".tmp-") {
			continue
		}
		info, err := ent.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue // still young: maybe an in-flight download
		}
		if m.deleteManaged(filepath.Join(m.opts.Dir, ent.Name()), "stale temp") {
			n++
		}
	}
	return n
}

// deleteManaged removes one path, but only after re-verifying it resolves
// inside the managed directory. Returns true when a file was actually removed.
func (m *Manager) deleteManaged(path, why string) bool {
	target, err := SecureDelete(m.opts.Dir, path)
	if err != nil {
		m.log.Warnf("[PIPELINE:download] refuse to delete %s (%s): %v", path, why, err)
		// A entry whose path is unsafe is dropped from the manifest anyway:
		// never keep a record pointing at a file the bot cannot manage.
		_, _ = m.entries.RemoveIf(func(e Entry) bool { return e.Path == path })
		return false
	}
	if err := os.Remove(target); err != nil {
		if os.IsNotExist(err) {
			m.fileServer.forget(path)
			return false // already gone: the outcome the caller wanted
		}
		m.log.Warnf("[PIPELINE:download] delete %s (%s): %v", target, why, err)
		return false
	}
	m.log.Infof("[PIPELINE:download] removed %s (%s)", target, why)
	m.fileServer.forget(path)
	return true
}

// diskFreeBytes reports free bytes on the filesystem holding path.
func diskFreeBytes(path string) (int64, bool) {
	var st syscallStatfs
	if err := statfs(path, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}

// Status reports the manager state for the health/dashboard surface.
type Status struct {
	Enabled         bool      `json:"enabled"`
	Dir             string    `json:"dir"`
	TTLMinutes      int       `json:"ttl_minutes"`
	MaxBytes        int64     `json:"max_bytes"`
	MaxConcurrent   int       `json:"max_concurrent"`
	ManagedFiles    int       `json:"managed_files"`
	ManagedBytes    int64     `json:"managed_bytes"`
	YTDLPAvailable  bool      `json:"ytdlp_available"`
	FFmpegAvailable bool      `json:"ffmpeg_available"`
	NextExpiry      time.Time `json:"next_expiry,omitempty"`
}

// Snapshot returns the current manager status.
func (m *Manager) Snapshot() Status {
	st := Status{
		Enabled:       m.opts.Enabled,
		Dir:           m.opts.Dir,
		TTLMinutes:    int(m.opts.TTL / time.Minute),
		MaxBytes:      m.opts.MaxBytes,
		MaxConcurrent: m.opts.MaxConcurrent,
	}
	if !m.opts.Enabled {
		return st
	}
	_, st.YTDLPAvailable = m.YTDLP()
	_, st.FFmpegAvailable = m.FFmpeg()
	var next time.Time
	for _, e := range m.entries.All() {
		st.ManagedFiles++
		st.ManagedBytes += e.Bytes
		if next.IsZero() || e.ExpiresAt.Before(next) {
			next = e.ExpiresAt
		}
	}
	st.NextExpiry = next
	return st
}
