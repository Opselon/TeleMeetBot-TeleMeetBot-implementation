package engine

// Queue end-of-media behaviour: repeat modes, shuffle and auto-advance.
//
// The watch loop calls queueEndAction when the player reports the current
// media finished, and this file owns the decision. The semantics are:
//
//   - auto-advance off: playback stops at the end of the current item;
//   - repeat one: the current item restarts;
//   - repeat all: Next when an upcoming item exists, otherwise the queue
//     wraps back to the first item;
//   - repeat off: Next while an upcoming item exists, otherwise stop;
//   - shuffle on (with advance): a random upcoming item plays next — the
//     played prefix and the current item are never re-picked until the tail
//     is exhausted, at which point repeat-all wraps to the queue start.
//
// The decision is a pure function of (repeat, shuffle, autoplay, cursor,
// length) so tests can pin every branch without a browser.

import (
	"errors"
	"fmt"
	"math/rand"
)

// Repeat modes for session playback.
const (
	RepeatOff = "off"
	RepeatOne = "one"
	RepeatAll = "all"
)

// NormalizeRepeatMode validates a repeat mode, falling back to off.
func NormalizeRepeatMode(s string) string {
	switch s {
	case RepeatOne, RepeatAll:
		return s
	default:
		return RepeatOff
	}
}

// queueEndAction is the decision taken when the current media ends.
type queueEndAction int

const (
	queueEndStop queueEndAction = iota
	queueEndNext
	queueEndWrap
	queueEndRestart
)

// decideQueueEnd maps the playback settings to an action. It is a pure
// function: autoplay false always stops, repeat one restarts, repeat all
// advances or wraps, and plain advance stops at the end of the queue.
func decideQueueEnd(autoplay bool, repeat string, cursor, length int) queueEndAction {
	if !autoplay || length == 0 {
		return queueEndStop
	}
	switch repeat {
	case RepeatOne:
		return queueEndRestart
	case RepeatAll:
		if cursor+1 < length {
			return queueEndNext
		}
		return queueEndWrap
	default:
		if cursor+1 < length {
			return queueEndNext
		}
		return queueEndStop
	}
}

// queueEndActionLocked returns the action for this session's current state.
// Callers must hold queueMu.
func (s *Session) queueEndActionLocked() queueEndAction {
	return decideQueueEnd(s.autoAdvance, s.repeatMode, s.queueIdx, len(s.queue))
}

// SetRepeatMode sets what happens when the current media ends: "off" plays
// the queue once, "one" restarts the current item, "all" loops the whole
// queue. The mode applies immediately, including to a running session.
func (s *Session) SetRepeatMode(mode string) error {
	normalized := NormalizeRepeatMode(mode)
	s.queueMu.Lock()
	s.repeatMode = normalized
	s.queueMu.Unlock()
	s.emit(EventState, "repeat mode: "+normalized)
	return nil
}

// RepeatMode returns the session's repeat mode. A session constructed
// without one (tests, older call sites) reads as "off".
func (s *Session) RepeatMode() string {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	return NormalizeRepeatMode(s.repeatMode)
}

// SetShuffle toggles random upcoming-item picking when playback auto-advances
// (and for the queueEnd shuffle pick). The already-played prefix and the
// current item are never re-picked until the tail is exhausted.
func (s *Session) SetShuffle(on bool) {
	s.queueMu.Lock()
	s.shuffle = on
	s.queueMu.Unlock()
	s.emit(EventState, "shuffle "+boolOnOff(on))
}

// Shuffle reports whether shuffled auto-advance is on.
func (s *Session) Shuffle() bool {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	return s.shuffle
}

// SetAutoAdvance toggles playing the next item when the current media ends.
func (s *Session) SetAutoAdvance(on bool) {
	s.queueMu.Lock()
	s.autoAdvance = on
	s.queueMu.Unlock()
	s.emit(EventState, "auto-advance "+boolOnOff(on))
}

// AutoAdvance reports whether auto-advance is on.
func (s *Session) AutoAdvance() bool {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	return s.autoAdvance
}

func boolOnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// pickShuffledNext returns the index of the upcoming item to play when
// shuffle is on. It never touches the played prefix or the current item, and
// returns -1 when there is nothing upcoming. rng may be nil (falls back to
// the package-level generator).
func (s *Session) pickShuffledNext(cursor, length int, rng *rand.Rand) int {
	start := cursor + 1
	if start < 0 {
		start = 0
	}
	if start >= length {
		return -1
	}
	n := length - start
	var pick int
	if rng != nil {
		pick = rng.Intn(n)
	} else {
		pick = rand.Intn(n)
	}
	return start + pick
}

// advanceShuffled plays one random upcoming item. The cursor follows the
// played item, exactly like JumpTo. It returns an error when nothing is
// upcoming or the switch failed; the queue is never modified on failure.
func (s *Session) advanceShuffled() error {
	s.queueMu.Lock()
	idx := s.pickShuffledNext(s.queueIdx, len(s.queue), s.rng)
	if idx < 0 {
		s.queueMu.Unlock()
		return ErrQueueEnd
	}
	s.queueIdx = idx
	target := s.queue[idx]
	curIdx := s.queueIdx
	total := len(s.queue)
	s.queueMu.Unlock()

	s.logger().Infof("[PIPELINE:queue] session %s: shuffle advanced to item %d/%d (%s)", s.ID, curIdx+1, total, target.Title)
	if err := s.SwitchMedia(target.URL); err != nil {
		return fmt.Errorf("shuffle advance to item %d: %w", curIdx+1, err)
	}
	s.promoteQueueLease(target.URL)
	s.emit(EventState, fmt.Sprintf("shuffle playing item %d/%d", curIdx+1, total))
	return nil
}

// handleQueueEnded runs the end-of-media decision and starts the chosen
// operation. It returns the action taken, for logging and tests.
func (s *Session) handleQueueEnded() queueEndAction {
	s.queueMu.RLock()
	action := s.queueEndActionLocked()
	shuffle := s.shuffle
	s.queueMu.RUnlock()

	switch action {
	case queueEndNext:
		s.setState(StateJoining, "advancing queue")
		if shuffle {
			go func() {
				if err := s.advanceShuffled(); err != nil && !errors.Is(err, ErrQueueEnd) {
					s.log.Errorf("[PIPELINE:queue] session %s: shuffle advance failed: %v", s.ID, err)
				}
			}()
		} else {
			go func() {
				if err := s.Next(); err != nil && !errors.Is(err, ErrQueueEnd) {
					s.log.Errorf("[PIPELINE:queue] session %s: auto-advance failed: %v", s.ID, err)
				}
			}()
		}
	case queueEndWrap:
		s.setState(StateJoining, "repeat all: restarting queue")
		go func() {
			if err := s.JumpTo(0); err != nil {
				s.log.Errorf("[PIPELINE:queue] session %s: repeat-all wrap failed: %v", s.ID, err)
			}
		}()
	case queueEndRestart:
		go func() {
			if err := s.RestartMedia(); err != nil {
				s.log.Errorf("[PIPELINE:queue] session %s: repeat-one restart failed: %v", s.ID, err)
			}
		}()
	default:
		s.setDetail("playback completed (end of queue)")
	}
	return action
}
