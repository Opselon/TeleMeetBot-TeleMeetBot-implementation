package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options configures the download manager. All limits are enforced hard:
// a request that crosses one fails instead of degrading silently.
type Options struct {
	// Enabled gates the whole feature. When false, Download returns
	// ErrDisabled and Browser mode is untouched.
	Enabled bool
	// Dir is the managed download directory. Only files inside it (after
	// symlink resolution) are ever created or deleted.
	Dir string
	// TTL is the mandatory expiry lifetime of every downloaded file,
	// counted from the moment the download completes. One hour in
	// production; configurable for operations only.
	TTL time.Duration
	// MaxBytes is the per-file size ceiling. Zero disables the limit
	// (not recommended).
	MaxBytes int64
	// Timeout bounds one whole download including transcode.
	Timeout time.Duration
	// MaxConcurrent bounds parallel downloads. Zero means one at a time.
	MaxConcurrent int
	// YTDLPPath is the yt-dlp binary; empty means auto-detect.
	YTDLPPath string
	// FFmpegPath is the ffmpeg binary; empty means auto-detect.
	FFmpegPath string
	// AllowTranscode permits an optional ffmpeg remux/transcode when the
	// fetched container is one Chromium cannot decode.
	AllowTranscode bool
	// HTTPTimeout bounds metadata fetches and per-request reads.
	HTTPTimeout time.Duration
	// UserAgent is used for plain HTTP fetches; empty uses a stock UA.
	UserAgent string
	// Proxy is handed to yt-dlp (e.g. "http://172.17.0.1:3339"). YouTube
	// bot-blocks datacenter egress ("Sign in to confirm you're not a bot"),
	// so a download from a VPS without the operator's proxy can never
	// extract a YouTube URL. Empty means a direct connection.
	Proxy string
}

// DefaultOptions returns the production policy: one hour TTL, 2 GiB ceiling,
// ten-minute timeout, single concurrent download.
func DefaultOptions(dir string) Options {
	return Options{
		Enabled:        true,
		Dir:            dir,
		TTL:            time.Hour,
		MaxBytes:       2 * 1024 * 1024 * 1024,
		Timeout:        10 * time.Minute,
		MaxConcurrent:  1,
		AllowTranscode: true,
		HTTPTimeout:    30 * time.Second,
	}
}

// Validate checks the option invariants before the manager starts.
func (o Options) Validate() error {
	if !o.Enabled {
		return nil
	}
	if strings.TrimSpace(o.Dir) == "" {
		return errors.New("download.dir is empty")
	}
	if o.TTL <= 0 {
		return errors.New("download.ttl_minutes must be positive")
	}
	if o.TTL > 24*time.Hour {
		return fmt.Errorf("download.ttl_minutes must be at most 1440 (24h), got %s", o.TTL)
	}
	if o.MaxBytes < 0 {
		return errors.New("download.max_size_mb must not be negative")
	}
	if o.Timeout <= 0 {
		return errors.New("download.timeout_seconds must be positive")
	}
	if o.MaxConcurrent < 0 {
		return errors.New("download.max_concurrent must not be negative")
	}
	return nil
}

// ResolveDir turns a possibly relative or empty Dir into an absolute path and
// makes sure it exists with private permissions. An empty Dir defaults to
// <baseDir>/downloads; a relative Dir is anchored on baseDir; an absolute Dir
// is used as-is.
func (o *Options) ResolveDir(baseDir string) error {
	dir := strings.TrimSpace(o.Dir)
	base := strings.TrimSpace(baseDir)
	if dir == "" {
		if base == "" {
			return errors.New("download.dir is empty and no base dir is known")
		}
		dir = "downloads"
	}
	if !filepath.IsAbs(dir) {
		if base == "" {
			// Nothing to anchor on: resolve against the process CWD.
			abs, err := filepath.Abs(dir)
			if err != nil {
				return fmt.Errorf("resolve download dir: %w", err)
			}
			dir = abs
		} else {
			absBase, err := filepath.Abs(base)
			if err != nil {
				return fmt.Errorf("resolve base dir: %w", err)
			}
			dir = filepath.Join(absBase, dir)
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve download dir: %w", err)
	}
	o.Dir = abs
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return fmt.Errorf("create download dir %s: %w", o.Dir, err)
	}
	return nil
}
