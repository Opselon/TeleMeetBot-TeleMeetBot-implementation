package googleauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPKCEGeneration(t *testing.T) {
	v, c, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE error: %v", err)
	}
	if len(v) < 43 {
		t.Fatalf("verifier too short: %d", len(v))
	}
	h := sha256.Sum256([]byte(v))
	expectedC := base64.RawURLEncoding.EncodeToString(h[:])
	if c != expectedC {
		t.Fatalf("challenge mismatch: got %s, want %s", c, expectedC)
	}
}

func TestStateSigningAndVerification(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	userID := int64(5094837833)
	stateToken := "test-state-token-1234"
	exp := time.Now().Add(5 * time.Minute)

	signed := signState(secret, userID, stateToken, exp)
	if !strings.Contains(signed, ".") {
		t.Fatalf("expected signed state to have separator: %s", signed)
	}

	gotUID, gotToken, err := verifyState(secret, signed)
	if err != nil {
		t.Fatalf("verifyState failed: %v", err)
	}
	if gotUID != userID {
		t.Fatalf("userID mismatch: got %d, want %d", gotUID, userID)
	}
	if gotToken != stateToken {
		t.Fatalf("stateToken mismatch: got %s, want %s", gotToken, stateToken)
	}

	// Test tampering payload
	parts := strings.Split(signed, ".")
	tampered := "dGVzdA." + parts[1]
	if _, _, err := verifyState(secret, tampered); err == nil {
		t.Fatalf("expected error for tampered payload")
	}

	// Test tampering signature
	tamperedSig := parts[0] + ".0000000000000000000000000000000000000000000000000000000000000000"
	if _, _, err := verifyState(secret, tamperedSig); err == nil {
		t.Fatalf("expected error for tampered signature")
	}

	// Test expired state
	expiredState := signState(secret, userID, stateToken, time.Now().Add(-1*time.Minute))
	if _, _, err := verifyState(secret, expiredState); err == nil {
		t.Fatalf("expected error for expired state")
	}

	// Test different secret
	diffSecret := []byte("diffdiffdiffdiffdiffdiffdiffdiff")
	if _, _, err := verifyState(diffSecret, signed); err == nil {
		t.Fatalf("expected error when verifying with wrong secret")
	}
}

func TestTokenEncryptionRoundTrip(t *testing.T) {
	key := []byte("12345678901234567890123456789012")
	plaintext := "ya29.a0AfH6SM...google-access-token"
	aad := "telemeetbot:user:5094837833:sub:1029384756"

	encrypted, err := encryptToken(key, plaintext, aad)
	if err != nil {
		t.Fatalf("encryptToken failed: %v", err)
	}
	if encrypted == plaintext {
		t.Fatalf("encrypted matches plaintext")
	}

	decrypted, err := decryptToken(key, encrypted, aad)
	if err != nil {
		t.Fatalf("decryptToken failed: %v", err)
	}
	if decrypted != plaintext {
		t.Fatalf("decrypted mismatch: got %s, want %s", decrypted, plaintext)
	}

	// Cross-tenant attack: decrypt with different AAD (e.g. attacker's user ID)
	attackerAAD := "telemeetbot:user:9999999999:sub:1029384756"
	if _, err := decryptToken(key, encrypted, attackerAAD); err == nil {
		t.Fatalf("decryptToken MUST fail when AAD does not match (cross-tenant isolation)")
	}
}

func makeFakeIDToken(iss, sub, aud, nonce, name, email string, exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := idTokenClaims{
		Iss:           iss,
		Sub:           sub,
		Aud:           aud,
		Exp:           exp.Unix(),
		Iat:           time.Now().Unix(),
		Nonce:         nonce,
		Email:         email,
		EmailVerified: true,
		Name:          name,
	}
	payloadBytes, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fake-rs256-signature"))
	return fmt.Sprintf("%s.%s.%s", header, payload, sig)
}

