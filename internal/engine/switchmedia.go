package engine

import (
	"context"
	"errors"
	"fmt"
	"os"

	mediateam "github.com/Opselon/TeleMeetBot/internal/media"
)

// ---- SwitchMedia: change the shared URL without touching the meeting ------

// errSwitchSuperseded unwinds a switch whose generation a newer switch has
// already replaced: the tab belongs to the newer attempt now.
var errSwitchSuperseded = errors.New("switch superseded by a newer media URL")

// SwitchMedia switches the media tab to a different URL while keeping the Meet
// session, the screen share and the browser alive. Chromium's getDisplayMedia
// is bound to the tab, not the URL, so navigating the same tab to a new target
// switches what participants see — no re-join, no re-present, no new browser.
//
// A generation counter protects rapid consecutive URL changes: each call claims
// the newest generation before waiting for the switch lock, so a switch that is
// already driving the tab sees the newer generation at its next checkpoint,
// unwinds without rolling back, and lets the newest call own the tab. Only the
// newest URL ends up active; a stale load can never overwrite a newer one.
//
// If the new URL fails to load or play, the media tab is navigated back to the
// URL that was working before, keeping the share up and the session usable.
// An unsupported URL is rejected before the tab is touched.
func (s *Session) SwitchMedia(newURL string) error {
	target, err := ClassifyMedia(newURL)
	if err != nil {
		s.logger().Warnf("[PIPELINE:switch] session %s: rejected %q: %v", s.ID, truncateStr(newURL, 80), err)
		return err
	}
	// Claim the newest generation BEFORE waiting for the lock. That is what
	// makes rapid consecutive switches safe: a switch already driving the tab
	// sees the newer generation at its next checkpoint, stops early, and lets
	// this call own the outcome instead of finishing a stale navigation.
	gen := s.switchGen.Add(1)

	// Only one navigate → prepare → play cycle may drive the tab at a time.
	s.switchMu.Lock()
	defer s.switchMu.Unlock()

	stale := func() bool {
		return s.switchGen.Load() != gen
	}
	if stale() {
		// Another switch was requested while this one waited for the lock; the
		// newer call applies its URL and reports its own result.
		s.logger().Infof("[PIPELINE:switch] session %s: switch to %q dropped, a newer URL arrived while queued", s.ID, truncateStr(target.URL, 80))
		return nil
	}
	return s.runSwitch(target, stale)
}

// runSwitch performs one switch. The caller must hold switchMu and have already
// claimed gen as the newest generation.
func (s *Session) runSwitch(target MediaTarget, stale func() bool) error {
	log := s.logger()

	s.mu.Lock()
	oldURL, oldKind := s.MediaURL, s.MediaKind
	prevState := s.state
	s.mu.Unlock()

	if s.ctx != nil && s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if s.mediaTab() == nil {
		// No tab means the session never finished opening media; there is
		// nothing to repoint, so the URL fields stay truthful.
		return errors.New("media page is not available")
	}

	log.Infof("[PIPELINE:switch] session %s: switching media %q → %q (kind=%s)", s.ID, truncateStr(oldURL, 60), truncateStr(target.URL, 60), target.Kind)
	// Publish the new target: the card, the watchdog and the refresh loop must
	// see what the tab is being pointed at while it reloads.
	s.mu.Lock()
	s.MediaURL, s.MediaKind = target.URL, target.Kind
	s.mu.Unlock()
	// Media-only transition: "joining" while the tab reloads keeps the watchdog
	// from treating the reload as a stall and "recovering" it.
	s.setState(StateJoining, "switching media")

	err := s.applySwitch(target, stale)
	if stale() || errors.Is(err, errSwitchSuperseded) {
		// A newer switch owns the tab and the URL fields from here on. Leave its
		// work intact: no rollback, no state overwrite.
		log.Infof("[PIPELINE:switch] session %s: switch to %q superseded by a newer URL", s.ID, truncateStr(target.URL, 80))
		return nil
	}
	if err != nil {
		log.Errorf("[PIPELINE:switch] session %s: switch to %q failed: %v", s.ID, truncateStr(target.URL, 80), err)
		s.rollBackSwitch(oldURL, oldKind, prevState, err)
		return fmt.Errorf("switch media: %w", err)
	}

	// The switch landed. Preserve "paused": a paused stream must not start
	// playing just because its URL changed.
	s.mu.Lock()
	if s.switchRun != nil && s.localFile != nil {
		old := s.localFile
		s.localFile = nil
		s.mu.Unlock()
		s.freeLocalMediaEntry(old, "switch media completed (mocked)")
	} else {
		s.mu.Unlock()
	}

	next := prevState
	if next != StatePaused && next != StateStreaming {
		next = StateStreaming
	}
	s.setState(next, "media switched to "+truncateStr(target.URL, 60))
	if next == StatePaused && s.switchRun == nil {
		_, _ = s.mediaAction("pause", 0)
	}
	s.emit(EventState, "media switched to "+target.URL)
	log.Infof("[PIPELINE:switch] session %s: media switched (share untouched) %q → %q", s.ID, truncateStr(oldURL, 60), truncateStr(target.URL, 60))
	return nil
}

