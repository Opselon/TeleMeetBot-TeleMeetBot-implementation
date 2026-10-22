// Package store keeps a small JSON history of streaming sessions so a restart
// can tell the operator what was interrupted.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one streaming session.
type Record struct {
	ID        string    `json:"id"`
	MeetURL   string    `json:"meet_url"`
	MediaURL  string    `json:"media_url"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	ChatID    int64     `json:"chat_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

// maxRecords bounds the history file.
const maxRecords = 50

// Store is a concurrency-safe session history persisted to a JSON file.
type Store struct {
	mu      sync.Mutex
	path    string
	records []Record
}

// Load reads the history; a missing or corrupt file yields an empty store.
func Load(path string) *Store {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var records []Record
	if err := json.Unmarshal(raw, &records); err != nil {
		// Keep the damaged file for forensics instead of silently deleting it.
		_ = os.Rename(path, fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix()))
		return s
	}
	s.records = records
	return s
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

// Update changes the state (and error) of an existing record.
func (s *Store) Update(id, state, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.records) - 1; i >= 0; i-- {
		if s.records[i].ID != id {
			continue
		}
		s.records[i].State = state
		if errMsg != "" {
			s.records[i].Error = errMsg
		}
		if state == "ended" || state == "error" || state == "interrupted" {
			s.records[i].EndedAt = time.Now()
		}
		_ = s.saveLocked()
		return
	}
}

// GeneratedAt returns the oldest record time, used for "history since" text.
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
	out, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
