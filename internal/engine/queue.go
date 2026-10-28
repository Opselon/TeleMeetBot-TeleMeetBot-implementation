package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrQueueEnd is returned when Next is called at the end of the playlist.
	ErrQueueEnd = errors.New("already at end of queue")
	// ErrQueueStart is returned when Previous is called at the beginning of the playlist.
	ErrQueueStart = errors.New("already at start of queue")
	// ErrQueueEmpty is returned when an operation requires items in the queue.
	ErrQueueEmpty = errors.New("queue is empty")
	// ErrQueueItemNotFound is returned when an item cannot be found in the queue.
	ErrQueueItemNotFound = errors.New("queue item not found")
)

// QueueItem represents an item in a playback session's playlist.
type QueueItem struct {
	ID       string    `json:"id"`
	URL      string    `json:"url"`
	Title    string    `json:"title,omitempty"`
	Kind     MediaKind `json:"kind"`
	Duration float64   `json:"duration,omitempty"`
	AddedAt  time.Time `json:"added_at"`
	// Quality is the per-item quality override ("" = session policy).
	Quality string `json:"quality,omitempty"`
	// CaptionLang is the per-item subtitle language override ("" = session
	// policy; "off" forces none for this item).
	CaptionLang string `json:"caption_lang,omitempty"`
}

// queueOwner returns the lease owner tag for items pinned in this session's queue.
func (s *Session) queueOwner() string {
	return fmt.Sprintf("session:%s:queue", s.ID)
}

// InitQueue initializes the session playlist with the initial media link and any
// additional queued items supplied at launch. It also restores the production
// defaults for the end-of-media behaviour (auto-advance on, repeat off), so a
// session built outside Manager.Start still behaves like a live one.
func (s *Session) InitQueue(initialURL string, extraQueue []string) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()

	s.queue = make([]QueueItem, 0, 1+len(extraQueue))
	s.queueIdx = -1
	s.nextQueueID = 1
	if s.repeatMode == "" {
		s.repeatMode = RepeatOff
	}
	if s.defaultQuality == "" {
		s.defaultQuality = QualityAuto
	}
	if s.qualityPolicy == "" {
		s.qualityPolicy = QualityAuto
	}
	if s.captionPolicy == "" {
		s.captionPolicy = CaptionOff
	}
	if s.defaultCaptionPolicy == "" {
		s.defaultCaptionPolicy = s.captionPolicy
	}
	if s.defaultCaptionLang == "" {
		s.defaultCaptionLang = s.captionLang
	}
	s.autoAdvance = true

	if strings.TrimSpace(initialURL) != "" {
		target, err := ClassifyMedia(initialURL)
		if err == nil {
			item := QueueItem{
				ID:      fmt.Sprintf("item-%d", s.nextQueueID),
				URL:     target.URL,
				Title:   itemTitle(target.URL),
				Kind:    target.Kind,
				AddedAt: time.Now(),
			}
			s.nextQueueID++
			s.queue = append(s.queue, item)
			s.queueIdx = 0
			s.leaseQueueLocal(target.URL)
		}
	}

	for _, raw := range extraQueue {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		target, err := ClassifyMedia(raw)
		if err != nil {
			continue
		}
		item := QueueItem{
			ID:      fmt.Sprintf("item-%d", s.nextQueueID),
			URL:     target.URL,
			Title:   itemTitle(target.URL),
			Kind:    target.Kind,
			AddedAt: time.Now(),
		}
		s.nextQueueID++
		s.queue = append(s.queue, item)
		s.leaseQueueLocal(target.URL)
	}

	if s.queueIdx == -1 && len(s.queue) > 0 {
		s.queueIdx = 0
	}
}

// leaseQueueLocal pins a managed local file to this session's queue owner.
func (s *Session) leaseQueueLocal(rawURL string) {
	mgr := s.mediaManager()
	if mgr == nil || !mgr.Enabled() {
		return
	}
	if entry, ok := mgr.FindByURL(rawURL); ok {
		mgr.Lease(entry.ID, s.queueOwner())
	}
}