// applySwitch performs the browser-facing part of a switch: navigate the
// existing media tab, make the surface presentable, then start playback. It
// returns errSwitchSuperseded when a newer switch claimed the tab mid-way.
func (s *Session) applySwitch(target MediaTarget, stale func() bool) error {
	if run := s.switchRun; run != nil {
		return run(target, stale)
	}
	media := s.mediaTab()
	if media == nil {
		return errors.New("media page is not available")
	}

	navURL := target.URL
	navKind := target.Kind

	var newLocal *localMedia
	if s.playMode == mediateam.ModeDownload && mediateam.IsDownloadable(string(target.Kind)) {
		gen := s.switchGen.Load()
		dlID := fmt.Sprintf("ses-%s-g%d", s.ID, gen)
		mgr := s.mediaManager()
		if mgr != nil && mgr.Enabled() {
			res, err := s.runDownloadWithID(context.Background(), mgr, target, dlID)
			if err != nil {
				return fmt.Errorf("download switch media: %w", err)
			}
			if stale() {
				// Never adopted: no lease was taken, so Remove is the right
				// (unconditional) cleanup for this superseded download.
				s.freeUnleasedResult(res, "switch superseded")
				return errSwitchSuperseded
			}
			// The switch generation still owns the tab: lease the new file
			// before the session adopts it.
			s.leaseMedia(res)
			newLocal = &localMedia{
				id:        res.ID,
				url:       res.LocalURL,
				path:      res.Path,
				expiresAt: res.ExpiresAt,
				via:       res.Via,
				bytes:     res.Bytes,
			}
			navURL = res.LocalURL
			navKind = MediaDirect
		}
	}

	if err := s.navigateMediaTab(media, navURL, navKind); err != nil {
		if newLocal != nil {
			s.freeLocalMediaEntry(newLocal, "switch navigation failed")
		}
		return fmt.Errorf("navigate: %w", err)
	}
	if stale() {
		if newLocal != nil {
			s.freeLocalMediaEntry(newLocal, "switch superseded")
		}
		return errSwitchSuperseded
	}
	// Consent wall, viewport fit, buffered-video wait and the audio track: the
	// same preparation a fresh session performs. The share keeps running
	// throughout — this is the same tab Meet is capturing.
	if err := s.prepareMedia(media); err != nil {
		if newLocal != nil {
			s.freeLocalMediaEntry(newLocal, "switch preparation failed")
		}
		return fmt.Errorf("prepare: %w", err)
	}
	if stale() {
		if newLocal != nil {
			s.freeLocalMediaEntry(newLocal, "switch superseded")
		}
		return errSwitchSuperseded
	}
	if err := s.playMedia(); err != nil {
		if newLocal != nil {
			s.freeLocalMediaEntry(newLocal, "switch playback failed")
		}
		return fmt.Errorf("playback: %w", err)
	}

	s.mu.Lock()
	// The switch landed. The OLD local file is dropped from the session and
	// its lease released (force: this is the teardown of that ownership).
	// Its file vanishes unless another session still leases it.
	oldToFree := s.localFile
	s.localFile = newLocal
	s.mu.Unlock()

	if oldToFree != nil {
		s.freeLocalMediaEntry(oldToFree, "switch media completed")
	}

	return nil
}

// freeUnleasedResult cleans up a download result the session never adopted
// (and therefore never leased): a superseded switch download. Remove is
// unconditional because there is no lease to honour.
func (s *Session) freeUnleasedResult(res *downloadResult, why string) {
	if res == nil {
		return
	}
	mgr := s.mediaManager()
	if mgr == nil || !mgr.Enabled() {
		if res.Path != "" {
			_ = os.Remove(res.Path)
		}
		return
	}
	_ = mgr.Remove(res.ID)
	s.logger().Infof("[PIPELINE:session] session %s: freed unleased media %s (%s)", s.ID, res.ID, why)
}

// rollBackSwitch restores the previous media after a failed switch, keeping the
// share up: the same go-to-URL → prepare → play cycle, pointed at the old link.
func (s *Session) rollBackSwitch(oldURL string, oldKind MediaKind, prevState string, restoreErr error) {
	s.mu.Lock()
	s.MediaURL, s.MediaKind = oldURL, oldKind
	s.mu.Unlock()
	s.logger().Warnf("[PIPELINE:switch] session %s: restoring previous media %q after: %v", s.ID, truncateStr(oldURL, 60), restoreErr)

	// The rollback cannot be superseded (runSwitch only calls it while its
	// generation is current), so the stale predicate stays false.
	err := s.applySwitch(MediaTarget{URL: oldURL, Kind: oldKind}, func() bool { return false })
	if err == nil {
		next := prevState
		if next != StatePaused && next != StateStreaming {
			next = StateStreaming
		}
		s.setState(next, "restored previous media after a failed switch")
		return
	}
	s.logger().Errorf("[PIPELINE:switch] session %s: restore of %q failed: %v", s.ID, truncateStr(oldURL, 60), err)
}

// switchStateDetail is the detail line shown on the card while a switch is in
// flight; kept as a constant so the bot layer and the tests agree on it.
const switchStateDetail = "switching media"
