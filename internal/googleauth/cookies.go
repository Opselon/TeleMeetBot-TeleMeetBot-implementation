package googleauth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// Logger is the minimal logging interface needed by googleauth.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Cookie represents a single parsed HTTP cookie.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires,omitempty"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
	SameSite string  `json:"sameSite,omitempty"`
}

// UserProfile stores the persistent Google authentication cookies for one user.
type UserProfile struct {
	UserID    int64     `json:"user_id"`
	UpdatedAt time.Time `json:"updated_at"`
	Cookies   []Cookie  `json:"cookies"`
}

// AccountSummary is a safe summary of a user's Google authentication state.
type AccountSummary struct {
	Connected   bool      `json:"connected"`
	CookieCount int       `json:"cookie_count"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	HasExpired  bool      `json:"has_expired"`
}

// Manager coordinates per-user Google account profiles.
type Manager struct {
	mu      sync.RWMutex
	baseDir string
	log     Logger
}

// New creates a Google authentication profile manager rooted at baseDir.
func New(baseDir string, log Logger) *Manager {
	return &Manager{
		baseDir: baseDir,
		log:     log,
	}
}

// googleAuthCookieNames lists standard cookies that identify a Google session.
var googleAuthCookieNames = map[string]bool{
	"SID":               true,
	"HSID":              true,
	"SSID":              true,
	"APISID":            true,
	"SAPISID":           true,
	"__Secure-1PSID":    true,
	"__Secure-3PSID":    true,
	"__Secure-1PAPISID": true,
	"__Secure-3PAPISID": true,
	"LOGIN_INFO":        true,
	"OSID":              true,
	"__Secure-OSID":     true,
}

// isGoogleDomain checks if the host is under Google or YouTube.
func isGoogleDomain(domain string) bool {
	d := strings.ToLower(strings.TrimPrefix(domain, "."))
	return d == "google.com" || strings.HasSuffix(d, ".google.com") ||
		d == "youtube.com" || strings.HasSuffix(d, ".youtube.com") ||
		d == "googleusercontent.com" || strings.HasSuffix(d, ".googleusercontent.com") ||
		d == "gstatic.com" || strings.HasSuffix(d, ".gstatic.com")
}

// ParseCookies parses cookies from either Netscape format or JSON format.
func ParseCookies(raw []byte) ([]Cookie, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("empty cookie data")
	}

	var parsed []Cookie
	var err error
	if trimmed[0] == '[' || trimmed[0] == '{' {
		parsed, err = parseJSONCookies(trimmed)
	} else {
		parsed, err = parseNetscapeCookies(trimmed)
	}
	if err != nil {
		return nil, err
	}

	// Filter for Google/YouTube domains and validate session identity.
	var validGoogle []Cookie
	hasAuthMarker := false
	for _, c := range parsed {
		if !isGoogleDomain(c.Domain) {
			continue
		}
		if googleAuthCookieNames[c.Name] && strings.TrimSpace(c.Value) != "" {
			hasAuthMarker = true
		}
		validGoogle = append(validGoogle, c)
	}

	if len(validGoogle) == 0 {
		return nil, errors.New("no Google or YouTube cookies found in file")
	}
	if !hasAuthMarker {
		return nil, errors.New("no valid Google session markers found (missing SID, SAPISID, or __Secure-1PSID); make sure you are logged in when exporting")
	}

	return validGoogle, nil
}

func parseNetscapeCookies(raw []byte) ([]Cookie, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	var cookies []Cookie

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		httpOnly := false
		if strings.HasPrefix(line, "#HttpOnly_") {
			httpOnly = true
			line = strings.TrimPrefix(line, "#HttpOnly_")
		} else if strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			// Some exporters use multiple spaces instead of tabs.
			parts = strings.Fields(line)
			if len(parts) < 7 {
				continue
			}
		}

		domain := parts[0]
		path := parts[2]
		secure := strings.EqualFold(parts[3], "true")
		expires, _ := strconv.ParseFloat(parts[4], 64)
		name := parts[5]
		val := parts[6]
		if len(parts) > 7 {
			val = strings.Join(parts[6:], " ")
		}

		if name == "" {
			continue
		}

		cookies = append(cookies, Cookie{
			Name:     name,
			Value:    val,
			Domain:   domain,
			Path:     path,
			Expires:  expires,
			Secure:   secure,
			HTTPOnly: httpOnly,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read cookie lines: %w", err)
	}
	if len(cookies) == 0 {
		return nil, errors.New("no cookie entries found in Netscape format")
	}
	return cookies, nil
}

func parseJSONCookies(raw []byte) ([]Cookie, error) {
	type rawJSONCookie struct {
		Name           string  `json:"name"`
		Value          string  `json:"value"`
		Domain         string  `json:"domain"`
		Path           string  `json:"path"`
		Secure         bool    `json:"secure"`
		HTTPOnly       bool    `json:"httpOnly"`
		SameSite       string  `json:"sameSite"`
		ExpirationDate float64 `json:"expirationDate"`
		Expires        float64 `json:"expires"`
	}

	var list []rawJSONCookie
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("invalid cookie JSON array: %w", err)
		}
	} else {
		var wrapper struct {
			Cookies []rawJSONCookie `json:"cookies"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return nil, fmt.Errorf("invalid cookie JSON object: %w", err)
		}
		list = wrapper.Cookies
	}

	var cookies []Cookie
	for _, r := range list {
		if r.Name == "" {
			continue
		}
		exp := r.ExpirationDate
		if exp <= 0 {
			exp = r.Expires
		}
		p := r.Path
		if p == "" {
			p = "/"
		}
		cookies = append(cookies, Cookie{
			Name:     r.Name,
			Value:    r.Value,
			Domain:   r.Domain,
			Path:     p,
			Expires:  exp,
			Secure:   r.Secure,
			HTTPOnly: r.HTTPOnly,
			SameSite: r.SameSite,
		})
	}

	if len(cookies) == 0 {
		return nil, errors.New("no cookie entries found in JSON")
	}
	return cookies, nil
}

