package googleauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultTokenURL    = "https://oauth2.googleapis.com/token"
	defaultUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	defaultRevokeURL   = "https://oauth2.googleapis.com/revoke"
	stateTTL           = 10 * time.Minute
)

// New creates and initializes a Google OAuth 2.0 / OpenID Connect Manager.
func New(baseDir string, cfg Config, log Logger) (*Manager, error) {
	cipherKey, err := initCipherKey(baseDir, cfg.TokenEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("initialize token cipher key: %w", err)
	}

	stateSecret, err := generateRandomBytes(32)
	if err != nil {
		return nil, fmt.Errorf("initialize state secret: %w", err)
	}

	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{
			"openid",
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
		}
	}

	return &Manager{
		baseDir:       baseDir,
		cfg:           cfg,
		log:           log,
		cipherKey:     cipherKey,
		stateSecret:   stateSecret,
		pendingStates: make(map[string]*pendingAuth),
		httpClient:    &http.Client{Timeout: 15 * time.Second},
		authURL:       defaultAuthURL,
		tokenURL:      defaultTokenURL,
		userInfoURL:   defaultUserInfoURL,
		revokeURL:     defaultRevokeURL,
	}, nil
}

// Config returns the active Google OAuth configuration.
func (m *Manager) Config() Config {
	return m.cfg
}

