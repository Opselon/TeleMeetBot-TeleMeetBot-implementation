package bot

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// ---- fakes ----------------------------------------------------------------

type sentMsg struct {
	chat int64
	text string
	kb   *tgbotapi.InlineKeyboardMarkup
}

type editMsg struct {
	chat int64
	msg  int
	text string
}

type fakeAPI struct {
	mu      sync.Mutex
	sent    []sentMsg
	edits   []editMsg
	answers []string
	seq     int
}

func (f *fakeAPI) Send(chatID int64, text string, kb *tgbotapi.InlineKeyboardMarkup) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.sent = append(f.sent, sentMsg{chatID, text, kb})
	return f.seq, nil
}

func (f *fakeAPI) Edit(chatID int64, messageID int, text string, kb *tgbotapi.InlineKeyboardMarkup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, editMsg{chatID, messageID, text})
	return nil
}

func (f *fakeAPI) AnswerCallback(id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, text)
	return nil
}

func (f *fakeAPI) BotUsername() string { return "telemeet_test_bot" }

func (f *fakeAPI) Updates(int) tgbotapi.UpdatesChannel { return make(tgbotapi.UpdatesChannel) }

func (f *fakeAPI) lastSent() sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return sentMsg{}
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeAPI) allSentText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, m := range f.sent {
		b.WriteString(m.text)
		b.WriteString("\n---\n")
	}
	return b.String()
}

func (f *fakeAPI) lastEdit() editMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edits) == 0 {
		return editMsg{}
	}
	return f.edits[len(f.edits)-1]
}

type fakeEngine struct {
	mu       sync.Mutex
	sessions []*engine.Session
	requests []engine.StartRequest
}

func (f *fakeEngine) Start(ctx context.Context, req engine.StartRequest) (*engine.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	s := &engine.Session{
		ID:        engine.SessionID("s-test1"),
		MeetURL:   req.MeetLink,
		MediaURL:  req.MediaLink,
		TabTitle:  "TeleMeet Media",
		MediaKind: engine.MediaYouTube,
	}
	f.sessions = append(f.sessions, s)
	return s, nil
}

func (f *fakeEngine) Get(id engine.SessionID) *engine.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func (f *fakeEngine) Active() []*engine.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*engine.Session{}, f.sessions...)
}

func (f *fakeEngine) List() []*engine.Session { return f.Active() }

func (f *fakeEngine) Events() <-chan engine.Event { return make(chan engine.Event) }

// ---- harness --------------------------------------------------------------

type harness struct {
	bot  *Bot
	api  *fakeAPI
	eng  *fakeEngine
	cfg  *config.Config
	st   *store.Store
	path string
}

func newHarness(t *testing.T, admins []int64, maxSessions int) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Telegram.Token = "123456:AAHtest"
	cfg.Telegram.Admins = admins
	cfg.Telegram.FirstStartClaimsAdmin = true
	cfg.Limits.MaxSessions = maxSessions
	cfg.Paths.DataDir = filepath.Join(dir, "data")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	path := filepath.Join(dir, "telemeetbot.yml")
	st := store.Load(filepath.Join(cfg.Paths.DataDir, "state.json"))
	api := &fakeAPI{}
	eng := &fakeEngine{}
	b := New(api, cfg, eng, st, nil, path, "/usr/bin/chromium", "")
	return &harness{bot: b, api: api, eng: eng, cfg: cfg, st: st, path: path}
}

func (h *harness) message(userID int64, text string) {
	msg := &tgbotapi.Message{
		MessageID: 1,
		From:      &tgbotapi.User{ID: userID},
		Chat:      &tgbotapi.Chat{ID: userID},
		Text:      text,
	}
	// Telegram marks commands with a bot_command entity; mirror that here.
	if strings.HasPrefix(text, "/") {
		length := len(text)
		if sp := strings.IndexByte(text, ' '); sp >= 0 {
			length = sp
		}
		msg.Entities = []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: length}}
	}
	h.bot.handleMessage(msg)
}

func (h *harness) callback(userID int64, data string) {
	h.bot.handleCallback(context.Background(), &tgbotapi.CallbackQuery{
		ID:   "cb1",
		From: &tgbotapi.User{ID: userID},
		Message: &tgbotapi.Message{
			MessageID: 10,
			Chat:      &tgbotapi.Chat{ID: userID},
		},
		Data: data,
	})
}

// ---- tests ----------------------------------------------------------------

func TestFirstStartClaimsAdminAndPersists(t *testing.T) {
	h := newHarness(t, nil, 2)
	h.message(5094837833, "/start")

	if len(h.cfg.Telegram.Admins) != 1 || h.cfg.Telegram.Admins[0] != 5094837833 {
		t.Fatalf("admin not claimed: %+v", h.cfg.Telegram.Admins)
	}
	if !config.Exists(h.path) {
		t.Fatal("claimed admin was not persisted to disk")
	}
	loaded, err := config.Load(h.path)
	if err != nil {
		t.Fatalf("load persisted config: %v", err)
	}
	if !loaded.IsAdmin(5094837833) {
		t.Fatal("persisted config lost the admin")
	}
	if !strings.Contains(h.api.allSentText(), "Active sessions") {
		t.Fatalf("dashboard not sent: %q", h.api.allSentText())
	}
}

