package googleauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type stateTestLogger struct{}

func (stateTestLogger) Debugf(string, ...interface{}) {}
func (stateTestLogger) Infof(string, ...interface{})  {}
func (stateTestLogger) Warnf(string, ...interface{})  {}
func (stateTestLogger) Errorf(string, ...interface{}) {}

// TestStateSecretSurvivesRestart is the regression test for the OAuth state
// secret's lifetime. New() generated the state secret per process, so a
// restart inside the ten-minute flow window (a deploy, a crash, an OOM kill,
// a systemd restart) failed every in-flight sign-in with "state signature
// verification failed" — the user completed Google's consent and was told the
// sign-in was broken. The cipher key was already persisted; the state secret
// was not.
//
// A restart must now keep the signature valid: two Managers built on the same
// directory must verify each other's state. (The single-use pendingStates map
// stays in memory on purpose, so a restarted flow still fails — but at the
// pending-lookup, with the honest "expired or already used" error, never at
// the signature.)
func TestStateSecretSurvivesRestart(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "googleauth-state-*")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := Config{
		ClientID:     "test-client-id.apps.googleusercontent.com",
		ClientSecret: "test-client-secret",
		RedirectURL:  "http://127.0.0.1:18080/auth/google/callback",
	}
	log := Logger(stateTestLogger{})

	m1, err := New(tmpDir, cfg, log)
	if err != nil {
		t.Fatalf("New(m1): %v", err)
	}
	url1, err := m1.AuthURL(5094837833)
	if err != nil {
		t.Fatalf("AuthURL: %v", err)
	}
	state := extractState(t, url1)

	// The secret must have been persisted, not just generated in memory.
	keyFile := filepath.Join(tmpDir, "secrets", "oauth_state.key")
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("state secret was not persisted: %v", err)
	}
	info, err := os.Stat(keyFile)
	if err == nil && info.Mode().Perm() != 0o600 {
		t.Fatalf("state secret permissions = %o, want 0600", info.Mode().Perm())
	}

	// The restart: a fresh Manager on the same directory.
	m2, err := New(tmpDir, cfg, log)
	if err != nil {
		t.Fatalf("New(m2): %v", err)
	}

	if uid, tok, err := verifyState(m2.stateSecret, state); err != nil {
		t.Fatalf("a restarted manager cannot verify the pre-restart state: %v — every in-flight sign-in dies on restart", err)
	} else if uid != 5094837833 || tok == "" {
		t.Fatalf("restarted manager decoded the wrong identity: uid=%d tok=%q", uid, tok)
	}

	// A second restart keeps the same secret (it is load-or-create, not
	// regenerate).
	m3, err := New(tmpDir, cfg, log)
	if err != nil {
		t.Fatalf("New(m3): %v", err)
	}
	if string(m1.stateSecret) != string(m2.stateSecret) || string(m2.stateSecret) != string(m3.stateSecret) {
		t.Fatalf("the state secret is not stable across restarts")
	}
}

// TestStateSecretExplicitKeyPinsTheSecret: an operator-supplied key wins over
// the persisted one, so a fleet can share one secret.
func TestStateSecretExplicitKeyPinsTheSecret(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "googleauth-state-explicit-*")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pinned := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	cfg := Config{
		ClientID:        "test-client-id.apps.googleusercontent.com",
		ClientSecret:    "test-client-secret",
		RedirectURL:     "http://127.0.0.1:18080/auth/google/callback",
		StateSigningKey: pinned,
	}
	m, err := New(tmpDir, cfg, stateTestLogger{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(m.stateSecret) != 32 {
		t.Fatalf("explicit key derived to %d bytes, want 32", len(m.stateSecret))
	}
	// A non-hex passphrase still derives a 32-byte key, never an error.
	odd := Config{ClientID: "a", ClientSecret: "b", RedirectURL: "c", StateSigningKey: "not-hex-but-a-passphrase"}
	m2, err := New(tmpDir, odd, stateTestLogger{})
	if err != nil {
		t.Fatalf("New(non-hex key): %v", err)
	}
	if len(m2.stateSecret) != 32 {
		t.Fatalf("non-hex key derived to %d bytes, want 32", len(m2.stateSecret))
	}
}

// extractState pulls the state parameter out of an authorization URL.
func extractState(t *testing.T, authURL string) string {
	t.Helper()
	i := strings.Index(authURL, "state=")
	if i < 0 {
		t.Fatalf("no state parameter in %s", authURL)
	}
	rest := authURL[i+len("state="):]
	if j := strings.IndexAny(rest, "&"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		t.Fatalf("empty state parameter in %s", authURL)
	}
	return rest
}