func (m *Manager) profilePath(userID int64) string {
	if userID <= 0 {
		return ""
	}
	return filepath.Join(m.baseDir, strconv.FormatInt(userID, 10), "cookies.json")
}

// Save parses raw cookie data, validates Google session markers, and stores it for userID.
func (m *Manager) Save(userID int64, raw []byte) (*AccountSummary, error) {
	if userID <= 0 {
		return nil, errors.New("invalid user id")
	}

	cookies, err := ParseCookies(raw)
	if err != nil {
		return nil, err
	}

	path := m.profilePath(userID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create user profile dir: %w", err)
	}

	prof := UserProfile{
		UserID:    userID,
		UpdatedAt: time.Now(),
		Cookies:   cookies,
	}

	data, err := json.MarshalIndent(prof, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal user profile: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Atomic write
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, fmt.Errorf("write user cookies: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("commit user cookies: %w", err)
	}

	if m.log != nil {
		m.log.Infof("[PIPELINE:googleauth] user %d: saved %d Google cookies", userID, len(cookies))
	}

	s := summaryFromProfile(prof)
	return &s, nil
}

// Get loads the stored UserProfile for userID, if any.
func (m *Manager) Get(userID int64) (*UserProfile, error) {
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
		return nil, fmt.Errorf("corrupt user profile %s: %w", path, err)
	}
	return &prof, nil
}

// Delete removes any saved profile for userID.
func (m *Manager) Delete(userID int64) error {
	if userID <= 0 {
		return errors.New("invalid user id")
	}

	path := m.profilePath(userID)
	m.mu.Lock()
	defer m.mu.Unlock()

	dir := filepath.Dir(path)
	_ = os.Remove(path)
	_ = os.Remove(dir)

	if m.log != nil {
		m.log.Infof("[PIPELINE:googleauth] user %d: deleted Google account profile", userID)
	}
	return nil
}

// Summary returns the current account status for userID.
func (m *Manager) Summary(userID int64) AccountSummary {
	prof, err := m.Get(userID)
	if err != nil || prof == nil || len(prof.Cookies) == 0 {
		return AccountSummary{Connected: false}
	}
	return summaryFromProfile(*prof)
}

func summaryFromProfile(prof UserProfile) AccountSummary {
	var maxExp float64
	for _, c := range prof.Cookies {
		if c.Expires > maxExp {
			maxExp = c.Expires
		}
	}

	var expTime time.Time
	if maxExp > 0 {
		expTime = time.Unix(int64(maxExp), 0)
	}

	expired := false
	if !expTime.IsZero() && time.Now().After(expTime) {
		expired = true
	}

	return AccountSummary{
		Connected:   true,
		CookieCount: len(prof.Cookies),
		UpdatedAt:   prof.UpdatedAt,
		ExpiresAt:   expTime,
		HasExpired:  expired,
	}
}

// GetCookieParams loads and converts the user's cookies to Rod CDP proto.NetworkCookieParam items.
func (m *Manager) GetCookieParams(userID int64) ([]*proto.NetworkCookieParam, error) {
	prof, err := m.Get(userID)
	if err != nil {
		return nil, err
	}
	if prof == nil || len(prof.Cookies) == 0 {
		return nil, nil
	}

	var out []*proto.NetworkCookieParam
	for _, c := range prof.Cookies {
		param := &proto.NetworkCookieParam{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			HTTPOnly: c.HTTPOnly,
		}
		if c.Expires > 0 {
			param.Expires = proto.TimeSinceEpoch(c.Expires)
		}
		switch strings.ToLower(c.SameSite) {
		case "strict":
			param.SameSite = proto.NetworkCookieSameSiteStrict
		case "lax":
			param.SameSite = proto.NetworkCookieSameSiteLax
		case "none", "no_restriction":
			param.SameSite = proto.NetworkCookieSameSiteNone
		}
		out = append(out, param)
	}
	return out, nil
}
