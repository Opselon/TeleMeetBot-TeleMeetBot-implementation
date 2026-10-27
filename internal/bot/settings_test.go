package bot

// Tests for the SQLite-backed per-user settings screen, the queue screen's
// new callbacks, and the guarantee that every rendered button routes to a
// handled action.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

// newHarnessWithSettings is newHarness plus a real SQLite user store wired
// through WithSettings, exactly like production main.go does.
func newHarnessWithSettings(t *testing.T, admins []int64, maxSessions int) *harness {
	t.Helper()
	h := newHarness(t, admins, maxSessions)
	us, err := store.NewUserStore(context.Background(), filepath.Join(h.cfg.Paths.DataDir, "users.db"))
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	t.Cleanup(func() { _ = us.Close() })
	h.bot.set = us
	return h
}

// addSessionWithQueue creates a live engine session with an n-item queue and
// binds it to the user's chat, like a real launch would.
func (h *harness) addSessionWithQueue(userID int64, n int) *engine.Session {
	s := &engine.Session{ID: "s-queue-test"}
	s.SetStopForTesting()
	s.SetSwitchRunForTesting(func(target engine.MediaTarget, stale func() bool) error { return nil })
	s.SetMediaRunForTesting(func(action string, arg float64) (engine.MediaStatus, error) {
		var st engine.MediaStatus
		st.Ready = 4
		st.Seekable = 1
		st.Duration = 180
		st.CurrentTime = 5
		st.OK = true
		switch action {
		case "seekStatus":
			return st, nil
		case "seekTo":
			st.CurrentTime = arg
			return st, nil
		case "rate":
			st.PlaybackRate = arg
			return st, nil
		default:
			return st, nil
		}
	})
	extra := make([]string, 0, n-1)
	for i := 1; i < n; i++ {
		extra = append(extra, "https://example.com/track"+string(rune('a'+i))+".mp4")
	}
	s.InitQueue("https://example.com/track0.mp4", extra)
	h.eng.addSession(s)
	h.bot.mu.Lock()
	h.bot.sessionChats[string(s.ID)] = userID
	h.bot.mu.Unlock()
	// Seed the durable metadata like launch does.
	h.bot.persistQueue(s)
	return s
}

// allCallbackData extracts every callback data string of a keyboard.
func allCallbackData(kb *tgbotapi.InlineKeyboardMarkup) map[string]bool {
	out := map[string]bool{}
	if kb == nil {
		return out
	}
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			if btn.CallbackData != nil {
				out[*btn.CallbackData] = true
			}
		}
	}
	return out
}

func TestSettingsScreenShowsSavedValues(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.message(42, "/settings")
	if got := h.api.lastSent().text; !strings.Contains(got, "Your settings") {
		t.Fatalf("settings screen not sent: %q", got)
	}

	// Change two values through the pickers, then re-open: the screen must
	// show the saved values, proving the load → save → refresh loop.
	h.callback(42, "set:vol:65")
	h.callback(42, "set:spd:1.5")
	h.callback(42, "set:repeat") // off -> one

	h.callback(42, "set:back")
	got := h.api.lastEdit().text
	for _, want := range []string{"65%", "1.5x", "one item"} {
		if !strings.Contains(got, want) {
			t.Fatalf("settings screen missing %q: %q", want, got)
		}
	}
}

func TestSettingsAutoplayToggleAndReset(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.callback(42, "set:autoplay")
	if got := h.api.answers[len(h.api.answers)-1]; got != "auto-advance off" {
		t.Fatalf("autoplay toast = %q, want off", got)
	}
	h.callback(42, "set:autoplay")
	if got := h.api.answers[len(h.api.answers)-1]; got != "auto-advance on" {
		t.Fatalf("autoplay toast = %q, want on", got)
	}

	// Reset returns everything to defaults and the screen reflects it.
	h.callback(42, "set:vol:100")
	h.callback(42, "set:reset")
	if got := h.api.answers[len(h.api.answers)-1]; got != "settings reset to defaults" {
		t.Fatalf("reset toast = %q", got)
	}
	u := h.bot.userSettings(42)
	if u.Volume != 0 || !u.Autoplay {
		t.Fatalf("post-reset settings = %+v", u)
	}
}

