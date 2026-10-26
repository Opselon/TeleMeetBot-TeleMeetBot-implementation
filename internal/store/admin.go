package store

// Administrative views over the session history store. These helpers exist so
// the Telegram management console can answer "who has used this bot?", "what
// preferences are saved?" and "can I reset one user?" without reaching into
// the store's internals.

import (
	"sort"
	"time"
)

// AdminStats is a compact snapshot for the management overview.
type AdminStats struct {
	Records     int
	Active      int
	Preferences int
	Users       int
	Oldest      time.Time
	Newest      time.Time
}

// Stats returns read-only counters over the persisted history.
func (s *Store) Stats() AdminStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := AdminStats{
		Records:     len(s.records),
		Preferences: len(s.prefs),
	}
	seen := make(map[int64]struct{}, len(s.prefs)+len(s.records))
	for uid := range s.prefs {
		seen[uid] = struct{}{}
	}
	for _, r := range s.records {
		if r.ChatID > 0 {
			seen[r.ChatID] = struct{}{}
		}
		if IsActiveState(r.State) {
			st.Active++
		}
		if !r.CreatedAt.IsZero() && (st.Oldest.IsZero() || r.CreatedAt.Before(st.Oldest)) {
			st.Oldest = r.CreatedAt
		}
		if r.CreatedAt.After(st.Newest) {
			st.Newest = r.CreatedAt
		}
	}
	st.Users = len(seen)
	return st
}

// IsActiveState reports whether a persisted state still implies a running
// session. It mirrors the set Interrupted uses, kept in one place so the
// console and the startup recovery cannot disagree.
func IsActiveState(state string) bool {
	switch state {
	case "starting", "joining", "streaming", "paused", "sharing", "queued", "waiting_for_admission", "admitting", "admitted":
		return true
	default:
		return false
	}
}

// Preferences returns a copy of every saved per-user preference keyed by
// Telegram user id.
func (s *Store) Preferences() map[int64]UserPreference {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]UserPreference, len(s.prefs))
	for id, pref := range s.prefs {
		out[id] = pref
	}
	return out
}

// UserPreference returns one user's saved preferences and whether any row
// exists. A zero value with false means the user still uses defaults.
func (s *Store) UserPreference(userID int64) (UserPreference, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pref, ok := s.prefs[userID]
	return pref, ok
}

// ResetUser removes every saved preference for one user. The user falls back
// to the configured defaults. It returns true when a row was removed.
func (s *Store) ResetUser(userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.prefs[userID]; !ok {
		return false
	}
	delete(s.prefs, userID)
	_ = s.saveLocked()
	return true
}

// ResetAllPreferences removes every saved user preference. It returns the
// number of users reset. Session history is left alone.
func (s *Store) ResetAllPreferences() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.prefs)
	if n == 0 {
		return 0
	}
	s.prefs = make(map[int64]UserPreference)
	_ = s.saveLocked()
	return n
}

// ClearHistory removes the persisted session records but keeps per-user
// preferences. It returns the number of records removed.
func (s *Store) ClearHistory() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.records)
	if n == 0 {
		return 0
	}
	s.records = nil
	_ = s.saveLocked()
	return n
}

// DeleteRecord removes one history record by id.
func (s *Store) DeleteRecord(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].ID != id {
			continue
		}
		s.records = append(s.records[:i], s.records[i+1:]...)
		_ = s.saveLocked()
		return true
	}
	return false
}

// KnownUserIDs returns the union of preference owners, session chat ids and
// meeting/playlist-style ids supplied by the caller, sorted ascending. The
// store cannot see other databases, so callers merge those in.
func (s *Store) KnownUserIDs(extra ...int64) []int64 {
	s.mu.Lock()
	seen := make(map[int64]struct{}, len(s.prefs)+len(s.records)+len(extra))
	for id := range s.prefs {
		seen[id] = struct{}{}
	}
	for _, r := range s.records {
		if r.ChatID > 0 {
			seen[r.ChatID] = struct{}{}
		}
	}
	s.mu.Unlock()
	for _, id := range extra {
		if id > 0 {
			seen[id] = struct{}{}
		}
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// HistoryForUser returns the newest records whose chat id matches userID,
// bounded by limit (0 means no bound).
func (s *Store) HistoryForUser(userID int64, limit int) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].ChatID != userID {
			continue
		}
		out = append(out, s.records[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
