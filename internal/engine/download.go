package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	ID          string
	LocalURL    string
	Path        string
	ContentType string
	Bytes       int64
	ExpiresAt   time.Time
	Via         string
}

// leaseMedia pins the downloaded file behind res to this session so the
// media TTL cannot delete it while the session is still playing it. The TTL
// stays an orphan-cleanup policy: an expired file is only reclaimed once no
// lease holds it, so a long video (or a paused stream) survives its own
// deadline for the whole owning session.
//
// The lease is taken before the session records the local file, so there is
// no window in which the janitor can delete a file the session is about to
// adopt. It is released on stop, close, media switch, mode change or failure
// (see releaseLocalMedia).
func (s *Session) leaseMedia(res *downloadResult) {
	if res == nil || strings.TrimSpace(res.ID) == "" {
		return
	}
	mgr := s.mediaManager()
	if mgr == nil || !mgr.Enabled() {
		return
	}
	if !mgr.Lease(res.ID, s.leaseOwner()) {
		s.logger().Warnf("[PIPELINE:session] session %s: could not lease %s (already reaped?)", s.ID, res.ID)
	}
}

// leaseOwner is the identity this session registers leases under. It derives
// from the session id so the lease map and the download manifest stay
// greppable together.
func (s *Session) leaseOwner() string { return "owner-" + strings.TrimSpace(string(s.ID)) }

// releaseLease drops this session's lease on one entry. force=false keeps the
// file alive while another session still holds it; force=true is the session
// teardown path, which still honours remaining owners and only deletes when
// the entry is unowned.
func (s *Session) releaseLease(id string, force bool) {
	mgr := s.mediaManager()
	if mgr == nil || !mgr.Enabled() || strings.TrimSpace(id) == "" {
		return
	}
	if force {
		mgr.ReleaseAndDelete(id, s.leaseOwner())
	} else {
		mgr.Release(id, s.leaseOwner())
	}
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
	// A Telegram-ingested file is already local: its URL is the loopback URL
	// of a managed entry. Re-fetching it through the downloader would both
	// duplicate the file and point the session at a second copy; instead
	// adopt the existing entry directly.
	if entry, ok := mgr.FindByURL(s.MediaURL); ok {
		// Lease before recording the local file: the entry already carries an
		// expiry deadline, so the adoption must not race the janitor.
		s.leaseMedia(&downloadResult{ID: entry.ID, LocalURL: s.MediaURL, Path: entry.Path,
			ContentType: entry.ContentType, Bytes: entry.Bytes, ExpiresAt: entry.ExpiresAt,
			Via: "telegram"})
		s.mu.Lock()
		s.localFile = &localMedia{
			id:        entry.ID,
			url:       s.MediaURL,
			path:      entry.Path,
			expiresAt: entry.ExpiresAt,
			via:       "telegram",
			bytes:     entry.Bytes,
		}
		playMode := s.playMode
		s.mu.Unlock()
		s.logger().Infof("[PIPELINE:session] session %s: media already local (%s, mode=%s, expires %s)",
			s.ID, humanBytesI(entry.Bytes), playMode, entry.ExpiresAt.Format(time.RFC3339))
		return nil
	}
	res, err := s.downloadMedia(context.Background(), target)
	if err != nil {
		return err
	}
	if res == nil || strings.TrimSpace(res.LocalURL) == "" {
		return errors.New("download produced no local file")
	}
	// Lease before the local file is recorded, so the janitor cannot delete
	// it between the download completing and the session owning it.
	s.leaseMedia(res)
	s.mu.Lock()
	s.localFile = &localMedia{
		id:        res.ID,
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
	return s.runDownloadWithID(ctx, mgr, target, id)
}

// runDownloadWithID performs the fetch with a caller-chosen id stem.
func (s *Session) runDownloadWithID(ctx context.Context, mgr *mediateam.Manager, target MediaTarget, id string) (*downloadResult, error) {
	if strings.TrimSpace(id) == "" {
		id = s.downloadID()
	}
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
		ID:          id,
		LocalURL:    res.URL,
		Path:        res.Path,
		ContentType: res.ContentType,
		Bytes:       res.Bytes,
		ExpiresAt:   res.ExpiresAt,
		Via:         res.FetchedVia,
	}, nil
}

// releaseLocalMedia removes the session's active downloaded media and frees
// the file from disk immediately, while never destroying a file another
// session still owns. The session teardown path (stop, close, switch, mode
// change) releases the lease with force=true: the file is deleted unless a
// different owner still holds a lease on the same entry.
func (s *Session) releaseLocalMedia(why string) {
	s.mu.Lock()
	local := s.localFile
	s.localFile = nil
	s.mu.Unlock()

	if local != nil {
		s.freeLocalMediaEntry(local, why)
	}
	// Residue guard: a crashed or superseded download can leave per-id files
	// behind with no localFile recorded. Remove() is unconditional here —
	// this is the explicit teardown path, and any genuine co-owner holds its
	// own lease on the manifest entry, not on these leftovers.
	mgr := s.mediaManager()
	if mgr != nil && mgr.Enabled() {
		_ = mgr.Remove(s.downloadID())
		_ = mgr.Sweep(context.Background())
	}
}

// freeLocalMediaEntry releases one downloaded file. force semantics live on
// the lease API: the file is deleted unless another session still holds a
// lease on the same entry (two sessions sharing one Telegram-ingested file,
// for instance).
func (s *Session) freeLocalMediaEntry(local *localMedia, why string) {
	if local == nil {
		return
	}
	id := local.id
	if id == "" {
		id = s.downloadID()
	}
	mgr := s.mediaManager()
	if mgr != nil && mgr.Enabled() {
		// Session teardown: force-release the lease, which deletes the file
		// only when no other owner remains.
		if !mgr.ReleaseAndDelete(id, s.leaseOwner()) {
			owners := mgr.LeaseOwners(id)
			if len(owners) > 0 {
				s.logger().Infof("[PIPELINE:session] session %s: kept local media %s (%s); still owned by %v", s.ID, id, why, owners)
				return
			}
		} else {
			s.logger().Infof("[PIPELINE:session] session %s: freed local media %s (%s)", s.ID, id, why)
			return
		}
		// ReleaseAndDelete returned false with no remaining owners: the entry
		// was already gone, or the manifest write failed. Sweep any residue.
		_ = mgr.Remove(id)
	} else if local.path != "" {
		_ = os.Remove(local.path)
	}
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