func TestSettingsAreIsolatedPerUser(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42, 43}, 2)

	h.callback(42, "set:vol:80")
	h.callback(43, "set:vol:20")

	if u := h.bot.userSettings(42); u.Volume != 80 {
		t.Fatalf("user 42 volume = %d", u.Volume)
	}
	if u := h.bot.userSettings(43); u.Volume != 20 {
		t.Fatalf("user 43 volume = %d (cross-user leak)", u.Volume)
	}
}

func TestSettingsPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Telegram.Token = "123456:AAHtest"
	cfg.Telegram.Admins = []int64{42}
	cfg.Paths.DataDir = filepath.Join(dir, "data")
	dbPath := filepath.Join(cfg.Paths.DataDir, "users.db")

	// First "process": change a setting.
	us1, err := store.NewUserStore(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if _, err := us1.SetRepeatMode(context.Background(), 42, "all"); err != nil {
		t.Fatalf("set: %v", err)
	}
	_ = us1.Close()

	// Second "process": the setting survives.
	us2, err := store.NewUserStore(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer us2.Close()
	u, err := us2.Get(context.Background(), 42)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if u.RepeatMode != store.RepeatAll {
		t.Fatalf("repeat after restart = %q, want all", u.RepeatMode)
	}
}

func TestSettingsScreenWithoutSQLiteStore(t *testing.T) {
	// The fallback harness has no settings store: the screen must still
	// render (legacy defaults) and toggles must answer instead of hanging.
	h := newHarness(t, []int64{42}, 2)
	h.message(42, "/settings")
	if got := h.api.lastSent().text; !strings.Contains(got, "Your settings") {
		t.Fatalf("fallback settings screen missing: %q", got)
	}
	h.callback(42, "set:autoplay")
	if got := h.api.answers[len(h.api.answers)-1]; !strings.Contains(got, "unavailable") {
		t.Fatalf("fallback toggle toast = %q", got)
	}
}

func TestLaunchAppliesPerUserSettings(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.callback(42, "set:vol:35")
	h.callback(42, "set:spd:1.25")
	h.callback(42, "set:repeat") // off → one
	h.callback(42, "set:autoplay")

	// Run the wizard to a launchable state.
	h.callback(42, cbNew)
	h.message(42, "https://meet.google.com/abc-defg-hij")
	h.message(42, "https://example.com/video.mp4")
	h.callback(42, cbLaunch)

	if len(h.eng.requests) != 1 {
		t.Fatalf("engine requests = %d, want 1", len(h.eng.requests))
	}
	req := h.eng.requests[0]
	if req.Volume != 35 || req.Speed != 1.25 || req.RepeatMode != store.RepeatOne {
		t.Fatalf("start request = vol %d speed %.2f repeat %q", req.Volume, req.Speed, req.RepeatMode)
	}
	if req.AutoPlay == nil || *req.AutoPlay {
		t.Fatal("autoplay=false was not forwarded to the engine")
	}
}

func TestQueueScreenShowsProgressAndToggles(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 3)

	h.callback(42, "act:"+string(s.ID)+":queue")
	got := h.api.lastEdit().text
	if !strings.Contains(got, "1/3") {
		t.Fatalf("queue screen missing position: %q", got)
	}
	data := allCallbackData(h.api.lastSent().kb)
	for _, want := range []string{
		"q:repeat:" + string(s.ID),
		"q:shuftoggle:" + string(s.ID),
		"q:jump:" + string(s.ID) + ":item-2",
		"q:add:" + string(s.ID),
		"q:load:" + string(s.ID),
	} {
		if !data[want] {
			t.Fatalf("queue keyboard missing %q; has %v", want, data)
		}
	}
}

func TestQueueRepeatToggleCyclesModes(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	id := string(s.ID)

	h.callback(42, "q:repeat:"+id)
	if got := s.RepeatMode(); got != engine.RepeatOne {
		t.Fatalf("after 1st toggle = %q, want one", got)
	}
	h.callback(42, "q:repeat:"+id)
	if got := s.RepeatMode(); got != engine.RepeatAll {
		t.Fatalf("after 2nd toggle = %q, want all", got)
	}
	h.callback(42, "q:repeat:"+id)
	if got := s.RepeatMode(); got != engine.RepeatOff {
		t.Fatalf("after 3rd toggle = %q, want off", got)
	}
}