func TestAuthFlow_SuccessAndReplayProtection(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "googleauth-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	clientID := "test-client-id.apps.googleusercontent.com"
	clientSecret := "test-client-secret"
	redirectURL := "http://127.0.0.1:18080/auth/google/callback"

	var nonceUsed string

	// Mock Google OAuth server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "authorization_code" {
				if r.Form.Get("code") != "valid-auth-code" {
					http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
					return
				}
				if r.Form.Get("code_verifier") == "" {
					http.Error(w, `{"error":"missing_verifier"}`, http.StatusBadRequest)
					return
				}
				idTok := makeFakeIDToken("https://accounts.google.com", "sub-123456", clientID, nonceUsed, "Test User", "test@gmail.com", time.Now().Add(1*time.Hour))
				resp := tokenResponse{
					AccessToken:  "mock-access-token-xyz",
					RefreshToken: "mock-refresh-token-abc",
					IDToken:      idTok,
					ExpiresIn:    3600,
					TokenType:    "Bearer",
					Scope:        "openid email profile",
				}
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
			if r.Form.Get("grant_type") == "refresh_token" {
				if r.Form.Get("refresh_token") != "mock-refresh-token-abc" {
					http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
					return
				}
				resp := tokenResponse{
					AccessToken: "mock-new-access-token-789",
					ExpiresIn:   3600,
					TokenType:   "Bearer",
				}
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
			http.Error(w, `{"error":"unsupported_grant"}`, http.StatusBadRequest)
		case "/revoke":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockServer.Close()

	mgr, err := New(tmpDir, Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
	}, nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	mgr.tokenURL = mockServer.URL + "/token"
	mgr.revokeURL = mockServer.URL + "/revoke"

	userID := int64(5094837833)

	// Step 1: Generate Auth URL
	authURLStr, err := mgr.AuthURL(userID)
	if err != nil {
		t.Fatalf("AuthURL failed: %v", err)
	}
	u, err := url.Parse(authURLStr)
	if err != nil {
		t.Fatalf("parse authURL: %v", err)
	}
	q := u.Query()
	stateParam := q.Get("state")
	nonceUsed = q.Get("nonce")
	if stateParam == "" || nonceUsed == "" {
		t.Fatalf("missing state or nonce in authURL")
	}

	// Step 2: Handle Callback
	ctx := context.Background()
	prof, err := mgr.HandleCallback(ctx, stateParam, "valid-auth-code", "")
	if err != nil {
		t.Fatalf("HandleCallback failed: %v", err)
	}
	if prof.Email != "test@gmail.com" || prof.Name != "Test User" {
		t.Fatalf("profile info mismatch: %+v", prof)
	}

	// Verify Summary
	sum := mgr.Summary(userID)
	if !sum.Connected || sum.Email != "test@gmail.com" {
		t.Fatalf("summary unexpected: %+v", sum)
	}

	// Step 3: Replay / CSRF defense test
	// Re-submitting the exact same state must fail because it is single-use
	if _, err := mgr.HandleCallback(ctx, stateParam, "valid-auth-code", ""); err == nil {
		t.Fatalf("expected replay of state to fail")
	}

	// Step 4: Token Retrieval and Decryption
	tok, err := mgr.GetToken(ctx, userID)
	if err != nil {
		t.Fatalf("GetToken failed: %v", err)
	}
	if tok != "mock-access-token-xyz" {
		t.Fatalf("GetToken mismatch: got %s", tok)
	}

	// Step 5: Refresh Token Flow
	if err := mgr.RefreshToken(ctx, userID); err != nil {
		t.Fatalf("RefreshToken failed: %v", err)
	}
	newTok, err := mgr.GetToken(ctx, userID)
	if err != nil {
		t.Fatalf("GetToken after refresh failed: %v", err)
	}
	if newTok != "mock-new-access-token-789" {
		t.Fatalf("refreshed token mismatch: got %s", newTok)
	}

	// Step 6: Revocation & Disconnect
	if err := mgr.Revoke(ctx, userID); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}
	sumAfterRevoke := mgr.Summary(userID)
	if sumAfterRevoke.Connected {
		t.Fatalf("expected disconnected after revoke")
	}
}

func TestIDTokenValidation_EdgeCases(t *testing.T) {
	clientID := "my-google-client-id"
	nonce := "random-nonce-value"

	// Case 1: Bad Issuer
	badIssTok := makeFakeIDToken("https://evil.com", "sub1", clientID, nonce, "User", "u@e.com", time.Now().Add(1*time.Hour))
	if _, err := parseAndValidateIDToken(badIssTok, clientID, nonce); err == nil {
		t.Fatalf("expected error on bad issuer")
	}

	// Case 2: Bad Audience
	badAudTok := makeFakeIDToken("https://accounts.google.com", "sub1", "other-client-id", nonce, "User", "u@e.com", time.Now().Add(1*time.Hour))
	if _, err := parseAndValidateIDToken(badAudTok, clientID, nonce); err == nil {
		t.Fatalf("expected error on bad audience")
	}

	// Case 3: Bad Nonce (Replay defense)
	badNonceTok := makeFakeIDToken("https://accounts.google.com", "sub1", clientID, "different-nonce", "User", "u@e.com", time.Now().Add(1*time.Hour))
	if _, err := parseAndValidateIDToken(badNonceTok, clientID, nonce); err == nil {
		t.Fatalf("expected error on bad nonce")
	}

	// Case 4: Expired token
	expTok := makeFakeIDToken("https://accounts.google.com", "sub1", clientID, nonce, "User", "u@e.com", time.Now().Add(-10*time.Minute))
	if _, err := parseAndValidateIDToken(expTok, clientID, nonce); err == nil {
		t.Fatalf("expected error on expired ID token")
	}
}

