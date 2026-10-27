package googleauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func (m *Manager) profilePath(userID int64) string {
	if userID <= 0 {
		return ""
	}
	return filepath.Join(m.baseDir, strconv.FormatInt(userID, 10), "google_profile.json")
}

// SaveProfile persists a UserProfile to disk with strict 0600 permissions.
func (m *Manager) SaveProfile(prof *UserProfile) error {
	if prof == nil || prof.UserID <= 0 {
		return errors.New("invalid user profile")
	}

	path := m.profilePath(prof.UserID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create user profile dir: %w", err)
	}

	data, err := json.MarshalIndent(prof, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal user profile: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Atomic file write
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write user profile: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit user profile: %w", err)
	}

	// Clean up legacy cookies.json if present
	_ = os.Remove(filepath.Join(filepath.Dir(path), "cookies.json"))

	return nil
}

// GetProfile loads the stored UserProfile for userID.
func (m *Manager) GetProfile(userID int64) (*UserProfile, error) {
	if userID <= 0 {
		return nil, errors.New("invalid user id")
	}

	path := m.profilePath(userID)
	m.mu.RLock()
	defer m.mu.RUnlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read user profile: %w", err)
	}

	var prof UserProfile
	if err := json.Unmarshal(data, &prof); err != nil {
		return nil, fmt.Errorf("corrupt user profile: %w", err)
	}
	return &prof, nil
}

// DeleteProfile removes any saved Google profile for userID.
func (m *Manager) DeleteProfile(userID int64) error {
	if userID <= 0 {
		return errors.New("invalid user id")
	}

	path := m.profilePath(userID)
	m.mu.Lock()
	defer m.mu.Unlock()

	dir := filepath.Dir(path)
	_ = os.Remove(path)
	_ = os.Remove(filepath.Join(dir, "cookies.json"))
	_ = os.Remove(dir)

	if m.log != nil {
		m.log.Infof("[PIPELINE:googleauth] user %d: deleted Google profile", userID)
	}
	return nil
}

// Summary returns a safe snapshot of a user's Google connection status.
func (m *Manager) Summary(userID int64) AccountSummary {
	sum := AccountSummary{
		Connected:    false,
		UserID:       userID,
		OAuthEnabled: m.cfg.ClientID != "",
	}

	prof, err := m.GetProfile(userID)
	if err != nil || prof == nil || prof.GoogleSub == "" {
		return sum
	}

	sum.Connected = true
	sum.GoogleSub = prof.GoogleSub
	sum.Email = prof.Email
	sum.Name = prof.Name
	sum.Picture = prof.Picture
	sum.ConnectedAt = prof.ConnectedAt
	sum.ExpiresAt = prof.TokenExpiry

	if !prof.TokenExpiry.IsZero() && time.Now().After(prof.TokenExpiry) {
		sum.HasExpired = true
	}

	return sum
}
