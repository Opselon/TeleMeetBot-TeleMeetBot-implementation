// Package store keeps a small JSON history of streaming sessions so a restart
// can tell the operator what was interrupted, alongside per-user preferences
// (such as YouTube audio language preference).
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAudioLang is Persian (fa) as mandated.
const DefaultAudioLang = "fa"

// Valid audio language preference codes.
const (
	AudioLangPersian  = "fa"
	AudioLangEnglish  = "en"
	AudioLangOriginal = "orig"
	AudioLangAuto     = "auto"
)

// DefaultMediaMode is the default playback mode: Browser (the original
// behaviour), so an upgrade changes nothing for existing users.
const DefaultMediaMode = "browser"

// NormalizeMediaMode normalizes a Download & Stream mode, falling back to
// Browser for anything unknown or empty.
func NormalizeMediaMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "download", "download-stream", "download_stream", "downloadandstream", "local":
		return "download"
	case "auto", "fallback", "smart":
		return "auto"
	case "browser", "direct", "play", "off", "none", "no":
		return "browser"
	default:
		return DefaultMediaMode
	}
}

// GetUserMediaMode returns the user's preferred media mode (Browser default).
func (s *Store) GetUserMediaMode(userID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pref, ok := s.prefs[userID]; ok && pref.MediaMode != "" {
		return NormalizeMediaMode(pref.MediaMode)
	}
	return DefaultMediaMode
}

// SetUserMediaMode sets and persists the user's preferred media mode.
func (s *Store) SetUserMediaMode(userID int64, mode string) error {
	normalized := NormalizeMediaMode(mode)
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.prefs[userID]
	if current.MediaMode == normalized {
		return nil
	}
	s.prefs[userID] = UserPreference{AudioLang: current.AudioLang, MediaMode: normalized}
	return s.saveLocked()
}

// NormalizeAudioLang normalizes language input, falling back to DefaultAudioLang.
func NormalizeAudioLang(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "fa", "persian", "farsi", "فارسی":
		return AudioLangPersian
	case "en", "english", "eng":
		return AudioLangEnglish
	case "orig", "original", "source":
		return AudioLangOriginal
	case "auto", "best":
		return AudioLangAuto
	default:
		return AudioLangPersian
	}
}

// UserPreference stores persistent settings for one Telegram user.
type UserPreference struct {
	AudioLang string `json:"audio_lang"`
	MediaMode string `json:"media_mode,omitempty"`
}