// releaseQueueLocal drops the queue lease for a managed local file.
func (s *Session) releaseQueueLocal(rawURL string) {
	mgr := s.mediaManager()
	if mgr == nil || !mgr.Enabled() {
		return
	}
	if entry, ok := mgr.FindByURL(rawURL); ok {
		mgr.ReleaseAndDelete(entry.ID, s.queueOwner())
	}
}

// promoteQueueLease transfers a managed local file's lease from queueOwner
// to this session's active playback lease.
func (s *Session) promoteQueueLease(rawURL string) {
	mgr := s.mediaManager()
	if mgr == nil || !mgr.Enabled() {
		return
	}
	if entry, ok := mgr.FindByURL(rawURL); ok {
		mgr.Lease(entry.ID, s.leaseOwner())
		mgr.Release(entry.ID, s.queueOwner())
	}
}

// itemTitle extracts a short display title from a media URL or filename.
func itemTitle(raw string) string {
	raw = strings.TrimSpace(raw)
	if idx := strings.LastIndex(raw, "/"); idx >= 0 && idx+1 < len(raw) {
		name := raw[idx+1:]
		if q := strings.Index(name, "?"); q >= 0 {
			name = name[:q]
		}
		if name != "" {
			return name
		}
	}
	return truncateStr(raw, 40)
}

// Enqueue adds one or more media URLs to the end of the session queue in submission order.
// If the session has no current media playing, it switches to the first newly enqueued item.
func (s *Session) Enqueue(urls ...string) ([]QueueItem, error) {
	var validTargets []MediaTarget
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		target, err := ClassifyMedia(u)
		if err != nil {
			continue
		}
		validTargets = append(validTargets, target)
	}

	if len(validTargets) == 0 {
		return nil, errors.New("no valid media URLs to enqueue")
	}

	s.queueMu.Lock()
	var added []QueueItem
	startPlayback := (s.queueIdx == -1 || len(s.queue) == 0)

	for _, target := range validTargets {
		item := QueueItem{
			ID:      fmt.Sprintf("item-%d", s.nextQueueID),
			URL:     target.URL,
			Title:   itemTitle(target.URL),
			Kind:    target.Kind,
			AddedAt: time.Now(),
		}
		s.nextQueueID++
		s.queue = append(s.queue, item)
		added = append(added, item)
		s.leaseQueueLocal(target.URL)
	}

	if startPlayback && len(s.queue) > 0 {
		s.queueIdx = 0
	}
	total := len(s.queue)
	s.queueMu.Unlock()

	s.logger().Infof("[PIPELINE:queue] session %s: enqueued %d items (total: %d)", s.ID, len(added), total)

	if startPlayback && len(added) > 0 {
		go func() {
			_ = s.SwitchMedia(added[0].URL)
		}()
	}

	s.emit(EventState, fmt.Sprintf("enqueued %d items", len(added)))
	return added, nil
}

// Next advances to the next item in the queue.
func (s *Session) Next() error {
	s.queueMu.Lock()
	if len(s.queue) == 0 {
		s.queueMu.Unlock()
		return ErrQueueEmpty
	}
	if s.queueIdx+1 >= len(s.queue) {
		s.queueMu.Unlock()
		return ErrQueueEnd
	}
	s.queueIdx++
	target := s.queue[s.queueIdx]
	curIdx := s.queueIdx
	total := len(s.queue)
	s.applyItemQualityLocked(target)
	s.applyItemCaptionLocked(target)
	s.queueMu.Unlock()

	s.logger().Infof("[PIPELINE:queue] session %s: advancing to next item %d/%d (%s)", s.ID, curIdx+1, total, target.Title)

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		err := s.SwitchMedia(target.URL)
		if err == nil {
			s.promoteQueueLease(target.URL)
			s.emit(EventState, fmt.Sprintf("playing item %d/%d", curIdx+1, total))
			return nil
		}
		lastErr = err
		s.logger().Warnf("[PIPELINE:queue] session %s: play attempt %d for %q failed: %v", s.ID, attempt, target.URL, err)
		time.Sleep(500 * time.Millisecond)
	}

	s.emit(EventError, fmt.Sprintf("Failed to play queue item %d (%s): %v", curIdx+1, target.Title, lastErr))
	return fmt.Errorf("play queue item %d: %w", curIdx+1, lastErr)
}

