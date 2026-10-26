package engine

// Advanced queue editing used by the Telegram management console.
//
// The basics (next/previous/jump/remove/clear) live in queue.go. This file adds
// the operations an operator needs while a playlist is already running:
// reorder neighbours, shuffle only the upcoming tail, sort alphabetically and
// insert "play next" items without touching what is currently on screen.

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// MoveQueueItem swaps an item with its neighbour in the queue. delta must be
// -1 (towards the start) or +1 (towards the end). Playback is not interrupted:
// only the order changes, and the current index follows the playing item if it
// was one of the two swapped rows.
func (s *Session) MoveQueueItem(id string, delta int) error {
	if delta != -1 && delta != 1 {
		return errors.New("move delta must be -1 or 1")
	}
	s.queueMu.Lock()
	defer s.queueMu.Unlock()

	pos := -1
	for i, it := range s.queue {
		if it.ID == id {
			pos = i
			break
		}
	}
	if pos < 0 {
		return ErrQueueItemNotFound
	}
	target := pos + delta
	if target < 0 {
		return errors.New("item is already first")
	}
	if target >= len(s.queue) {
		return errors.New("item is already last")
	}
	s.queue[pos], s.queue[target] = s.queue[target], s.queue[pos]
	switch s.queueIdx {
	case pos:
		s.queueIdx = target
	case target:
		s.queueIdx = pos
	}
	s.logger().Infof("[PIPELINE:queue] session %s: moved item %s to position %d", s.ID, id, target+1)
	s.emit(EventState, "queue reordered")
	return nil
}

// ShuffleUpcoming randomizes the order of every item after the current one.
// The playing item and anything already played stay untouched, so a shuffle
// during playback never causes an audible cut.
func (s *Session) ShuffleUpcoming() int {
	s.queueMu.Lock()
	start := s.queueIdx + 1
	if start < 0 {
		start = 0
	}
	if start >= len(s.queue) {
		s.queueMu.Unlock()
		return 0
	}
	tail := s.queue[start:]
	rand.Shuffle(len(tail), func(i, j int) {
		tail[i], tail[j] = tail[j], tail[i]
	})
	n := len(tail)
	s.queueMu.Unlock()

	s.logger().Infof("[PIPELINE:queue] session %s: shuffled %d upcoming items", s.ID, n)
	s.emit(EventState, fmt.Sprintf("shuffled %d upcoming items", n))
	return n
}

// SortUpcoming orders the upcoming tail by title (case-insensitive) and then
// by URL, which gives a stable result for equal titles.
func (s *Session) SortUpcoming() int {
	s.queueMu.Lock()
	start := s.queueIdx + 1
	if start < 0 {
		start = 0
	}
	if start >= len(s.queue) {
		s.queueMu.Unlock()
		return 0
	}
	tail := s.queue[start:]
	sort.SliceStable(tail, func(i, j int) bool {
		ti := strings.ToLower(strings.TrimSpace(tail[i].Title))
		tj := strings.ToLower(strings.TrimSpace(tail[j].Title))
		if ti != tj {
			return ti < tj
		}
		return tail[i].URL < tail[j].URL
	})
	n := len(tail)
	s.queueMu.Unlock()

	s.logger().Infof("[PIPELINE:queue] session %s: sorted %d upcoming items by title", s.ID, n)
	s.emit(EventState, fmt.Sprintf("sorted %d upcoming items", n))
	return n
}

