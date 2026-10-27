package googleauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// generateRandomBytes generates n cryptographically secure random bytes.
func generateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("read random bytes: %w", err)
	}
	return b, nil
}

// generatePKCE generates a code verifier and code challenge (S256) per RFC 7636.
func generatePKCE() (verifier, challenge string, err error) {
	raw, err := generateRandomBytes(32)
	if err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// generateNonce generates an OpenID Connect nonce.
func generateNonce() (string, error) {
	raw, err := generateRandomBytes(24)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// signState creates an authenticated, tamper-evident OAuth state parameter.
// Format: base64url(userID:stateToken:expiryUnix).hex(hmacSHA256)
func signState(secret []byte, userID int64, stateToken string, exp time.Time) string {
	payload := fmt.Sprintf("%d:%s:%d", userID, stateToken, exp.Unix())
	encodedPayload := base64.RawURLEncoding.EncodeToString([]byte(payload))

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encodedPayload))
	sig := hex.EncodeToString(mac.Sum(nil))

	return encodedPayload + "." + sig
}

// verifyState validates the HMAC signature and expiration of an OAuth state parameter.
func verifyState(secret []byte, rawState string) (userID int64, stateToken string, err error) {
	parts := strings.Split(rawState, ".")
	if len(parts) != 2 {
		return 0, "", errors.New("invalid state structure")
	}

	encodedPayload, sig := parts[0], parts[1]

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encodedPayload))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return 0, "", errors.New("state signature verification failed")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return 0, "", fmt.Errorf("decode state payload: %w", err)
	}

	fields := strings.Split(string(payloadBytes), ":")
	if len(fields) != 3 {
		return 0, "", errors.New("malformed state fields")
	}

	uid, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || uid <= 0 {
		return 0, "", errors.New("invalid user id in state")
	}

	token := fields[1]
	if token == "" {
		return 0, "", errors.New("empty state token")
	}

	expUnix, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, "", errors.New("invalid expiration in state")
	}

	if time.Now().Unix() > expUnix {
		return 0, "", errors.New("state has expired")
	}

	return uid, token, nil
}

// encryptToken encrypts plaintext using AES-256-GCM with associated data (AAD).
func encryptToken(key []byte, plaintext, aad string) (string, error) {
	if len(key) != 32 {
		return "", errors.New("encryption key must be exactly 32 bytes (256 bits)")
	}
	if plaintext == "" {
		return "", nil
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonce, err := generateRandomBytes(gcm.NonceSize())
	if err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

// decryptToken decrypts an AES-256-GCM ciphertext using associated data (AAD).
func decryptToken(key []byte, encodedCiphertext, aad string) (string, error) {
	if len(key) != 32 {
		return "", errors.New("encryption key must be exactly 32 bytes (256 bits)")
	}
	if encodedCiphertext == "" {
		return "", nil
	}

	data, err := base64.RawURLEncoding.DecodeString(encodedCiphertext)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext shorter than nonce size")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(aad))
	if err != nil {
		return "", fmt.Errorf("decrypt token: %w", err)
	}

	return string(plaintext), nil
}

// initCipherKey retrieves or generates a persistent 32-byte master encryption key.
func initCipherKey(baseDir, explicitKey string) ([]byte, error) {
	if strings.TrimSpace(explicitKey) != "" {
		k := strings.TrimSpace(explicitKey)
		decoded, err := hex.DecodeString(k)
		if err == nil && len(decoded) == 32 {
			return decoded, nil
		}
		// If not valid 32-byte hex, derive 32-byte key via SHA-256
		h := sha256.Sum256([]byte(k))
		return h[:], nil
	}

	secretsDir := filepath.Join(baseDir, "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create secrets dir: %w", err)
	}

	keyFile := filepath.Join(secretsDir, "oauth_cipher.key")
	if existing, err := os.ReadFile(keyFile); err == nil {
		decoded, err := hex.DecodeString(strings.TrimSpace(string(existing)))
		if err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}

	// Generate fresh key
	newKey, err := generateRandomBytes(32)
	if err != nil {
		return nil, fmt.Errorf("generate fresh cipher key: %w", err)
	}

	hexKey := hex.EncodeToString(newKey)
	if err := os.WriteFile(keyFile, []byte(hexKey), 0o600); err != nil {
		return nil, fmt.Errorf("persist cipher key: %w", err)
	}

	return newKey, nil
}
