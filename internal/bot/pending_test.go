package bot

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/media"
	"github.com/go-rod/rod"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ---- staged-media & playlist queue regression suite ------------------------

// TestForwardedFileIsStagedAndLeased proves a forwarded mp3/mp4 is not
// discarded: it lands in the managed directory, is registered, and is leased
// to the user's flow so the janitor cannot reclaim it before the user acts.
func TestForwardedFileIsStagedAndLeased(t *testing.T) {
	h := newHarness(t, []int64{123}, 8)
	dir := filepath.Join(h.cfg.Paths.DataDir, "downloads")
	mgr, err := media.NewManager(media.DefaultOptions(dir), &testLogBot{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })
	h.bot.mgr = mgr
	m := makeMediaMessage("track.mp3", "audio/mpeg", 4096)
	h.bot.handleTelegramMedia(1, 123, m)
	items := h.bot.getPending(123)

	if len(items) == 0 {
		t.Fatal("forwarded file must be staged")
	}
	pm := items[0]
	if !h.bot.mediaMgr().Lease(pm.EntryID, pendingOwner(123)) {
		t.Fatal("the staged file must be leased to the user's flow")
	}
	if len(h.api.sentTexts()) == 0 {
		t.Fatal("the user must be told the file is staged")
	}
}

// TestPendingCardOffersAdaptiveOptions proves the staged card offers both
// Enqueue and Switch when a session is live.
func TestPendingCardOffersAdaptiveOptions(t *testing.T) {
	h := newHarness(t, []int64{123}, 8)
	pm := &pendingMedia{UserID: 123, EntryID: "tg123", URL: "http://127.0.0.1:18081/d/tg123",
		Kind: engine.MediaDirect, Name: "track.mp3", MIME: "audio/mpeg", Bytes: 4096, Created: time.Now()}
	h.bot.setPending(pm)
	items := h.bot.getPending(123)

	assertLabels(t, h.bot.pendingKB(items, nil), "▶️ Start a new stream", "🗑 Discard staged media", "⬅️ Dashboard")
	st := engine.Status{ID: "s-a", Title: "track.mp3"}
	assertLabels(t, h.bot.pendingKB(items, []engine.Status{st}), "➕ Add to active queue", "🔀 Switch live session", "▶️ Start a new stream", "🗑 Discard staged media", "⬅️ Dashboard")
	h.bot.handlePendingCallback(1, 123, 7, cbPendingDrop)
	if h.bot.getPending(123) != nil {
		t.Fatal("discard must clear the pending slot")
	}
	if !h.api.hasText("Discarded") {
		t.Fatal("discard must confirm")
	}
}

// TestQueue50ForwardedTelegramSongs proves 50 forwarded Telegram files are
// accepted and added to the queue in submission order without manual user clicks.
func TestQueue50ForwardedTelegramSongs(t *testing.T) {
	h := newHarness(t, []int64{123}, 8)
	dir := filepath.Join(h.cfg.Paths.DataDir, "downloads")
	mgr, err := media.NewManager(media.DefaultOptions(dir), &testLogBot{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })
	h.bot.mgr = mgr

	// Case 1: When no session is live, 50 forwarded songs build a staged playlist
	for i := 1; i <= 50; i++ {
		m := &tgbotapi.Message{
			Document: &tgbotapi.Document{
				FileID:       fmt.Sprintf("doc-%d", i),
				FileName:     fmt.Sprintf("song-%02d.mp3", i),
				MimeType:     "audio/mpeg",
				FileSize:     1024,
				FileUniqueID: fmt.Sprintf("u-%d", i),
			},
		}
		h.bot.handleTelegramMedia(1, 123, m)
	}

	staged := h.bot.getPending(123)
	if len(staged) != 50 {
		t.Fatalf("staged count = %d, want 50", len(staged))
	}
	for i, pm := range staged {
		wantName := fmt.Sprintf("song-%02d.mp3", i+1)
		if pm.Name != wantName {
			t.Fatalf("item %d name = %q, want %q", i, pm.Name, wantName)
		}
	}

	// Now launch the session from staged playlist
	h.bot.handlePendingCallback(1, 123, 7, cbPendingStart)
	f := h.bot.getFlow(123)
	if f == nil || len(f.queue) != 49 {
		t.Fatalf("wizard queue count = %d, want 49 additional tracks", len(f.queue))
	}

	h.bot.stepInput(1, 123, f, "meet.google.com/abc-defg-hij")
	h.bot.launch(1, 123)

	active := h.eng.Active()
	if len(active) != 1 {
		t.Fatalf("active sessions = %d, want 1", len(active))
	}
	sess := active[0]
	q, curIdx := sess.QueueSnapshot()
	if len(q) != 50 {
		t.Fatalf("session queue count = %d, want 50", len(q))
	}
	if curIdx != 0 {
		t.Fatalf("curIdx = %d, want 0", curIdx)
	}
}

