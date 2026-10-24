package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// Download & Stream integration.
//
// The engine asks the media manager to resolve a remote URL into a playable
// LOCAL file; the media tab then loads the loopback player pointed at the
// local file exactly as it does for any direct media URL. Everything heavy —
// fetching, subprocesses, expiry, the janitor — lives in internal/media.

// downloadResult is the engine-side view of one resolved download.
type downloadResult struct {
	LocalURL    string
	Path        string
	ContentType string
	Bytes       int64
	ExpiresAt   time.Time
	Via         string
}

// ModeOf returns the effective playback mode for this session, defaulting to
// Browser so existing callers keep their behaviour when nothing is set.
func (s *Session) ModeOf() mediateam.Mode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.playMode
}

// downloadMedia resolves target to a local file through the media manager.
// It returns ErrDownloadUnavailable when the feature is off, so callers can
// fall back to Browser mode or surface a clear error.
func (s *Session) downloadMedia(ctx context.Context, target MediaTarget) (*downloadResult, error) {
	mgr := s.mediaManager()
	if mgr == nil || !mgr.Enabled() {
		return nil, ErrDownloadUnavailable
	}
	if !mediateam.IsDownloadable(string(target.Kind)) {
		return nil, fmt.Errorf("%w: %s links are not downloadable", ErrUnsupported, target.Kind)
	}
	return s.runDownload(ctx, mgr, target)
}

// ErrDownloadUnavailable means the Download & Stream feature is disabled.
var ErrDownloadUnavailable = errors.New("download & stream is not available")

// ErrUnsupported means the media cannot be downloaded (a web page, an unknown
// content type). Distinct from a network failure: never retried forever.
var ErrUnsupported = errors.New("media kind is not downloadable")

// prepareLocalMedia fetches the session's media to disk and repoints the
// session at the local file. It is called before the media tab is opened in
// Download and Auto modes; Browser mode never reaches it.
func (s *Session) prepareLocalMedia() error {
	mgr := s.mediaManager()
	if mgr == nil || !mgr.Enabled() {
		return ErrDownloadUnavailable
	}
	target := MediaTarget{URL: s.MediaURL, Kind: s.MediaKind}
	res, err := s.downloadMedia(context.Background(), target)
	if err != nil {
		return err
	}
	if res == nil || strings.TrimSpace(res.LocalURL) == "" {
		return errors.New("download produced no local file")
	}
	s.mu.Lock()
	s.localFile = &localMedia{
		url:       res.LocalURL,
		path:      res.Path,
		expiresAt: res.ExpiresAt,
		via:       res.Via,
		bytes:     res.Bytes,
	}
	playMode := s.playMode
	s.mu.Unlock()
	s.logger().Infof("[PIPELINE:session] session %s: media downloaded via %s (%s, mode=%s, expires %s)",
		s.ID, res.Via, humanBytesI(res.Bytes), playMode, res.ExpiresAt.Format(time.RFC3339))
	return nil
}

// runDownload performs the fetch through the media manager.
func (s *Session) runDownload(ctx context.Context, mgr *mediateam.Manager, target MediaTarget) (*downloadResult, error) {
	id := s.downloadID()
	// The manager owns dedupe, concurrency gating, size/time limits and the
	// expiry bookkeeping. The URL was already classified by the caller.
	res, err := mgr.Download(ctx, mediateam.DownloadRequest{
		URL:  target.URL,
		Kind: string(target.Kind),
		ID:   id,
	})
	if err != nil {
		return nil, err
	}
	return &downloadResult{
		LocalURL:    res.URL,
		Path:        res.Path,
		ContentType: res.ContentType,
		Bytes:       res.Bytes,
		ExpiresAt:   res.ExpiresAt,
		Via:         res.FetchedVia,
	}, nil
}

// humanBytesI renders a byte count for engine logs.
func humanBytesI(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for n2 := n / unit; n2 >= unit; n2 /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// downloadID is the stable stem for this session's downloads.
func (s *Session) downloadID() string {
	return "ses-" + strings.TrimSpace(string(s.ID))
}

// mediaManager returns the manager's media manager (nil when absent).
func (s *Session) mediaManager() *mediateam.Manager {
	if s.mgr == nil {
		return nil
	}
	return s.mgr.mediaMgr
}

// fallbackToDownload is Auto mode's single, bounded fallback: the browser
// pipeline failed to play the remote source, so fetch the media to disk and
// repoint the media tab at the local file. It runs once per session — never
// a retry loop, and never for Download mode (which already tried before the
// tab was opened).
func (s *Session) fallbackToDownload() error {
	if s.playMode != mediateam.ModeAuto {
		return errors.New("auto fallback is only available in Auto mode")
	}
	if s.localFile != nil {
		return errors.New("a local file is already in use")
	}
	if err := s.prepareLocalMedia(); err != nil {
		return err
	}
	// Point the existing media tab at the local file. The share stays bound
	// to the tab, so no re-present is needed (or allowed).
	media := s.mediaTab()
	if media == nil {
		return errors.New("media page is not available")
	}
	s.mu.RLock()
	local := s.localFile
	s.mu.RUnlock()
	if local == nil {
		return errors.New("no local file after download")
	}
	if err := s.navigateMediaTab(media, local.url, MediaDirect); err != nil {
		return fmt.Errorf("navigate to local file: %w", err)
	}
	if err := s.prepareMedia(media); err != nil {
		return fmt.Errorf("prepare local media: %w", err)
	}
	if err := s.playMedia(); err != nil {
		return fmt.Errorf("play local media: %w", err)
	}
	s.setState(StateStreaming, "playing from downloaded media (auto fallback)")
	s.log.Infof("[PIPELINE:session] session %s: auto fallback complete, playing local file", s.ID)
	return nil
}

// ---- Mode helpers for the Telegram layer -----------------------------------

// MediaModeBrowser/Auto/Download are the playback-mode string constants the
// bot and the engine share. They mirror media.Mode so the bot does not import
// the media package directly.
const (
	MediaModeBrowser  = "browser"
	MediaModeDownload = "download"
	MediaModeAuto     = "auto"
)

// NormalizePlaybackMode validates a mode string, defaulting to Browser so
// every existing caller keeps its behaviour when nothing is set.
func NormalizePlaybackMode(s string) string {
	return string(mediateam.NormalizeMode(s))
}

// PlaybackModeDisplay is the human label for cards and logs.
func PlaybackModeDisplay(s string) string {
	return mediateam.NormalizeMode(s).Display()
}

// PlaybackModeEmoji is the dashboard icon for a mode.
func PlaybackModeEmoji(s string) string {
	return mediateam.NormalizeMode(s).Emoji()
}
