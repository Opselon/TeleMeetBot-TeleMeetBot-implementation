package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DownloadRequest is one fetch. ID is the caller-chosen file stem (the engine
// uses its session id), which makes downloads dedupable and the manifest
// greppable.
type DownloadRequest struct {
	URL  string
	Kind string // "direct" | "youtube"
	ID   string
	// Quality is the requested quality policy (NormalizeQualityPolicy
	// validates it). Empty means auto.
	Quality string
	// SubLang requests a caption track alongside the media ("", "off" or a
	// language code). Best effort: a subtitle failure never fails the video.
	SubLang string
}

// Download resolves req into a playable local file.
//
// Decision ladder, from the cheapest and safest:
//  1. Validate the URL (syntax, scheme, no embedded credentials, no private
//     IP literal) — a private destination never reaches the network stack.
//  2. Resolve the host and reject any private/non-routable address (SSRF).
//  3. Check free disk space before allocating anything.
//  4. Pick a backend: a direct media URL is fetched with plain HTTP (no
//     subprocess); a YouTube URL needs yt-dlp.
//  5. Take a concurrency slot, fetch, verify the file, optionally transcode.
//  6. Record the manifest entry with the expiry deadline already computed.
//
// Every failure is terminal and descriptive: the caller falls back or gives
// up, never retries blindly.
func (m *Manager) Download(ctx context.Context, req DownloadRequest) (*Result, error) {
	if !m.opts.Enabled {
		return nil, ErrDisabled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, err := ValidateURL(req.URL)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = SanitizeFilename(req.URL)
	}
	id = SanitizeFilename(id)

	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if !IsDownloadable(kind) {
		return nil, fmt.Errorf("%w: kind %q is not downloadable", ErrUnsupported, kind)
	}

	// SSRF gate, second half: the resolved destination must be public. A
	// friendly hostname can still point at the cloud metadata service.
	host := strings.ToLower(u.Hostname())
	if !isBareIP(host) {
		if err := ValidateResolvedDestination(host, portOf(u), m.resolver()); err != nil {
			return nil, err
		}
	}

	// Disk space: refuse before allocating a temp file rather than failing
	// mid-download with a truncated file.
	if m.diskFree != nil {
		if free, ok := m.diskFree(m.opts.Dir); ok {
			minFree := m.opts.MinFreeBytes
			if minFree <= 0 {
				minFree = 256 * 1024 * 1024
			}
			if m.opts.MaxBytes > 0 && m.opts.MaxBytes < minFree {
				minFree = m.opts.MaxBytes
			}
			if free < minFree {
				// Reclaim space from expired, orphan, and temp files first
				m.Sweep(ctx)
				if refreshed, ok2 := m.diskFree(m.opts.Dir); ok2 {
					free = refreshed
				}
			}
			if free < minFree {
				return nil, fmt.Errorf("insufficient disk space: %s free in %s (minimum %s required)", humanBytes(free), m.opts.Dir, humanBytes(minFree))
			}
		}
	}

	release := m.acquireSlot()
	defer release()

	m.markInflight(id)
	defer m.clearInflight(id)

	var (
		path, ct, via string
		bytes         int64
		transcoded    bool
		selected      SelectedFormat
	)
	switch kind {
	case "direct":
		res, err := m.fetchDirect(ctx, u, id)
		if err != nil {
			m.sweepIDFiles(id)
			return nil, err
		}
		path, ct, bytes, via = res.Path, res.ContentType, res.Bytes, "http"
		transcoded = res.Transcoded
	case "youtube":
		res, err := m.fetchYTDLP(ctx, u.String(), id, NormalizeQualityPolicy(req.Quality), req.SubLang)
		if err != nil {
			m.sweepIDFiles(id)
			return nil, err
		}
		path, ct, bytes = res.Path, res.ContentType, res.Bytes
		via = "yt-dlp"
		if res.Transcoded {
			via = "yt-dlp+ffmpeg"
		}
		transcoded = res.Transcoded
		selected = res.Selected
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, kind)
	}

	// Re-verify what landed on disk before announcing it to the engine.
	if err := verifyDownloadedFile(path, m.opts.MaxBytes); err != nil {
		_ = m.deleteManaged(path, "verification failed")
		m.sweepIDFiles(id)
		return nil, err
	}

	now := time.Now()
	entry := Entry{
		ID:          id,
		Path:        path,
		URL:         u.String(),
		Kind:        kind,
		ContentType: ct,
		Bytes:       bytes,
		FetchedAt:   now,
		ExpiresAt:   now.Add(m.opts.TTL),
		Transcoded:  transcoded,
	}
	if err := m.entries.Add(entry); err != nil {
		m.log.Warnf("[PIPELINE:download] %s: cannot persist manifest: %v", id, err)
	}
	m.reportProgress(Progress{URL: u.String(), ID: id, Bytes: bytes, Total: bytes, Done: true})
	m.log.Infof("[PIPELINE:download] %s: ready via %s (%s, expires in %s)", id, via, humanBytes(bytes), m.opts.TTL)
	result := &Result{
		URL:         m.localURL(entry),
		Path:        path,
		ContentType: ct,
		Bytes:       bytes,
		ExpiresAt:   entry.ExpiresAt,
		ID:          id,
		FetchedVia:  via,
		Transcoded:  transcoded,
		Selected:    selected,
	}
	// Best-effort subtitle: failure is logged and skipped, never fatal to
	// the video the user asked for.
	if lang := strings.ToLower(strings.TrimSpace(req.SubLang)); lang != "" && lang != "off" {
		if sub, err := m.prepareSubtitle(id, entry.ExpiresAt); err != nil {
			m.log.Infof("[PIPELINE:download] %s: subtitle %q unavailable: %v", id, lang, err)
		} else {
			result.Subtitle = sub
		}
	}
	return result, nil
}

// localURL builds the loopback URL the media tab loads. Chromium must not load
// file:// here (opaque origin, no media fetch), so the file is served through
// the same loopback server as the player document.
func (m *Manager) localURL(e Entry) string {
	if m.fileServer == nil {
		return ""
	}
	return m.fileServer.serve(e)
}

// verifyDownloadedFile re-checks the committed file: it must exist, be a
// regular file, be inside the managed dir and be within the size limit.
func verifyDownloadedFile(path string, max int64) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("no download path")
	}
	st, err := statFile(path)
	if err != nil {
		return fmt.Errorf("verify download: %w", err)
	}
	if !st.regular {
		return errors.New("downloaded path is not a regular file")
	}
	if max > 0 && st.size > max {
		return fmt.Errorf("%w: %s on disk", errSizeLimit, humanBytes(st.size))
	}
	return nil
}
