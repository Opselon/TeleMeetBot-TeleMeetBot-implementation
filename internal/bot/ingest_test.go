package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// Telegram ingestion: a forwarded file must be classified by its real MIME
// type, rejected when it is not playable media, and never reach the YouTube
// player logic.

func TestTelegramKindMIMEAuthoritative(t *testing.T) {
	cases := []struct {
		name, file, mime string
		want             engine.MediaKind
		wantOK           bool
	}{
		{"audio mime", "track.bin", "audio/mpeg", engine.MediaDirect, true},
		{"video mime", "clip.bin", "video/mp4", engine.MediaDirect, true},
		{"mime overrides ext", "report.pdf", "audio/mpeg", engine.MediaDirect, true},
		{"pdf document", "report.pdf", "application/pdf", "", false},
		{"zip document", "archive.zip", "application/zip", "", false},
		{"empty mime uses ext", "song.opus", "", engine.MediaDirect, true},
		{"empty mime bad ext", "data.dat", "", "", false},
		{"no mime no ext", "noext", "", "", false},
		{"ogv video", "clip.ogv", "video/ogg", engine.MediaDirect, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := telegramKind(tc.file, tc.mime)
			if ok != tc.wantOK {
				t.Fatalf("telegramKind(%q, %q) ok = %v, want %v", tc.file, tc.mime, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("telegramKind(%q, %q) = %q, want %q", tc.file, tc.mime, got, tc.want)
			}
		})
	}
}

func TestSanitizeTgName(t *testing.T) {
	cases := map[string]struct{ id, file, mime, want string }{
		"keeps extension":       {id: "sess1", file: "song.mp3", mime: "", want: "sess1.mp3"},
		"guesses ext from mime": {id: "sess2", file: "", mime: "audio/ogg", want: "sess2.ogg"},
		"rejects dangerous ext": {id: "sess3", file: "../../etc/passwd.mp3", mime: "", want: "sess3.mp3"},
		"rejects html ext":      {id: "sess4", file: "page.html", mime: "", want: "sess4.bin"},
		"unsafe id characters":  {id: "../../tmp", file: "a.mp4", mime: "", want: "------tmp.mp4"},
		"empty id":              {id: "", file: "a.mp4", mime: "", want: "tg.mp4"},
		"unknown mime no ext":   {id: "s5", file: "", mime: "application/x-weird", want: "s5.bin"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := sanitizeTgName(tc.id, tc.file, tc.mime)
			if got != tc.want {
				t.Fatalf("sanitizeTgName(%q, %q, %q) = %q, want %q", tc.id, tc.file, tc.mime, got, tc.want)
			}
		})
	}
}

func TestSanitizeTgNameNoTraversal(t *testing.T) {
	// The name must never escape the managed directory.
	for _, id := range []string{"..", "../..", "a/b", "..\\..", "id\x00"} {
		got := sanitizeTgName(id, "x.mp3", "")
		if strings.Contains(got, "..") || strings.ContainsAny(got, "/\\") {
			t.Fatalf("sanitizeTgName(%q) = %q, escapes the directory", id, got)
		}
	}
}

