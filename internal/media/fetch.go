package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// directResult is what a plain HTTP fetch yields.
type directResult struct {
	Path        string
	ContentType string
	Bytes       int64
	Transcoded  bool
}

// errSizeLimit is the sentinel for crossing MaxBytes.
var errSizeLimit = errors.New("download exceeds size limit")

// maxRedirects bounds the redirect chain the manager will follow.
const maxRedirects = 5

// fetchDirect downloads url to a temp file inside the managed directory using
// the shared HTTP client. Content-Length is checked BEFORE the body is read
// when the server reports it; otherwise the running count enforces the limit
// mid-stream.
func (m *Manager) fetchDirect(ctx context.Context, u *url.URL, id string) (*directResult, error) {
	if m.opts.HTTPTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.opts.HTTPTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", m.userAgent())
	req.Header.Set("Accept", "audio/*,video/*,*/*;q=0.9")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s: HTTP %d", u.Host, resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !AllowedContentType(ct) {
		return nil, fmt.Errorf("unsupported content type %q", firstLine(ct))
	}
	if m.opts.MaxBytes > 0 {
		if resp.ContentLength > m.opts.MaxBytes {
			return nil, fmt.Errorf("%w: server reports %s, limit is %s",
				errSizeLimit, humanBytes(resp.ContentLength), humanBytes(m.opts.MaxBytes))
		}
		if resp.ContentLength < 0 {
			m.log.Warnf("[PIPELINE:download] %s: unknown length, enforcing limit mid-stream", id)
		}
	}

	tmp, err := m.newTempFile(id)
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	n, err := m.copyLimitedProgress(ctx, resp.Body, tmp, m.opts.MaxBytes, func(read, speed int64) {
		m.reportProgress(Progress{URL: u.String(), ID: id, Bytes: read, Total: resp.ContentLength, Speed: speed})
	})
	if err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("copy body: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temp file: %w", err)
	}
	if n == 0 {
		return nil, errors.New("empty response body")
	}

	finalName, err := m.finalName(id, guessExt(u, ct))
	if err != nil {
		return nil, err
	}
	finalPath, err := SafeJoin(m.opts.Dir, finalName)
	if err != nil {
		return nil, err
	}
	// Rename within the same directory is atomic; a stale final file from a
	// previous run with the same id is replaced.
	_ = os.Remove(finalPath)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return nil, fmt.Errorf("commit file: %w", err)
	}
	removeTmp = false
	if err := os.Chmod(finalPath, 0o600); err != nil {
		m.log.Warnf("[PIPELINE:download] %s: chmod: %v", id, err)
	}
	m.log.Infof("[PIPELINE:download] %s: fetched %s (%s, type %s)", id, humanBytes(n), u.Host, firstLine(ct))
	return &directResult{Path: finalPath, ContentType: ct, Bytes: n}, nil
}

// copyLimited copies from r to w, failing as soon as more than max bytes would
// be written (max <= 0 means unlimited). A stream that ends exactly at the
// limit is accepted: the loop reads one extra time to tell EOF from truncation.
func (m *Manager) copyLimited(r io.Reader, w io.Writer, max int64) (int64, error) {
	buf := make([]byte, 64*1024)
	var written int64
	for {
		if max > 0 && written >= max {
			n, err := r.Read(buf)
			if n > 0 {
				return written, fmt.Errorf("%w after %s", errSizeLimit, humanBytes(written+int64(n)))
			}
			if err == io.EOF || err == nil {
				return written, nil
			}
			return written, err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if max > 0 && written+int64(n) > max {
				return written, fmt.Errorf("%w after %s", errSizeLimit, humanBytes(written+int64(n)))
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
		}
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, err
		}
	}
}

// newTempFile creates a private temp file INSIDE the managed directory so the
// committed download never crosses filesystems on rename, and an interrupted
// download never lands outside the bot-managed tree.
func (m *Manager) newTempFile(id string) (*os.File, error) {
	if err := os.MkdirAll(m.opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	f, err := os.CreateTemp(m.opts.Dir, ".tmp-"+sanitizeForPattern(id)+"-")
	if err != nil {
		// Fall back to the plain pattern: a CreateTemp failure must not kill
		// an otherwise healthy download path.
		f, err = os.CreateTemp(m.opts.Dir, ".tmp-")
		if err != nil {
			return nil, fmt.Errorf("create temp file: %w", err)
		}
	}
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("chmod temp file: %w", err)
	}
	return f, nil
}

// sanitizeForPattern keeps the temp-file pattern separator-free.
func sanitizeForPattern(id string) string {
	var sb strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	s := sb.String()
	if s == "" {
		return "media"
	}
	return s
}