// AuthURL generates the official Google OAuth 2.0 authorization URL with PKCE and signed state.
func (m *Manager) AuthURL(userID int64) (string, error) {
	if userID <= 0 {
		return "", errors.New("invalid user id")
	}
	if strings.TrimSpace(m.cfg.ClientID) == "" {
		return "", errors.New("google oauth client_id is not configured")
	}
	if strings.TrimSpace(m.cfg.RedirectURL) == "" {
		return "", errors.New("google oauth redirect_url is not configured")
	}

	verifier, challenge, err := generatePKCE()
	if err != nil {
		return "", fmt.Errorf("generate PKCE: %w", err)
	}

	nonce, err := generateNonce()
	if err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	rawStateBytes, err := generateRandomBytes(16)
	if err != nil {
		return "", fmt.Errorf("generate state token: %w", err)
	}
	stateToken := base64.RawURLEncoding.EncodeToString(rawStateBytes)

	now := time.Now()
	expiry := now.Add(stateTTL)
	signedState := signState(m.stateSecret, userID, stateToken, expiry)

	m.pendingMu.Lock()
	// Garbage collect stale states
	for k, v := range m.pendingStates {
		if now.Sub(v.CreatedAt) > stateTTL {
			delete(m.pendingStates, k)
		}
	}
	m.pendingStates[stateToken] = &pendingAuth{
		UserID:       userID,
		CodeVerifier: verifier,
		Nonce:        nonce,
		CreatedAt:    now,
	}
	m.pendingMu.Unlock()

	params := url.Values{}
	params.Set("client_id", m.cfg.ClientID)
	params.Set("redirect_uri", m.cfg.RedirectURL)
	params.Set("response_type", "code")
	params.Set("scope", strings.Join(m.cfg.Scopes, " "))
	params.Set("state", signedState)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("nonce", nonce)
	params.Set("access_type", "offline")
	params.Set("prompt", "consent")

	return m.authURL + "?" + params.Encode(), nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type idTokenClaims struct {
	Iss           string `json:"iss"`
	Sub           string `json:"sub"`
	Aud           string `json:"aud"`
	Exp           int64  `json:"exp"`
	Iat           int64  `json:"iat"`
	Nonce         string `json:"nonce"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

func parseAndValidateIDToken(rawIDToken, expectedClientID, expectedNonce string) (*idTokenClaims, error) {
	parts := strings.Split(rawIDToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed id_token: expected 3 parts")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode id_token payload: %w", err)
	}

	var claims idTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("unmarshal id_token claims: %w", err)
	}

	// Validate Issuer
	if claims.Iss != "https://accounts.google.com" && claims.Iss != "accounts.google.com" {
		return nil, fmt.Errorf("invalid id_token issuer: %s", claims.Iss)
	}

	// Validate Audience
	if claims.Aud != expectedClientID {
		return nil, fmt.Errorf("invalid id_token audience: %s (expected %s)", claims.Aud, expectedClientID)
	}

	// Validate Expiration (allow 60s clock skew)
	if time.Now().Unix() > claims.Exp+60 {
		return nil, errors.New("id_token has expired")
	}

	// Validate Nonce
	if expectedNonce != "" && claims.Nonce != expectedNonce {
		return nil, errors.New("id_token nonce mismatch: possible replay attack")
	}

	if claims.Sub == "" {
		return nil, errors.New("id_token missing subject (sub)")
	}

	return &claims, nil
}

// HandleCallback handles the Google OAuth redirect callback, verifies state and PKCE,
// exchanges code for tokens, verifies the OpenID Connect ID token, and persists the user profile.
func (m *Manager) HandleCallback(ctx context.Context, rawState, code, errParam string) (*UserProfile, error) {
	if errParam != "" {
		return nil, fmt.Errorf("google authentication failed: %s", errParam)
	}
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("missing authorization code")
	}

	userID, stateToken, err := verifyState(m.stateSecret, rawState)
	if err != nil {
		return nil, fmt.Errorf("invalid oauth state: %w", err)
	}

	m.pendingMu.Lock()
	pending, found := m.pendingStates[stateToken]
	if found {
		delete(m.pendingStates, stateToken) // Single use
	}
	m.pendingMu.Unlock()

	if !found || pending == nil {
		return nil, errors.New("oauth session expired or already used (CSRF protection)")
	}

	if pending.UserID != userID {
		return nil, errors.New("user identity mismatch in oauth callback")
	}

	// Exchange Authorization Code + PKCE code_verifier for tokens
	tokenVals := url.Values{}
	tokenVals.Set("grant_type", "authorization_code")
	tokenVals.Set("code", code)
	tokenVals.Set("code_verifier", pending.CodeVerifier)
	tokenVals.Set("client_id", m.cfg.ClientID)
	tokenVals.Set("client_secret", m.cfg.ClientSecret)
	tokenVals.Set("redirect_uri", m.cfg.RedirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, strings.NewReader(tokenVals.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange code for tokens: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	var tokResp tokenResponse
	if err := json.Unmarshal(body, &tokResp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	if tokResp.Error != "" {
		return nil, fmt.Errorf("token error %s: %s", tokResp.Error, tokResp.ErrorDesc)
	}

	if tokResp.AccessToken == "" {
		return nil, errors.New("token response missing access_token")
	}

	// Verify ID Token
	claims, err := parseAndValidateIDToken(tokResp.IDToken, m.cfg.ClientID, pending.Nonce)
	if err != nil {
		return nil, fmt.Errorf("validate id_token: %w", err)
	}

	// If Name or Email missing from ID token, fetch from userinfo endpoint
	if claims.Name == "" || claims.Email == "" {
		m.fetchUserInfo(ctx, tokResp.AccessToken, claims)
	}

	// Encrypt tokens at rest bound to user ID and Google Sub
	aad := fmt.Sprintf("telemeetbot:user:%d:sub:%s", userID, claims.Sub)

	encAccess, err := encryptToken(m.cipherKey, tokResp.AccessToken, aad)
	if err != nil {
		return nil, fmt.Errorf("encrypt access token: %w", err)
	}

	encRefresh, err := encryptToken(m.cipherKey, tokResp.RefreshToken, aad)
	if err != nil {
		return nil, fmt.Errorf("encrypt refresh token: %w", err)
	}

	encID, err := encryptToken(m.cipherKey, tokResp.IDToken, aad)
	if err != nil {
		return nil, fmt.Errorf("encrypt id token: %w", err)
	}

	// If no new refresh token returned, preserve existing refresh token if available
	if tokResp.RefreshToken == "" {
		if existing, _ := m.GetProfile(userID); existing != nil && existing.GoogleSub == claims.Sub {
			encRefresh = existing.EncryptedRefreshToken
		}
	}

	expiry := time.Now().Add(time.Duration(tokResp.ExpiresIn) * time.Second)
	if tokResp.ExpiresIn <= 0 {
		expiry = time.Now().Add(1 * time.Hour)
	}

	scopes := m.cfg.Scopes
	if tokResp.Scope != "" {
		scopes = strings.Split(tokResp.Scope, " ")
	}

	prof := UserProfile{
		UserID:                userID,
		GoogleSub:             claims.Sub,
		Email:                 claims.Email,
		EmailVerified:         claims.EmailVerified,
		Name:                  claims.Name,
		Picture:               claims.Picture,
		EncryptedAccessToken:  encAccess,
		EncryptedRefreshToken: encRefresh,
		EncryptedIDToken:      encID,
		TokenExpiry:           expiry,
		Scopes:                scopes,
		ConnectedAt:           time.Now(),
		UpdatedAt:             time.Now(),
	}

	if err := m.SaveProfile(&prof); err != nil {
		return nil, fmt.Errorf("save user profile: %w", err)
	}

	if m.log != nil {
		m.log.Infof("[PIPELINE:googleauth] user %d successfully connected Google account: %s (%s)", userID, prof.Name, prof.Email)
	}

	return &prof, nil
}

func (m *Manager) fetchUserInfo(ctx context.Context, accessToken string, claims *idTokenClaims) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.userInfoURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := m.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	defer resp.Body.Close()

	var info struct {
		Name          string `json:"name"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Picture       string `json:"picture"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err == nil {
		if claims.Name == "" {
			claims.Name = info.Name
		}
		if claims.Email == "" {
			claims.Email = info.Email
			claims.EmailVerified = info.EmailVerified
		}
		if claims.Picture == "" {
			claims.Picture = info.Picture
		}
	}
}

// RefreshToken exchanges the stored refresh token for a fresh access token.
func (m *Manager) RefreshToken(ctx context.Context, userID int64) error {
	prof, err := m.GetProfile(userID)
	if err != nil {
		return err
	}
	if prof == nil {
		return errors.New("no user profile found")
	}

	aad := fmt.Sprintf("telemeetbot:user:%d:sub:%s", userID, prof.GoogleSub)
	refreshToken, err := decryptToken(m.cipherKey, prof.EncryptedRefreshToken, aad)
	if err != nil || refreshToken == "" {
		return errors.New("no valid refresh token stored; user must re-authenticate")
	}

	vals := url.Values{}
	vals.Set("grant_type", "refresh_token")
	vals.Set("refresh_token", refreshToken)
	vals.Set("client_id", m.cfg.ClientID)
	vals.Set("client_secret", m.cfg.ClientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, strings.NewReader(vals.Encode()))
	if err != nil {
		return fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("refresh token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("refresh token failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokResp tokenResponse
	if err := json.Unmarshal(body, &tokResp); err != nil {
		return fmt.Errorf("parse refresh token response: %w", err)
	}

	if tokResp.AccessToken == "" {
		return errors.New("refresh response missing access_token")
	}

	encAccess, err := encryptToken(m.cipherKey, tokResp.AccessToken, aad)
	if err != nil {
		return fmt.Errorf("encrypt refreshed access token: %w", err)
	}

	prof.EncryptedAccessToken = encAccess
	if tokResp.RefreshToken != "" {
		if encRef, err := encryptToken(m.cipherKey, tokResp.RefreshToken, aad); err == nil {
			prof.EncryptedRefreshToken = encRef
		}
	}

	prof.TokenExpiry = time.Now().Add(time.Duration(tokResp.ExpiresIn) * time.Second)
	prof.UpdatedAt = time.Now()

	if err := m.SaveProfile(prof); err != nil {
		return fmt.Errorf("save refreshed profile: %w", err)
	}

	if m.log != nil {
		m.log.Infof("[PIPELINE:googleauth] user %d: refreshed Google OAuth access token", userID)
	}

	return nil
}

// Revoke revokes the user's OAuth tokens with Google and removes the profile from disk.
func (m *Manager) Revoke(ctx context.Context, userID int64) error {
	prof, err := m.GetProfile(userID)
	if err != nil {
		return err
	}
	if prof == nil {
		return nil
	}

	aad := fmt.Sprintf("telemeetbot:user:%d:sub:%s", userID, prof.GoogleSub)

	// Attempt revocation with Google
	tokenToRevoke, _ := decryptToken(m.cipherKey, prof.EncryptedRefreshToken, aad)
	if tokenToRevoke == "" {
		tokenToRevoke, _ = decryptToken(m.cipherKey, prof.EncryptedAccessToken, aad)
	}

	if tokenToRevoke != "" {
		vals := url.Values{}
		vals.Set("token", tokenToRevoke)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.revokeURL, strings.NewReader(vals.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := m.httpClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}
	}

	return m.DeleteProfile(userID)
}

// GetToken returns a valid decrypted access token for userID, refreshing it if needed.
func (m *Manager) GetToken(ctx context.Context, userID int64) (string, error) {
	prof, err := m.GetProfile(userID)
	if err != nil {
		return "", err
	}
	if prof == nil {
		return "", errors.New("no user profile found")
	}

	// If expired or within 2 minutes of expiration, refresh
	if time.Now().Add(2 * time.Minute).After(prof.TokenExpiry) {
		if err := m.RefreshToken(ctx, userID); err != nil {
			if m.log != nil {
				m.log.Warnf("[PIPELINE:googleauth] user %d: refresh token failed: %v", userID, err)
			}
		} else {
			prof, _ = m.GetProfile(userID)
		}
	}

	aad := fmt.Sprintf("telemeetbot:user:%d:sub:%s", userID, prof.GoogleSub)
	return decryptToken(m.cipherKey, prof.EncryptedAccessToken, aad)
}