// QueueItemRecord represents one entry in a session's persisted playlist.
type QueueItemRecord struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// Record is one streaming session.
type Record struct {
	ID        string            `json:"id"`
	MeetURL   string            `json:"meet_url"`
	MediaURL  string            `json:"media_url"`
	Title     string            `json:"title"`
	State     string            `json:"state"`
	Error     string            `json:"error,omitempty"`
	ChatID    int64             `json:"chat_id,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	EndedAt   time.Time         `json:"ended_at,omitempty"`
	Queue     []QueueItemRecord `json:"queue,omitempty"`
	QueueIdx  int               `json:"queue_idx,omitempty"`
}

// maxRecords bounds the history file.
const maxRecords = 50

type persistedPayload struct {
	Records []Record                  `json:"records"`
	Prefs   map[string]UserPreference `json:"user_prefs,omitempty"`
}

// Store is a concurrency-safe session history persisted to a JSON file.
type Store struct {
	mu      sync.Mutex
	path    string
	records []Record
	prefs   map[int64]UserPreference
	// writes counts atomic file replacements; tests assert that no-op updates
	// do not touch the disk.
	writes int
}

// Load reads the history and preferences; a missing or corrupt file yields an empty store.
func Load(path string) *Store {
	s := &Store{
		path:  path,
		prefs: make(map[int64]UserPreference),
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return s
	}

	// Case 1: Legacy format where state.json is a JSON array of []Record
	if trimmed[0] == '[' {
		var records []Record
		if err := json.Unmarshal(trimmed, &records); err == nil {
			s.records = records
			return s
		}
	}

	// Case 2: Object format { "records": [...], "user_prefs": {...} }
	var payload persistedPayload
	if err := json.Unmarshal(trimmed, &payload); err == nil {
		s.records = payload.Records
		if payload.Prefs != nil {
			for k, v := range payload.Prefs {
				if uid, err := strconv.ParseInt(k, 10, 64); err == nil {
					s.prefs[uid] = v
				}
			}
		}
		return s
	}

	// Case 3: Damaged file
	_ = os.Rename(path, fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix()))
	return s
}

// AudioLang returns the user's preferred YouTube audio language, defaulting to "fa" (Persian).
func (s *Store) AudioLang(userID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pref, ok := s.prefs[userID]; ok && pref.AudioLang != "" {
		return pref.AudioLang
	}
	return DefaultAudioLang
}

// SetAudioLang updates and persists the user's audio language preference.
func (s *Store) SetAudioLang(userID int64, lang string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalized := NormalizeAudioLang(lang)
	current := s.prefs[userID]
	if current.AudioLang == normalized {
		return nil
	}
	s.prefs[userID] = UserPreference{AudioLang: normalized}
	return s.saveLocked()
}

// GetUserAudioLang returns the user's preferred YouTube audio language.
func (s *Store) GetUserAudioLang(userID int64) string {
	return s.AudioLang(userID)
}

// SetUserAudioLang sets and persists the user's preferred YouTube audio language.
func (s *Store) SetUserAudioLang(userID int64, lang string) error {
	return s.SetAudioLang(userID, lang)
}

// Interrupted marks records that were still running when the process died.
// It returns the ones that were interrupted.
func (s *Store) Interrupted() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for i := range s.records {
		switch s.records[i].State {
		case "starting", "joining", "streaming", "paused", "sharing":
			s.records[i].State = "interrupted"
			s.records[i].EndedAt = time.Now()
			out = append(out, s.records[i])
		}
	}
	if len(out) > 0 {
		_ = s.saveLocked()
	}
	return out
}

// Add records a new session.
func (s *Store) Add(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	s.records = append(s.records, r)
	if len(s.records) > maxRecords {
		s.records = s.records[len(s.records)-maxRecords:]
	}
	_ = s.saveLocked()
}

// Update changes the state (and error) of an existing record. A no-op update
// (same state, no new error) does not rewrite the file: progress events repeat
// the current state, and rewriting the history for every one of them was pure
// disk churn during a join/present flow.
func (s *Store) Update(id, state, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].ID != id {
			continue
		}
		terminal := state == "ended" || state == "error" || state == "interrupted"
		if s.records[i].State == state && (errMsg == "" || s.records[i].Error == errMsg) {
			// Terminal states also stamp EndedAt once; only skip when that is
			// already recorded.
			if !terminal || !s.records[i].EndedAt.IsZero() {
				return
			}
		}
		s.records[i].State = state
		if errMsg != "" {
			s.records[i].Error = errMsg
		}
		if terminal {
			s.records[i].EndedAt = time.Now()
		}
		_ = s.saveLocked()
		return
	}
}

// UpdateQueue records playlist changes and playback position for a session.
func (s *Store) UpdateQueue(id string, queue []QueueItemRecord, idx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].ID == id {
			s.records[i].Queue = queue
			s.records[i].QueueIdx = idx
			_ = s.saveLocked()
			return
		}
	}
}

// GetRecord returns a copy of a session record, or nil if not found.
func (s *Store) GetRecord(id string) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.ID == id {
			cp := r
			return &cp
		}
	}
	return nil
}

// Records returns a copy of stored records.
func (s *Store) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.records))
	copy(out, s.records)
	return out
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	payload := persistedPayload{
		Records: s.records,
		Prefs:   make(map[string]UserPreference, len(s.prefs)),
	}
	for uid, pref := range s.prefs {
		payload.Prefs[strconv.FormatInt(uid, 10)] = pref
	}

	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.writes++
	return nil
}