func TestLegacyCookieCleanup(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "googleauth-cleanup-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	mgr, err := New(tmpDir, Config{
		ClientID:     "client-id",
		ClientSecret: "secret",
		RedirectURL:  "http://localhost/callback",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	userID := int64(12345)
	userDir := filepath.Join(tmpDir, "12345")
	_ = os.MkdirAll(userDir, 0o700)
	legacyCookiePath := filepath.Join(userDir, "cookies.json")
	_ = os.WriteFile(legacyCookiePath, []byte(`{"legacy":"cookies"}`), 0o600)

	// Save OAuth profile
	prof := UserProfile{
		UserID:    userID,
		GoogleSub: "sub-12345",
		Email:     "user@example.com",
		Name:      "Legacy Cleaner",
	}
	if err := mgr.SaveProfile(&prof); err != nil {
		t.Fatalf("SaveProfile failed: %v", err)
	}

	// Legacy file must be deleted
	if _, err := os.Stat(legacyCookiePath); !os.IsNotExist(err) {
		t.Fatalf("legacy cookies.json was not cleaned up!")
	}
}

func TestConcurrentAuthAttemptsAndMultiUserIsolation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "googleauth-concurrent-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	clientID := "concurrent-client.apps.googleusercontent.com"
	mgr, err := New(tmpDir, Config{
		ClientID:     clientID,
		ClientSecret: "secret",
		RedirectURL:  "http://localhost:18080/auth/google/callback",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Concurrent AuthURL generation across multiple distinct users
	const numUsers = 20
	errCh := make(chan error, numUsers)

	for i := 1; i <= numUsers; i++ {
		uid := int64(i * 1000)
		go func(u int64) {
			urlStr, err := mgr.AuthURL(u)
			if err != nil {
				errCh <- fmt.Errorf("user %d: %w", u, err)
				return
			}
			parsed, err := url.Parse(urlStr)
			if err != nil {
				errCh <- fmt.Errorf("user %d parse url: %w", u, err)
				return
			}
			st := parsed.Query().Get("state")
			gotUID, _, err := verifyState(mgr.stateSecret, st)
			if err != nil || gotUID != u {
				errCh <- fmt.Errorf("user %d state mismatch: got %d, err %v", u, gotUID, err)
				return
			}
			errCh <- nil
		}(uid)
	}

	for i := 1; i <= numUsers; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent AuthURL failure: %v", err)
		}
	}

	// Verify multi-user profile isolation
	userA := int64(1001)
	userB := int64(1002)

	profA := UserProfile{
		UserID:    userA,
		GoogleSub: "sub-user-a",
		Email:     "a@example.com",
		Name:      "User A",
	}
	profB := UserProfile{
		UserID:    userB,
		GoogleSub: "sub-user-b",
		Email:     "b@example.com",
		Name:      "User B",
	}

	if err := mgr.SaveProfile(&profA); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SaveProfile(&profB); err != nil {
		t.Fatal(err)
	}

	sumA := mgr.Summary(userA)
	sumB := mgr.Summary(userB)

	if sumA.Email != "a@example.com" || sumB.Email != "b@example.com" {
		t.Fatalf("cross-user data bleed detected: sumA=%+v, sumB=%+v", sumA, sumB)
	}

	// Delete user A, ensure user B remains completely unaffected
	if err := mgr.DeleteProfile(userA); err != nil {
		t.Fatal(err)
	}

	if mgr.Summary(userA).Connected {
		t.Fatalf("user A should be disconnected")
	}
	if !mgr.Summary(userB).Connected || mgr.Summary(userB).Email != "b@example.com" {
		t.Fatalf("user B was affected by user A deletion!")
	}
}
