package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/googleauth"
)

func newHarnessWithGoogleAuth(t *testing.T, admins []int64) *harness {
	t.Helper()
	h := newHarness(t, admins, 2)
	authDir := filepath.Join(h.cfg.Paths.DataDir, "user-profiles")
	_ = os.MkdirAll(authDir, 0o700)
	gauth, err := googleauth.New(authDir, googleauth.Config{
		ClientID:     "mock-client-id.apps.googleusercontent.com",
		ClientSecret: "mock-client-secret",
		RedirectURL:  "http://localhost:18080/auth/google/callback",
	}, nil)
	if err != nil {
		t.Fatalf("initialize googleauth: %v", err)
	}
	h.bot.gauth = gauth
	return h
}

func TestAccountCommandGuestMode(t *testing.T) {
	h := newHarnessWithGoogleAuth(t, []int64{42})

	h.message(42, "/account")
	last := h.api.lastSent()
	if !strings.Contains(last.text, "Guest Mode") {
		t.Fatalf("expected Guest Mode in text, got: %s", last.text)
	}
	if !strings.Contains(last.text, "Google Account Profile") {
		t.Fatalf("expected Google Account Profile in text, got: %s", last.text)
	}
	if last.kb == nil {
		t.Fatalf("expected keyboard with Continue with Google")
	}
}

func TestAccountCommandConnected(t *testing.T) {
	h := newHarnessWithGoogleAuth(t, []int64{42})

	// Pre-populate verified Google profile
	prof := &googleauth.UserProfile{
		UserID:      42,
		GoogleSub:   "google-sub-42",
		Email:       "testuser@gmail.com",
		Name:        "Test User",
		ConnectedAt: time.Now(),
		TokenExpiry: time.Now().Add(1 * time.Hour),
	}
	if err := h.bot.gauth.SaveProfile(prof); err != nil {
		t.Fatalf("save profile: %v", err)
	}

	h.message(42, "/account")
	last := h.api.lastSent()
	if !strings.Contains(last.text, "Connected") {
		t.Fatalf("expected Connected state, got: %s", last.text)
	}
	if !strings.Contains(last.text, "testuser@gmail.com") {
		t.Fatalf("expected email in text, got: %s", last.text)
	}
	if !strings.Contains(last.text, "Test User") {
		t.Fatalf("expected name in text, got: %s", last.text)
	}

	// Disconnecting
	h.callback(42, cbAccountRemove)
	last = h.api.lastSent()
	if !strings.Contains(last.text, "Guest Mode") {
		t.Fatalf("expected Guest Mode after disconnect, got: %s", last.text)
	}
	if h.bot.hasGoogleAccount(42) {
		t.Fatalf("user 42 should no longer have google account")
	}
}

func TestAccountHelpView(t *testing.T) {
	h := newHarnessWithGoogleAuth(t, []int64{42})

	h.callback(42, cbAccountHelp)
	last := h.api.lastSent()
	if !strings.Contains(last.text, "OAuth 2.0") || !strings.Contains(last.text, "Google Meet") {
		t.Fatalf("expected OAuth and Meet integration info in help, got: %s", last.text)
	}
}

func TestNotifyGoogleConnected(t *testing.T) {
	h := newHarnessWithGoogleAuth(t, []int64{42})

	h.bot.NotifyGoogleConnected(42, "user@domain.com", "John Doe")
	last := h.api.lastSent()
	if !strings.Contains(last.text, "Google Account Connected") || !strings.Contains(last.text, "John Doe") {
		t.Fatalf("expected notification message, got: %s", last.text)
	}
}

func TestLaunchAttachesVerifiedIdentity(t *testing.T) {
	h := newHarnessWithGoogleAuth(t, []int64{42, 99})

	// Pre-populate verified Google profile for user 42
	prof := &googleauth.UserProfile{
		UserID:    42,
		GoogleSub: "google-sub-42",
		Email:     "verified@gmail.com",
		Name:      "Verified Attendant",
	}
	if err := h.bot.gauth.SaveProfile(prof); err != nil {
		t.Fatalf("save profile: %v", err)
	}

	// Launch session for user 42
	h.bot.setFlow(42, &flow{
		step:  stepNone,
		meet:  "https://meet.google.com/abc-defg-hij",
		media: "https://example.com/audio.mp3",
	})
	h.bot.launch(42, 42)

	active := h.eng.Active()
	if len(active) == 0 {
		t.Fatalf("expected active session")
	}

	// User 99 (who has no Google account) remains a guest
	if h.bot.hasGoogleAccount(99) {
		t.Errorf("user 99 should not have google account")
	}
}
