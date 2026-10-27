package googleauth

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Logger is the minimal logging interface needed by googleauth.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Config controls the Google OAuth 2.0 / OpenID Connect engine.
type Config struct {
	ClientID           string
	ClientSecret       string
	RedirectURL        string
	TokenEncryptionKey string
	Scopes             []string
}

// UserProfile holds the persistent, authenticated Google profile for a Telegram user.
type UserProfile struct {
	UserID                int64     `json:"user_id"`
	GoogleSub             string    `json:"google_sub"`
	Email                 string    `json:"email"`
	EmailVerified         bool      `json:"email_verified"`
	Name                  string    `json:"name"`
	Picture               string    `json:"picture"`
	EncryptedAccessToken  string    `json:"encrypted_access_token"`
	EncryptedRefreshToken string    `json:"encrypted_refresh_token,omitempty"`
	EncryptedIDToken      string    `json:"encrypted_id_token,omitempty"`
	TokenExpiry           time.Time `json:"token_expiry"`
	Scopes                []string  `json:"scopes"`
	ConnectedAt           time.Time `json:"connected_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// AccountSummary is a safe, redacted summary of a user's Google connection status.
// Never exposes tokens or sensitive secrets.
type AccountSummary struct {
	Connected    bool      `json:"connected"`
	UserID       int64     `json:"user_id"`
	GoogleSub    string    `json:"google_sub,omitempty"`
	Email        string    `json:"email,omitempty"`
	Name         string    `json:"name,omitempty"`
	Picture      string    `json:"picture,omitempty"`
	ConnectedAt  time.Time `json:"connected_at,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	HasExpired   bool      `json:"has_expired"`
	OAuthEnabled bool      `json:"oauth_enabled"`
}

type pendingAuth struct {
	UserID       int64
	CodeVerifier string
	Nonce        string
	CreatedAt    time.Time
}

// Manager coordinates OAuth 2.0 / OpenID Connect authentication and user profiles.
type Manager struct {
	mu            sync.RWMutex
	baseDir       string
	cfg           Config
	log           Logger
	cipherKey     []byte
	stateSecret   []byte
	pendingMu     sync.Mutex
	pendingStates map[string]*pendingAuth
	httpClient    *http.Client

	// Endpoints can be overridden for unit testing.
	authURL     string
	tokenURL    string
	userInfoURL string
	revokeURL   string
}

// TokenRefresher is an interface for retrieving fresh access tokens.
type TokenRefresher interface {
	GetToken(ctx context.Context, userID int64) (string, error)
}