func TestQueueShuffleToggle(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	id := string(s.ID)

	h.callback(42, "q:shuftoggle:"+id)
	if !s.Shuffle() {
		t.Fatal("shuffle not enabled")
	}
	h.callback(42, "q:shuftoggle:"+id)
	if s.Shuffle() {
		t.Fatal("shuffle not disabled")
	}
}

func TestQueueSelectItemJumpsAndPersists(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 3)

	h.callback(42, "q:jump:"+string(s.ID)+":item-3")
	if _, cur := s.QueueSnapshot(); cur != 2 {
		t.Fatalf("cursor = %d, want 2", cur)
	}
	// The queue cursor is durably persisted for the owning user.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	snap, found, err := h.bot.set.LoadSessionQueue(ctx, 42, string(s.ID))
	if err != nil || !found {
		t.Fatalf("load persisted queue: found=%v err=%v", found, err)
	}
	if snap.Cursor != 2 || len(snap.Items) != 3 {
		t.Fatalf("persisted snapshot = cursor %d, %d items", snap.Cursor, len(snap.Items))
	}
}

func TestQueueJumpToMissingItemAnswersAndRefreshes(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)

	h.callback(42, "q:jump:"+string(s.ID)+":item-999")
	if got := h.api.answers[len(h.api.answers)-1]; !strings.Contains(got, "not found") {
		t.Fatalf("stale item toast = %q", got)
	}
	// The screen refreshed instead of leaving the user on the stale view.
	if got := h.api.lastEdit().text; !strings.Contains(got, "Media Queue") {
		t.Fatalf("queue screen not refreshed: %q", got)
	}
}

func TestPrevAtQueueStartRestartsItem(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)

	h.callback(42, "act:"+string(s.ID)+":prev")
	if got := h.api.answers[len(h.api.answers)-1]; !strings.Contains(got, "restarted") {
		t.Fatalf("prev-at-start toast = %q", got)
	}
	if _, cur := s.QueueSnapshot(); cur != 0 {
		t.Fatalf("cursor = %d, want 0", cur)
	}
}

func TestQueuePagination(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 20) // 3 pages at 8/page

	h.callback(42, "act:"+string(s.ID)+":queue")
	data := allCallbackData(h.api.lastSent().kb)
	if !data["q:page:"+string(s.ID)+":1"] {
		t.Fatalf("page 2 button missing: %v", data)
	}

	h.callback(42, "q:page:"+string(s.ID)+":1")
	got := h.api.lastEdit().text
	if !strings.Contains(got, "Page 2/3") {
		t.Fatalf("page indicator missing: %q", got)
	}
	if !strings.Contains(got, "9. ") {
		t.Fatalf("page 2 does not list item 9: %q", got)
	}
}

func TestQueueAddFlowAppendsAndPersists(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 1)

	h.callback(42, "q:add:"+string(s.ID))
	h.message(42, "https://example.com/x1.mp4 https://example.com/x2.mp4")

	items, cur := s.QueueSnapshot()
	if len(items) != 3 {
		t.Fatalf("queue = %d items, want 3", len(items))
	}
	if cur != 0 {
		t.Fatalf("cursor moved on append: %d", cur)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	snap, found, _ := h.bot.set.LoadSessionQueue(ctx, 42, string(s.ID))
	if !found || len(snap.Items) != 3 {
		t.Fatalf("persisted queue not updated: found=%v items=%d", found, len(snap.Items))
	}
}

func TestQueueAddNextFlowInsertsAfterCurrent(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 3)

	h.callback(42, "q:addnext:"+string(s.ID))
	h.message(42, "https://example.com/next.mp4")

	items, cur := s.QueueSnapshot()
	if len(items) != 4 {
		t.Fatalf("queue = %d items, want 4", len(items))
	}
	if items[1].URL != "https://example.com/next.mp4" {
		t.Fatalf("play-next item at position 2 = %q", items[1].URL)
	}
	if cur != 0 {
		t.Fatalf("cursor = %d, want 0", cur)
	}
}