// Previous advances to the previous item in the queue.
func (s *Session) Previous() error {
	s.queueMu.Lock()
	if len(s.queue) == 0 {
		s.queueMu.Unlock()
		return ErrQueueEmpty
	}
	if s.queueIdx <= 0 {
		s.queueMu.Unlock()
		return ErrQueueStart
	}
	s.queueIdx--
	target := s.queue[s.queueIdx]
	curIdx := s.queueIdx
	total := len(s.queue)
	s.applyItemQualityLocked(target)
	s.applyItemCaptionLocked(target)
	s.queueMu.Unlock()

	s.logger().Infof("[PIPELINE:queue] session %s: returning to previous item %d/%d (%s)", s.ID, curIdx+1, total, target.Title)

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		err := s.SwitchMedia(target.URL)
		if err == nil {
			s.promoteQueueLease(target.URL)
			s.emit(EventState, fmt.Sprintf("playing item %d/%d", curIdx+1, total))
			return nil
		}
		lastErr = err
		s.logger().Warnf("[PIPELINE:queue] session %s: play attempt %d for %q failed: %v", s.ID, attempt, target.URL, err)
		time.Sleep(500 * time.Millisecond)
	}

	s.emit(EventError, fmt.Sprintf("Failed to return to queue item %d (%s): %v", curIdx+1, target.Title, lastErr))
	return fmt.Errorf("play queue item %d: %w", curIdx+1, lastErr)
}

// JumpTo switches playback to the specified 0-based index in the queue.
func (s *Session) JumpTo(idx int) error {
	s.queueMu.Lock()
	if idx < 0 || idx >= len(s.queue) {
		s.queueMu.Unlock()
		return fmt.Errorf("queue index %d out of bounds (0..%d)", idx, len(s.queue)-1)
	}
	s.queueIdx = idx
	target := s.queue[idx]
	curIdx := s.queueIdx
	total := len(s.queue)
	s.applyItemQualityLocked(target)
	s.applyItemCaptionLocked(target)
	s.queueMu.Unlock()

	s.logger().Infof("[PIPELINE:queue] session %s: jumping to item %d/%d (%s)", s.ID, curIdx+1, total, target.Title)
	if err := s.SwitchMedia(target.URL); err != nil {
		return fmt.Errorf("jump to item %d: %w", curIdx+1, err)
	}
	s.promoteQueueLease(target.URL)
	s.emit(EventState, fmt.Sprintf("jumped to item %d/%d", curIdx+1, total))
	return nil
}

// RemoveQueueItem removes an item from the queue by its ID.
func (s *Session) RemoveQueueItem(id string) error {
	s.queueMu.Lock()
	found := -1
	for i, it := range s.queue {
		if it.ID == id {
			found = i
			break
		}
	}
	if found < 0 {
		s.queueMu.Unlock()
		return ErrQueueItemNotFound
	}

	target := s.queue[found]
	// Remove from slice
	s.queue = append(s.queue[:found], s.queue[found+1:]...)

	// Release lease on removed local file
	s.releaseQueueLocal(target.URL)

	isCurrent := (found == s.queueIdx)
	if found < s.queueIdx {
		s.queueIdx--
	} else if isCurrent {
		if s.queueIdx >= len(s.queue) {
			s.queueIdx = len(s.queue) - 1
		}
	}
	curIdx := s.queueIdx
	s.queueMu.Unlock()

	s.logger().Infof("[PIPELINE:queue] session %s: removed item %s (%s)", s.ID, id, target.Title)

	// If the current playing item was removed, advance to the new current item
	if isCurrent && curIdx >= 0 && curIdx < len(s.queue) {
		s.queueMu.RLock()
		nextURL := s.queue[curIdx].URL
		s.queueMu.RUnlock()
		go func() {
			_ = s.SwitchMedia(nextURL)
		}()
	}

	s.emit(EventState, "queue item removed")
	return nil
}