// EnqueueNext inserts media directly after the currently playing item, the
// classic "play next" gesture. When nothing is playing it behaves like
// Enqueue: the items are appended and playback starts at the first one.
func (s *Session) EnqueueNext(urls ...string) ([]QueueItem, error) {
	var targets []MediaTarget
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		target, err := ClassifyMedia(u)
		if err != nil {
			continue
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, errors.New("no valid media URLs to enqueue")
	}

	s.queueMu.Lock()
	insertAt := s.queueIdx + 1
	startPlayback := len(s.queue) == 0 || s.queueIdx < 0
	if insertAt < 0 {
		insertAt = 0
	}
	if insertAt > len(s.queue) {
		insertAt = len(s.queue)
	}
	added := make([]QueueItem, 0, len(targets))
	newItems := make([]QueueItem, 0, len(targets))
	for _, target := range targets {
		item := QueueItem{
			ID:      fmt.Sprintf("item-%d", s.nextQueueID),
			URL:     target.URL,
			Title:   itemTitle(target.URL),
			Kind:    target.Kind,
			AddedAt: time.Now(),
		}
		s.nextQueueID++
		newItems = append(newItems, item)
		added = append(added, item)
	}
	head := append([]QueueItem{}, s.queue[:insertAt]...)
	tail := append([]QueueItem{}, s.queue[insertAt:]...)
	s.queue = append(append(head, newItems...), tail...)
	if startPlayback {
		s.queueIdx = 0
	}
	total := len(s.queue)
	s.queueMu.Unlock()

	for _, it := range added {
		s.leaseQueueLocal(it.URL)
	}
	s.logger().Infof("[PIPELINE:queue] session %s: inserted %d items after current (total: %d)", s.ID, len(added), total)
	if startPlayback && len(added) > 0 {
		go func() { _ = s.SwitchMedia(added[0].URL) }()
	}
	s.emit(EventState, fmt.Sprintf("queued %d items to play next", len(added)))
	return added, nil
}

// QueueURLs returns the queue in playback order. The snapshot is copied, so
// the caller can hand it to a playlist import without holding the lock.
func (s *Session) QueueURLs() []string {
	items, _ := s.QueueSnapshot()
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.URL)
	}
	return out
}

// QueueTotalDuration sums the known item durations. Items without metadata
// contribute zero, so this is a lower bound until probes fill durations in.
func (s *Session) QueueTotalDuration() float64 {
	items, _ := s.QueueSnapshot()
	var total float64
	for _, it := range items {
		if it.Duration > 0 {
			total += it.Duration
		}
	}
	return total
}

// QueueItemByID returns one item and its position.
func (s *Session) QueueItemByID(id string) (QueueItem, int, bool) {
	items, _ := s.QueueSnapshot()
	for i, it := range items {
		if it.ID == id {
			return it, i, true
		}
	}
	return QueueItem{}, -1, false
}

// ReplaceUpcoming swaps everything after the current item for urls. It is the
// "load playlist from here" operation: the currently playing media keeps
// playing while the rest of the queue is rebuilt.
func (s *Session) ReplaceUpcoming(urls []string) (int, error) {
	var targets []MediaTarget
	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		target, err := ClassifyMedia(raw)
		if err != nil {
			continue
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return 0, errors.New("no valid media URLs to add")
	}

	s.queueMu.Lock()
	keep := s.queueIdx + 1
	if keep < 0 {
		keep = 0
	}
	if keep > len(s.queue) {
		keep = len(s.queue)
	}
	var dropped []QueueItem
	if keep < len(s.queue) {
		dropped = append(dropped, s.queue[keep:]...)
	}
	next := make([]QueueItem, 0, keep+len(targets))
	next = append(next, s.queue[:keep]...)
	for _, target := range targets {
		next = append(next, QueueItem{
			ID:      fmt.Sprintf("item-%d", s.nextQueueID),
			URL:     target.URL,
			Title:   itemTitle(target.URL),
			Kind:    target.Kind,
			AddedAt: time.Now(),
		})
		s.nextQueueID++
	}
	s.queue = next
	if s.queueIdx < 0 {
		s.queueIdx = 0
	}
	s.queueMu.Unlock()

	for _, it := range dropped {
		s.releaseQueueLocal(it.URL)
	}
	s.logger().Infof("[PIPELINE:queue] session %s: replaced upcoming items with %d new items", s.ID, len(targets))
	s.emit(EventState, fmt.Sprintf("loaded %d items into the queue", len(targets)))
	return len(targets), nil
}