// finalName builds the on-disk name for a downloaded file.
func (m *Manager) finalName(id, ext string) (string, error) {
	if strings.TrimSpace(ext) == "" {
		ext = ".mp4"
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	if !AllowedExtension(ext) {
		return "", fmt.Errorf("unsupported extension %q", ext)
	}
	return id + ext, nil
}

// guessExt picks the extension for the saved file from the URL path or the
// server's content type.
func guessExt(u *url.URL, contentType string) string {
	if e := strings.ToLower(filepath.Ext(u.Path)); e != "" && AllowedExtension(e) {
		return e
	}
	switch strings.ToLower(strings.TrimSpace(firstLine(strings.Split(contentType, ";")[0]))) {
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/x-matroska":
		return ".mkv"
	case "video/quicktime":
		return ".mov"
	case "video/ogg", "application/ogg":
		return ".ogv"
	case "video/x-msvideo":
		return ".avi"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/aac":
		return ".aac"
	case "audio/flac":
		return ".flac"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "audio/opus":
		return ".opus"
	default:
		return ".mp4"
	}
}

// firstLine returns the first line of a header value.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// humanBytes renders a byte count for humans.
func humanBytes(n int64) string {
	if n < 0 {
		return "?" + humanBytes(-n)
	}
	f := float64(n)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1f%s", math.Round(f*10)/10, units[i])
}

// userAgent is the UA for plain fetches.
func (m *Manager) userAgent() string {
	if ua := strings.TrimSpace(m.opts.UserAgent); ua != "" {
		return ua
	}
	return defaultUserAgent
}

// defaultUserAgent identifies the bot without leaking a browser vendor.
const defaultUserAgent = "TeleMeetBot/1.0 (media fetcher)"

// clientWithDialer builds an HTTP client whose dialer enforces the SSRF
// destination gate at connect time, so even a DNS rebinding between the
// pre-flight resolve and the actual connect cannot reach a private address.
func clientWithDialer(timeout time.Duration, gate func(network, addr string) bool) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if gate != nil && !gate(network, addr) {
				return nil, fmt.Errorf("%w: dial %s refused (private destination)", ErrUnsafeURL, addr)
			}
			return dialer.DialContext(ctx, network, addr)
		},
		MaxIdleConns:        16,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// checkRedirect is installed on the manager's client so every redirect target
// is re-validated (scheme, host, destination IP) before it is followed.
func (m *Manager) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("too many redirects (>%d)", maxRedirects)
	}
	if _, err := ValidateURL(req.URL.String()); err != nil {
		return fmt.Errorf("redirect target rejected: %w", err)
	}
	host := strings.ToLower(req.URL.Hostname())
	if isBareIP(host) {
		if isPrivateIP(net.ParseIP(strings.Trim(host, "[]"))) {
			return fmt.Errorf("redirect to private address refused: %s", host)
		}
		return nil
	}
	if err := ValidateResolvedDestination(host, portOf(req.URL), m.resolver()); err != nil {
		return fmt.Errorf("redirect target rejected: %w", err)
	}
	return nil
}

// portOf is the effective port of a URL.
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// resolver is overridable in tests.
func (m *Manager) resolver() func(string) ([]net.IP, error) {
	if m.lookupFn != nil {
		return m.lookupFn
	}
	return net.LookupIP
}

// copyLimitedProgress is copyLimited with a mid-stream callback for progress
// reporting. The callback fires at most 4x/second so a big file cannot flood
// the bus. It also calculates instantaneous download speed in bytes/sec.
func (m *Manager) copyLimitedProgress(ctx context.Context, r io.Reader, w io.Writer, max int64, every func(read, speed int64)) (int64, error) {
	buf := make([]byte, 512*1024)
	var written int64
	var lastWritten int64
	lastReport := time.Now()
	for {
		if max > 0 && written >= max {
			n, err := r.Read(buf)
			if n > 0 {
				return written, fmt.Errorf("%w after %s", errSizeLimit, humanBytes(written+int64(n)))
			}
			if err == io.EOF || err == nil {
				return written, nil
			}
			return written, err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if max > 0 && written+int64(n) > max {
				return written, fmt.Errorf("%w after %s", errSizeLimit, humanBytes(written+int64(n)))
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
			now := time.Now()
			elapsed := now.Sub(lastReport)
			if every != nil && elapsed >= 250*time.Millisecond {
				speed := int64(0)
				if sec := elapsed.Seconds(); sec > 0 {
					speed = int64(float64(written-lastWritten) / sec)
				}
				lastReport = now
				lastWritten = written
				every(written, speed)
			}
		}
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, err
		}
		if ctx.Err() != nil {
			return written, ctx.Err()
		}
	}
}