// TestQueue50BatchURLs proves submitting 50 URLs in one message enqueues them in order.
func TestQueue50BatchURLs(t *testing.T) {
	h := newHarness(t, []int64{123}, 8)
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		sb.WriteString(fmt.Sprintf("https://www.youtube.com/watch?v=video%02d\n", i))
	}

	// Submit batch to bot
	m := &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 1},
		From: &tgbotapi.User{ID: 123},
		Text: sb.String(),
	}
	h.bot.handleMessage(m)

	f := h.bot.getFlow(123)
	if f == nil || len(f.queue) != 49 {
		t.Fatalf("batch URLs staged = %d, want 49 in queue (+1 initial)", len(f.queue))
	}
	h.bot.stepInput(1, 123, f, "meet.google.com/abc-defg-hij")
	h.bot.launch(1, 123)

	sess := h.eng.Active()[0]
	q, cur := sess.QueueSnapshot()
	if len(q) != 50 {
		t.Fatalf("queue length = %d, want 50", len(q))
	}
	if cur != 0 {
		t.Fatalf("cur = %d, want 0", cur)
	}
	for i := 0; i < 50; i++ {
		wantURL := fmt.Sprintf("https://www.youtube.com/watch?v=video%02d", i+1)
		if !strings.HasPrefix(q[i].URL, wantURL) {
			t.Fatalf("item %d URL = %q, want prefix %q", i, q[i].URL, wantURL)
		}
	}
}

// TestQueueControlsPrevNextBoundary proves Prev and Next navigate the playlist
// and behave predictably at boundaries without crashing or duplicating.
func TestQueueControlsPrevNextBoundary(t *testing.T) {
	s := &engine.Session{ID: "s-ctrl"}
	s.SetMediaPageForTesting(&rod.Page{})
	s.InitQueue("https://example.com/track1.mp4", []string{
		"https://example.com/track2.mp4",
		"https://example.com/track3.mp4",
	})
	s.SetSwitchRunForTesting(func(target engine.MediaTarget, stale func() bool) error {
		return nil
	})

	// Already at start: Previous must return ErrQueueStart
	if err := s.Previous(); err != engine.ErrQueueStart {
		t.Fatalf("Previous at start = %v, want ErrQueueStart", err)
	}

	// Advance to track 2
	if err := s.Next(); err != nil {
		t.Fatalf("Next = %v", err)
	}
	if _, cur := s.QueueSnapshot(); cur != 1 {
		t.Fatalf("curIdx after Next = %d, want 1", cur)
	}

	// Advance to track 3
	if err := s.Next(); err != nil {
		t.Fatalf("Next = %v", err)
	}
	if _, cur := s.QueueSnapshot(); cur != 2 {
		t.Fatalf("curIdx after Next = %d, want 2", cur)
	}

	// Already at end: Next must return ErrQueueEnd
	if err := s.Next(); err != engine.ErrQueueEnd {
		t.Fatalf("Next at end = %v, want ErrQueueEnd", err)
	}

	// Return to track 2
	if err := s.Previous(); err != nil {
		t.Fatalf("Previous = %v", err)
	}
	if _, cur := s.QueueSnapshot(); cur != 1 {
		t.Fatalf("curIdx after Previous = %d, want 1", cur)
	}
}

// TestQueueRemoveAndClearUpcoming proves removing items and clearing upcoming
// preserves the current playing index and frees resources.
func TestQueueRemoveAndClearUpcoming(t *testing.T) {
	s := &engine.Session{ID: "s-rm"}
	s.InitQueue("https://example.com/t1.mp4", []string{
		"https://example.com/t2.mp4",
		"https://example.com/t3.mp4",
		"https://example.com/t4.mp4",
	})

	_ = s.Next() // curIdx = 1 (t2.mp4)

	// Remove upcoming item (t3.mp4 which is item-3)
	if err := s.RemoveQueueItem("item-3"); err != nil {
		t.Fatalf("RemoveQueueItem = %v", err)
	}
	q, cur := s.QueueSnapshot()
	if len(q) != 3 {
		t.Fatalf("queue len = %d, want 3", len(q))
	}
	if cur != 1 {
		t.Fatalf("curIdx = %d, want 1", cur)
	}
	if q[2].URL != "https://example.com/t4.mp4" {
		t.Fatalf("last item = %q, want t4.mp4", q[2].URL)
	}

	// Clear upcoming
	if err := s.ClearUpcoming(); err != nil {
		t.Fatalf("ClearUpcoming = %v", err)
	}
	q, cur = s.QueueSnapshot()
	if len(q) != 2 {
		t.Fatalf("after clear queue len = %d, want 2", len(q))
	}
	if cur != 1 {
		t.Fatalf("curIdx = %d, want 1", cur)
	}
}

// ---- helpers ---------------------------------------------------------------

func makeMediaMessage(fileName, mime string, size int64) *tgbotapi.Message {
	return &tgbotapi.Message{
		Document: &tgbotapi.Document{
			FileID:       "doc-1",
			FileName:     fileName,
			MimeType:     mime,
			FileSize:     int(size),
			FileUniqueID: "u-1",
		},
	}
}

func labels(kb *tgbotapi.InlineKeyboardMarkup) []string {
	var out []string
	if kb == nil {
		return out
	}
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			out = append(out, btn.Text)
		}
	}
	return out
}

type testLogBot struct{ t *testing.T }

func (l *testLogBot) Debugf(f string, a ...interface{}) {}
func (l *testLogBot) Infof(f string, a ...interface{})  {}
func (l *testLogBot) Warnf(f string, a ...interface{})  {}
func (l *testLogBot) Errorf(f string, a ...interface{}) {}

func (f *fakeAPI) hasText(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.sent {
		if strings.Contains(m.text, substr) {
			return true
		}
	}
	return false
}

func (f *fakeAPI) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, m := range f.sent {
		out[i] = m.text
	}
	return out
}

func assertLabels(t *testing.T, kb *tgbotapi.InlineKeyboardMarkup, want ...string) {
	t.Helper()
	got := labels(kb)
	if len(got) != len(want) {
		t.Fatalf("buttons = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("buttons = %v, want %v", got, want)
		}
	}
}
