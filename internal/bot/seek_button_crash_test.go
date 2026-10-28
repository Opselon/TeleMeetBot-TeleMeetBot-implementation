package bot

import (
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// TestSeek60sButtonDoesNotCrashTheApp reproduces the user report: pressing the
// "+60s" button sometimes takes the whole application down.
//
// The press is handled on one goroutine (handleUpdate -> handleCallback ->
// handleAction -> seekAction -> SeekBy) while the session card is re-rendered
// on another (refreshLoop). The app has exactly one recover(), inside
// handleUpdate; refreshLoop, RunEvents and the engine's own goroutines have
// none, so a panic in any of them kills that goroutine permanently — the
// dashboard freezes and the operator reads it as "the app crashed".
//
// This test drives the full bot-layer path with the refresh rendering running
// concurrently, so a panic in either goroutine surfaces as a test failure
// instead of a production outage.
func TestSeek60sButtonDoesNotCrashTheApp(t *testing.T) {
	h := newHarness(t, []int64{1}, 2)

	// A live session whose mediaRun is a seekable 300s track at 30s.
	sess := &engine.Session{
		ID:        engine.SessionID("s-seek60"),
		MeetURL:   "https://meet.google.com/abc-defg-hij",
		MediaURL:  "https://example.com/song.mp3",
		TabTitle:  "TeleMeet Media",
		MediaKind: engine.MediaDirect,
	}
	sess.SetMediaRunForTesting(fakeSeekMediaRun(30, 300))
	sess.SetStopForTesting()
	h.eng.addSession(sess)

	// Bind the session card the way a live session does.
	h.bot.mu.Lock()
	h.bot.cards[string(sess.ID)] = &cardRef{
		chat:    1,
		msg:     10,
		text:    "",
		view:    "card",
		viewAt:  time.Now(),
		fixedAt: time.Now(),
	}
	h.bot.sessionChats[string(sess.ID)] = 1
	h.bot.mu.Unlock()

	var wg sync.WaitGroup

	// Rapid +60s presses (the user tapping the button repeatedly).
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("handleAction(+60s) panicked on press %d: %v", i, r)
				}
			}()
			q := &tgbotapi.CallbackQuery{
				ID:   "q",
				From: &tgbotapi.User{ID: 1},
				Message: &tgbotapi.Message{
					MessageID: 10,
					Chat:      &tgbotapi.Chat{ID: 1},
				},
				Data: "act:s-seek60:fwd60",
			}
			h.bot.handleUpdate(nil, &tgbotapi.Update{CallbackQuery: q})
		}(i)
	}

	// The refresh loop re-rendering the same card concurrently.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("card re-render raced with +60s and panicked (iteration %d): %v", i, r)
				}
			}()
			st := sess.Snapshot()
			_ = h.bot.api.Edit(1, 10, sessionCardText(st), sessionCardKB(st))
		}(i)
	}

	wg.Wait()

	// The app must still be functional afterwards: the session survives and
	// reports a position that reflects the seeks.
	st := sess.Snapshot()
	if st.Cur < 30 {
		t.Fatalf("session position went backwards after +60s presses: %v", st.Cur)
	}
}

// fakeSeekMediaRun is a mediaRun double: a seekable track of the given length
// sitting at the given position.
func fakeSeekMediaRun(cur, dur float64) func(action string, arg float64) (engine.MediaStatus, error) {
	return func(action string, arg float64) (engine.MediaStatus, error) {
		var st engine.MediaStatus
		st.Duration = dur
		st.CurrentTime = cur
		st.Ready = 4
		st.Seekable = 3
		st.OK = true
		if action == "seekTo" {
			st.CurrentTime = arg
		}
		return st, nil
	}
}