func TestStrangerIsDenied(t *testing.T) {
	h := newHarness(t, []int64{1}, 2)
	h.message(666, "/new")
	if !strings.Contains(h.api.lastSent().text, "Access denied") {
		t.Fatalf("expected denial, got %q", h.api.lastSent().text)
	}
	if f := h.bot.getFlow(666); f != nil {
		t.Fatal("stranger started a flow")
	}
	if len(h.eng.requests) != 0 {
		t.Fatal("stranger reached the engine")
	}
}

func TestNewStreamHappyPath(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)

	h.message(42, "/new")
	if !strings.Contains(h.api.lastSent().text, "step 1/2") {
		t.Fatalf("step 1 missing: %q", h.api.lastSent().text)
	}

	h.message(42, "meet.google.com/abc-defg-hij")
	if !strings.Contains(h.api.lastSent().text, "step 2/2") {
		t.Fatalf("step 2 missing: %q", h.api.lastSent().text)
	}

	h.message(42, "https://youtu.be/dQw4w9WgXcQ")
	if !strings.Contains(h.api.lastSent().text, "Ready to launch") {
		t.Fatalf("confirm missing: %q", h.api.lastSent().text)
	}
	if h.api.lastSent().kb == nil {
		t.Fatal("confirm keyboard missing")
	}

	h.callback(42, cbLaunch)
	if len(h.eng.requests) != 1 {
		t.Fatalf("engine not started: %+v", h.eng.requests)
	}
	req := h.eng.requests[0]
	if req.MeetLink != "https://meet.google.com/abc-defg-hij?hl=en-US" {
		t.Fatalf("meet link = %q", req.MeetLink)
	}
	if !strings.Contains(req.MediaLink, "youtube.com/watch?v=dQw4w9WgXcQ") {
		t.Fatalf("media link = %q", req.MediaLink)
	}
	if !strings.Contains(h.api.allSentText(), "s-test1") {
		t.Fatalf("session card missing: %q", h.api.allSentText())
	}
	if rec := h.st.Records(); len(rec) != 1 || rec[0].ID != "s-test1" {
		t.Fatalf("store record missing: %+v", rec)
	}
	if h.bot.getFlow(42) != nil {
		t.Fatal("flow not cleared after launch")
	}
}

func TestWizardRejectsInvalidLinks(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	h.message(42, "/new")

	h.message(42, "https://zoom.us/j/123")
	if !strings.Contains(h.api.lastSent().text, "❌") {
		t.Fatalf("zoom link accepted: %q", h.api.lastSent().text)
	}
	if f := h.bot.getFlow(42); f == nil || f.step != stepMeet {
		t.Fatal("flow moved on after an invalid meet link")
	}

	h.message(42, "abc-defg-hij")
	h.message(42, "javascript:alert(1)")
	if !strings.Contains(h.api.lastSent().text, "❌") {
		t.Fatalf("javascript: link accepted: %q", h.api.lastSent().text)
	}
	if f := h.bot.getFlow(42); f == nil || f.step != stepMedia {
		t.Fatal("flow moved on after an invalid media link")
	}
}

func TestMenuCallbacksRenderViews(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	h.message(42, "/start")

	for data, want := range map[string]string{
		cbHome:     "Active sessions",
		cbHealth:   "Health",
		cbSettings: "Settings",
		cbHelp:     "How TeleMeetBot works",
		cbSessions: "Sessions",
	} {
		h.callback(42, data)
		got := h.api.lastEdit().text
		if !strings.Contains(got, want) {
			t.Fatalf("%s rendered %q, want %q", data, got, want)
		}
	}
}

func TestLimitReachedBlocksNewFlow(t *testing.T) {
	h := newHarness(t, []int64{42}, 1)
	// Fake an active session.
	_, _ = h.eng.Start(context.Background(), engine.StartRequest{MeetLink: "https://meet.google.com/abc-defg-hij", MediaLink: "https://example.com/a.mp4"})

	h.message(42, "/new")
	if !strings.Contains(h.api.lastSent().text, "Limit reached") {
		t.Fatalf("limit not enforced: %q", h.api.lastSent().text)
	}
	if f := h.bot.getFlow(42); f != nil {
		t.Fatal("flow opened despite limit")
	}
}

func TestSessionCardActionRefresh(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	_, _ = h.eng.Start(context.Background(), engine.StartRequest{MeetLink: "https://meet.google.com/abc-defg-hij", MediaLink: "https://youtu.be/dQw4w9WgXcQ"})

	h.callback(42, "act:s-test1:refresh")
	if got := h.api.lastEdit().text; !strings.Contains(got, "s-test1") {
		t.Fatalf("card not rendered: %q", got)
	}
	if len(h.api.answers) == 0 || h.api.answers[0] != "refreshed" {
		t.Fatalf("toast = %v", h.api.answers)
	}
}

func TestUnknownCallbackIsTolerated(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	h.callback(42, "act:missing:pause")
	if got := h.api.lastEdit().text; !strings.Contains(got, "gone") {
		t.Fatalf("expected a friendly message, got %q", got)
	}
}

func TestCancelFlow(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	h.message(42, "/new")
	h.callback(42, cbCancel)
	if h.bot.getFlow(42) != nil {
		t.Fatal("flow survived cancel")
	}
	if !strings.Contains(h.api.lastEdit().text, "Cancelled") {
		t.Fatalf("cancel not confirmed: %q", h.api.lastEdit().text)
	}
}

var _ = bytes.NewBuffer
var _ = context.Background
