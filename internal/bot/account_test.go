package bot

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/googleauth"
)

const testValidNetscapeCookies = `# Netscape HTTP Cookie File
.google.com	TRUE	/	TRUE	1893456000	SID	test-sid-token-value
#HttpOnly_.google.com	TRUE	/	TRUE	1893456000	HSID	test-hsid-token-value
.google.com	TRUE	/	TRUE	1893456000	SAPISID	test-sapisid-token-value
.google.com	TRUE	/	TRUE	1893456000	__Secure-1PSID	test-sec1psid-token-value
`

func newHarnessWithGoogleAuth(t *testing.T, admins []int64) *harness {
	t.Helper()
	h := newHarness(t, admins, 2)
	authDir := filepath.Join(h.cfg.Paths.DataDir, "user-profiles")
	_ = os.MkdirAll(authDir, 0o700)
	gauth := googleauth.New(authDir, nil)
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
}

func TestAccountUploadFlowViaText(t *testing.T) {
	h := newHarnessWithGoogleAuth(t, []int64{42})

	// Click Upload Cookies button
	h.callback(42, cbAccountUpload)

	f := h.bot.getFlow(42)
	if f == nil || f.step != stepUploadCookies {
		t.Fatalf("expected flow to be in stepUploadCookies, got %+v", f)
	}

	// Send valid cookie text
	h.message(42, testValidNetscapeCookies)

	last := h.api.lastSent()
	if !strings.Contains(last.text, "Google Account Connected") {
		t.Fatalf("expected confirmation message, got: %s", last.text)
	}
	if !strings.Contains(last.text, "4") { // 4 valid google cookies
		t.Fatalf("expected 4 cookies saved, got: %s", last.text)
	}

	// Flow should be cleared
	if f := h.bot.getFlow(42); f != nil {
		t.Fatalf("expected flow cleared, got %+v", f)
	}

	// Viewing /account now shows Connected
	h.message(42, "/account")
	last = h.api.lastSent()
	if !strings.Contains(last.text, "Connected") || !strings.Contains(last.text, "4 Google session cookies") {
		t.Fatalf("expected Connected state with 4 cookies, got: %s", last.text)
	}

	// Disconnecting
	h.callback(42, cbAccountRemove)
	last = h.api.lastSent()
	if !strings.Contains(last.text, "Guest Mode") {
		t.Fatalf("expected Guest Mode after remove, got: %s", last.text)
	}
}

func TestAccountUploadViaDocument(t *testing.T) {
	h := newHarnessWithGoogleAuth(t, []int64{42})

	// Set up mock HTTP server for file download
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(testValidNetscapeCookies))
	}))
	defer srv.Close()

	h.api.srvURL = srv.URL
	h.api.filePath = "cookies.txt"

	docMsg := &tgbotapi.Message{
		MessageID: 10,
		From:      &tgbotapi.User{ID: 42},
		Chat:      &tgbotapi.Chat{ID: 42},
		Document: &tgbotapi.Document{
			FileID:   "cookie-file-123",
			FileName: "cookies.txt",
			FileSize: len(testValidNetscapeCookies),
			MimeType: "text/plain",
		},
	}

	h.bot.handleMessage(docMsg)

	last := h.api.lastSent()
	if !strings.Contains(last.text, "Google Account Connected") {
		t.Fatalf("expected success uploading cookie doc, got: %s", last.text)
	}

	if !h.bot.hasGoogleAccount(42) {
		t.Fatalf("expected user 42 to have google account")
	}
}

func TestLaunchAttachesGoogleCookies(t *testing.T) {
	h := newHarnessWithGoogleAuth(t, []int64{42, 99})

	// Save cookies for user 42 only
	_, err := h.bot.gauth.Save(42, []byte(testValidNetscapeCookies))
	if err != nil {
		t.Fatalf("save cookies: %v", err)
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

	// Verify User 99 (who has no cookies) launches without cookies
	h.bot.setFlow(99, &flow{
		step:  stepNone,
		meet:  "https://meet.google.com/xyz-uvwx-rst",
		media: "https://example.com/other.mp3",
	})
	h.bot.launch(99, 99)

	// User 99's session should have 0 cookies
	if h.bot.hasGoogleAccount(99) {
		t.Errorf("user 99 should not have google account")
	}
}

func TestAccountUploadRejectsInvalidFile(t *testing.T) {
	h := newHarnessWithGoogleAuth(t, []int64{42})

	h.callback(42, cbAccountUpload)
	h.message(42, "this is definitely not a valid cookie file")

	last := h.api.lastSent()
	if !strings.Contains(last.text, "Failed to load cookies") {
		t.Fatalf("expected error message, got: %s", last.text)
	}
}