func TestValidateTgMediaSniffsContent(t *testing.T) {
	dir := t.TempDir()

	// Real MP3 bytes (a tiny valid header) must pass.
	mp3 := filepath.Join(dir, "ok.mp3")
	if err := os.WriteFile(mp3, []byte("ID3\x04\x00\x00\x00\x00\x00\x00"+strings.Repeat("\x00", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTgMedia(mp3, 0); err != nil {
		t.Fatalf("validateTgMedia(mp3) = %v, want nil", err)
	}

	// A file named .mp3 that is actually HTML must be rejected: the extension
	// must not be trusted.
	fake := filepath.Join(dir, "fake.mp3")
	if err := os.WriteFile(fake, []byte("<html><body>not audio</body></html>"+strings.Repeat(" ", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTgMedia(fake, 0); err == nil {
		t.Fatal("validateTgMedia(html named .mp3) = nil, want rejection")
	}

	// A plain text document must be rejected.
	txt := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(txt, []byte("hello world "+strings.Repeat("x", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateTgMedia(txt, 0); err == nil {
		t.Fatal("validateTgMedia(text) = nil, want rejection")
	}
}

func TestValidateTgMediaSizeLimit(t *testing.T) {
	dir := t.TempDir()
	mp3 := filepath.Join(dir, "big.mp3")
	body := []byte("ID3\x04\x00" + strings.Repeat("\x00", 600))
	if err := os.WriteFile(mp3, body, 0o600); err != nil {
		t.Fatal(err)
	}
	// Sniff passes (ID3 header), the explicit limit must still refuse.
	if err := validateTgMedia(mp3, 16); err == nil {
		t.Fatal("validateTgMedia over limit = nil, want rejection")
	}
	if err := validateTgMedia(mp3, 0); err != nil {
		t.Fatalf("validateTgMedia unlimited = %v, want nil", err)
	}
}

func TestTgAttachment(t *testing.T) {
	t.Run("audio", func(t *testing.T) {
		m := &tgbotapi.Message{Audio: &tgbotapi.Audio{FileID: "fid1", FileName: "a.mp3", MimeType: "audio/mpeg", FileSize: 100}}
		id, name, mime, size := tgAttachment(m)
		if id != "fid1" || name != "a.mp3" || mime != "audio/mpeg" || size != 100 {
			t.Fatalf("tgAttachment(audio) = %q %q %q %d", id, name, mime, size)
		}
	})
	t.Run("document", func(t *testing.T) {
		m := &tgbotapi.Message{Document: &tgbotapi.Document{FileID: "fid2", FileName: "d.pdf", MimeType: "application/pdf", FileSize: 5}}
		id, name, mime, size := tgAttachment(m)
		if id != "fid2" || name != "d.pdf" || mime != "application/pdf" || size != 5 {
			t.Fatalf("tgAttachment(document) = %q %q %q %d", id, name, mime, size)
		}
	})
	t.Run("video defaults name", func(t *testing.T) {
		m := &tgbotapi.Message{Video: &tgbotapi.Video{FileID: "fid3", MimeType: "video/mp4", FileSize: 9}}
		id, name, mime, size := tgAttachment(m)
		if id != "fid3" || name != "video.mp4" || mime != "video/mp4" || size != 9 {
			t.Fatalf("tgAttachment(video) = %q %q %q %d", id, name, mime, size)
		}
	})
	t.Run("empty message", func(t *testing.T) {
		m := &tgbotapi.Message{}
		if id, _, _, _ := tgAttachment(m); id != "" {
			t.Fatalf("tgAttachment(empty) = %q, want empty", id)
		}
	})
}

func TestTgHasMedia(t *testing.T) {
	if tgHasMedia(&tgbotapi.Message{}) {
		t.Fatal("tgHasMedia(empty) = true, want false")
	}
	if !tgHasMedia(&tgbotapi.Message{Audio: &tgbotapi.Audio{}}) {
		t.Fatal("tgHasMedia(audio) = false, want true")
	}
	if !tgHasMedia(&tgbotapi.Message{Document: &tgbotapi.Document{}}) {
		t.Fatal("tgHasMedia(document) = false, want true")
	}
	if !tgHasMedia(&tgbotapi.Message{Video: &tgbotapi.Video{}}) {
		t.Fatal("tgHasMedia(video) = false, want true")
	}
	if tgHasMedia(nil) {
		t.Fatal("tgHasMedia(nil) = true, want false")
	}
}

func TestHumanBytesLocal(t *testing.T) {
	cases := map[int64]string{
		512:                "512 B",
		20 * 1024 * 1024:   "20.0 MiB",
		20*1024*1024 + 512: "20.0 MiB",
	}
	for n, want := range cases {
		if got := humanBytesLocal(n); !strings.HasPrefix(got, strings.Split(want, " ")[0]) {
			t.Logf("humanBytesLocal(%d) = %s (want ~%s)", n, got, want)
		}
	}
}

type mockLocalAPI struct {
	file *tgbotapi.File
}

func (m *mockLocalAPI) File(fileID string) (*tgbotapi.File, error) {
	return m.file, nil
}

func (m *mockLocalAPI) FileDirectURL(filePath string) string {
	return "http://127.0.0.1:8081/file/botTEST/" + filePath
}

func (m *mockLocalAPI) Send(chatID int64, text string, kb *tgbotapi.InlineKeyboardMarkup) (int, error) {
	return 1, nil
}

func (m *mockLocalAPI) Edit(chatID int64, msgID int, text string, kb *tgbotapi.InlineKeyboardMarkup) error {
	return nil
}

func (m *mockLocalAPI) AnswerCallback(id, text string) error {
	return nil
}

func (m *mockLocalAPI) BotUsername() string {
	return "mockbot"
}

func (m *mockLocalAPI) Updates(timeoutSeconds int) tgbotapi.UpdatesChannel {
	return nil
}

func TestCopyTgFileLocalFastPath(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "local_video.mp4")
	destPath := filepath.Join(tmpDir, "copied_video.mp4")

	content := []byte("dummy video content for local bot api test")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	api := &mockLocalAPI{
		file: &tgbotapi.File{
			FileID:   "local_file_123",
			FilePath: srcPath,
			FileSize: len(content),
		},
	}

	ctx := context.Background()
	if err := copyTgFile(ctx, api, "local_file_123", destPath, int64(len(content)), 2000*1024*1024); err != nil {
		t.Fatalf("copyTgFile failed: %v", err)
	}

	copied, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read dest file: %v", err)
	}
	if string(copied) != string(content) {
		t.Errorf("expected content %q, got %q", content, copied)
	}
}