// ClearUpcoming removes all items after the currently playing item from the queue.
func (s *Session) ClearUpcoming() error {
	s.queueMu.Lock()
	if len(s.queue) == 0 {
		s.queueMu.Unlock()
		return nil
	}

	var upcoming []QueueItem
	if s.queueIdx < 0 {
		if len(s.queue) > 1 {
			upcoming = s.queue[1:]
			s.queue = s.queue[:1]
		}
	} else if s.queueIdx+1 < len(s.queue) {
		upcoming = s.queue[s.queueIdx+1:]
		s.queue = s.queue[:s.queueIdx+1]
	}
	s.queueMu.Unlock()

	for _, it := range upcoming {
		s.releaseQueueLocal(it.URL)
	}

	s.logger().Infof("[PIPELINE:queue] session %s: cleared %d upcoming queue items", s.ID, len(upcoming))
	s.emit(EventState, "upcoming queue cleared")
	return nil
}

// ClearAllExceptCurrent keeps only the currently playing item and discards all others.
func (s *Session) ClearAllExceptCurrent() error {
	s.queueMu.Lock()
	if len(s.queue) <= 1 {
		s.queueMu.Unlock()
		return nil
	}

	var removed []QueueItem
	curIdx := s.queueIdx
	if curIdx >= 0 && curIdx < len(s.queue) {
		current := s.queue[curIdx]
		for i, it := range s.queue {
			if i != curIdx {
				removed = append(removed, it)
			}
		}
		s.queue = []QueueItem{current}
		s.queueIdx = 0
	} else {
		removed = s.queue[1:]
		s.queue = s.queue[:1]
		s.queueIdx = 0
	}
	s.queueMu.Unlock()

	for _, it := range removed {
		s.releaseQueueLocal(it.URL)
	}

	s.logger().Infof("[PIPELINE:queue] session %s: cleared %d items (kept active track)", s.ID, len(removed))
	s.emit(EventState, "playlist cleared (kept active track)")
	return nil
}

// ClearEverything completely clears the playlist queue and releases all leases.
func (s *Session) ClearEverything() error {
	s.queueMu.Lock()
	items := s.queue
	s.queue = nil
	s.queueIdx = -1
	s.queueMu.Unlock()

	for _, it := range items {
		s.releaseQueueLocal(it.URL)
	}

	s.logger().Infof("[PIPELINE:queue] session %s: completely cleared %d queue items", s.ID, len(items))
	s.emit(EventState, "playlist completely cleared")
	return nil
}

// QueueSnapshot returns a copy of the current queue items and current playing index.
func (s *Session) QueueSnapshot() ([]QueueItem, int) {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()

	copied := make([]QueueItem, len(s.queue))
	copy(copied, s.queue)
	return copied, s.queueIdx
}

// hasNextInQueue checks if there is another item available to play.
func (s *Session) hasNextInQueue() bool {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	return s.queueIdx >= 0 && s.queueIdx+1 < len(s.queue)
}

// hasPrevInQueue checks if there is a previous item in the playlist.
func (s *Session) hasPrevInQueue() bool {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	return s.queueIdx > 0
}

// cleanupQueue releases queue leases on all remaining items when the session ends.
func (s *Session) cleanupQueue() {
	s.queueMu.Lock()
	items := s.queue
	s.queue = nil
	s.queueIdx = -1
	s.queueMu.Unlock()

	for _, it := range items {
		s.releaseQueueLocal(it.URL)
	}
}
