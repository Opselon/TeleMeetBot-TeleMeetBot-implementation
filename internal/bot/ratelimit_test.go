package bot

import (
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/Opselon/TeleMeetBot/internal/store"
)

func TestRateLimiterBurstAndSustain(t *testing.T) {
	rl := newRateLimiter()
	t0 := time.Unix(1000, 0)

	// Burst: the first burst actions pass instantly.
	for i := 0; i < 12; i++ {
		if !rl.allow(1, t0) {
			t.Fatalf("action %d of the burst was rejected", i+1)
		}
	}
	// Bucket empty: the next action drops.
	if rl.allow(1, t0) {
		t.Fatal("action beyond burst must be dropped")
	}
	// After 1s, 4 tokens refilled: 4 pass, the 5th drops.
	t1 := t0.Add(time.Second)
	for i := 0; i < 4; i++ {
		if !rl.allow(1, t1) {
			t.Fatalf("refilled action %d was rejected", i+1)
		}
	}
	if rl.allow(1, t1) {
		t.Fatal("fifth action in one second must be dropped")
	}
}

func TestRateLimiterPerUser(t *testing.T) {
	rl := newRateLimiter()
	t0 := time.Unix(1000, 0)
	for i := 0; i < 12; i++ {
		rl.allow(1, t0)
	}
	if !rl.allow(2, t0) {
		t.Fatal("user 2 must be independent of user 1's bucket")
	}
	if rl.allow(0, t0) == false {
		t.Fatal("system-originated updates (id 0) must never be throttled")
	}
}

func TestRateLimiterNoticeThrottle(t *testing.T) {
	rl := newRateLimiter()
	t0 := time.Unix(1000, 0)
	for i := 0; i < 20; i++ {
		rl.allow(1, t0)
	}
	if !rl.shouldNotice(1, t0) {
		t.Fatal("first excess must notify")
	}
	for i := 1; i <= 5; i++ {
		if rl.shouldNotice(1, t0.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("notice %d fired inside the throttle window", i)
		}
	}
	if !rl.shouldNotice(1, t0.Add(11*time.Second)) {
		t.Fatal("notice after the window must fire")
	}
}

func TestRateLimiterGC(t *testing.T) {
	rl := newRateLimiter()
	t0 := time.Unix(1000, 0)
	rl.allow(1, t0)
	rl.allow(2, t0)
	rl.gc(t0.Add(3*time.Hour), 2*time.Hour)
	if len(rl.buckets) != 0 {
		t.Fatalf("idle buckets must be pruned, len = %d", len(rl.buckets))
	}
}

func TestHistoryView(t *testing.T) {
	base := time.Unix(1000, 0)
	recs := []store.Record{
		{ID: "ses-1", State: "completed", CreatedAt: base, EndedAt: base.Add(10 * time.Minute)},
		{ID: "ses-2", State: "error", Error: "admission timed out after 300s", CreatedAt: base.Add(time.Hour)},
	}
	out := historyView(recs)
	for _, want := range []string{"ses-1", "ses-2", "completed", "failed", "admission timed out"} {
		if !contains(out, want) {
			t.Errorf("history missing %q:\n%s", want, out)
		}
	}
	// Newest first.
	if idx1, idx2 := indexOf(out, "ses-2"), indexOf(out, "ses-1"); idx1 > idx2 {
		t.Errorf("newest session must be listed first")
	}
	// Empty history.
	if empty := historyView(nil); !contains(empty, "No sessions yet") {
		t.Errorf("empty history text wrong: %q", empty)
	}
}

func TestHistoryCapsAtFive(t *testing.T) {
	var recs []store.Record
	for i := 0; i < 9; i++ {
		recs = append(recs, store.Record{ID: string(rune('a' + i)), State: "completed"})
	}
	out := historyView(recs)
	if contains(out, "<code>a</code>") {
		t.Error("oldest records beyond the cap must not render")
	}
	if !contains(out, "<code>i</code>") {
		t.Error("newest record missing")
	}
}

func TestHandleUpdateRateLimited(t *testing.T) {
	h := newHarness(t, []int64{42}, 1)
	// Exhaust user 42's burst with real message updates.
	for i := 0; i < 12; i++ {
		upd := tgbotapi.Update{Message: &tgbotapi.Message{
			Chat: &tgbotapi.Chat{ID: 42},
			From: &tgbotapi.User{ID: 42},
			Text: "/ping",
		}}
		h.bot.handleUpdate(nil, &upd)
	}
	if got := len(h.api.sentTexts()); got == 0 {
		t.Fatal("burst actions should have produced replies")
	}
	// The bucket is empty now: further updates are dropped silently and
	// must not panic.
	upd := tgbotapi.Update{Message: &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: 42},
		From: &tgbotapi.User{ID: 42},
		Text: "/ping",
	}}
	h.bot.handleUpdate(nil, &upd)
	if !h.bot.limiter.allow(42, time.Now().Add(time.Hour)) {
		t.Fatal("bucket must be refillable after time passes")
	}
}

// helpers --------------------------------------------------------------------

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func indexOf(s, sub string) int { return strings.Index(s, sub) }