func TestQueueClearRequiresConfirmation(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 3)
	id := string(s.ID)

	// Pressing "Clear…" only shows the confirmation.
	h.callback(42, "q:clear_confirm:"+id)
	items, _ := s.QueueSnapshot()
	if len(items) != 3 {
		t.Fatalf("clear_confirm mutated the queue: %d items", len(items))
	}
	// The confirmed action does the work.
	h.callback(42, "q:do_clear:"+id)
	items, _ = s.QueueSnapshot()
	if len(items) != 1 {
		t.Fatalf("do_clear left %d items, want 1", len(items))
	}
}

func TestInterruptedQueueShownByQueueCommand(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	// A persisted queue from a "previous process" — marked interrupted.
	ctx := context.Background()
	_ = h.bot.set.SaveSessionQueue(ctx, store.SessionQueueSnapshot{
		SessionID: "s-dead",
		OwnerID:   42,
		State:     store.QueueStateInterrupted,
		Cursor:    1,
		Items: []store.QueueItemSnapshot{
			{URL: "https://example.com/a.mp4", Title: "a"},
			{URL: "https://example.com/b.mp4", Title: "b"},
		},
	})

	h.message(42, "/queue")
	got := h.api.lastSent().text
	if !strings.Contains(got, "Interrupted queue") || !strings.Contains(got, "s-dead") {
		t.Fatalf("interrupted queue not shown: %q", got)
	}
}

func TestEveryRenderedCallbackRoutes(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 3)
	s := h.addSessionWithQueue(42, 3)

	// Render every pure screen the bot can show and collect their buttons.
	st := s.Snapshot()
	items, curIdx := s.QueueSnapshot()
	_, moveMenuKB := queueMoveMenu(st, items, curIdx)
	markups := []*tgbotapi.InlineKeyboardMarkup{
		mainMenuKB(),
		helpKB(),
		audioLangKB("fa"),
		mediaModeKB("browser"),
		backKB(),
		cancelKB(),
		sessionCardKB(st),
		sessionsKB([]engine.Status{st}),
		confirmKB("fa", "browser", false),
		enqueuedKB(string(s.ID), "item-2"),
		queueViewKB(st, items, curIdx, 0),
		queueRemoveMenuKB(string(s.ID), items, curIdx),
		queueClearConfirmKB(string(s.ID)),
		moveMenuKB,
		playlistQueueToolsKB(st),
		userSettingsKB(h.bot.userSettings(42)),
		queueLoadKB(st, nil),
		meetPickerKB(nil),
		meetManageKB(nil),
	}
	if h.bot.pl != nil {
		markups = append(markups, playlistListKB(nil, 0))
	}

	for _, mk := range markups {
		for data := range allCallbackData(mk) {
			if !routesToHandler(data) {
				t.Errorf("rendered callback %q has no route in handleCallback", data)
			}
		}
	}
}

// routesToHandler mirrors the routing table in handleCallback. It lives in
// the test so a new prefix or exact match added to the router must be
// mirrored here — the compiler keeps the two from drifting apart only via
// this test's failure.
func routesToHandler(data string) bool {
	switch {
	case data == cbHome || data == cbNew || data == cbSessions || data == cbHealth ||
		data == cbHelp || data == cbSettings || data == cbAudioLang || data == cbPlaylists ||
		data == cbManage || data == cbLaunch || data == cbCancel || data == cbAgain ||
		data == cbRefresh || data == cbMediaMode || data == cbAccount ||
		data == cbAccountConnect || data == cbAccountRemove || data == cbAccountHelp ||
		data == cbLangFa || data == cbLangEn || data == cbLangOrig || data == cbLangAuto:
		return true
	case data == cbEditMeet || data == cbEditMedia || data == cbResumeDraft:
		return true
	case strings.HasPrefix(data, "mode:"),
		strings.HasPrefix(data, "lang:"),
		strings.HasPrefix(data, "ses:"),
		strings.HasPrefix(data, "pm:"),
		strings.HasPrefix(data, "q:"),
		strings.HasPrefix(data, "act:"),
		strings.HasPrefix(data, "set:"),
		strings.HasPrefix(data, cbMeetDelete),
		strings.HasPrefix(data, cbMeetRename),
		strings.HasPrefix(data, cbPlaylistMeet),
		strings.HasPrefix(data, cbPlaylist),
		strings.HasPrefix(data, cbManage+":"):
		return true
	case data == cbMeetNew || data == cbMeetList || data == cbMeetDone ||
		data == cbMeetPick: // "meet:" is both exact and a prefix
		return true
	}
	return false
}
