package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingLogger records log lines so a test can count churn.
type countingLogger struct {
	mu    sync.Mutex
	infos []string
}

func (c *countingLogger) Debugf(f string, a ...interface{}) {}
func (c *countingLogger) Infof(f string, a ...interface{}) {
	c.mu.Lock()
	c.infos = append(c.infos, f)
	c.mu.Unlock()
}
func (c *countingLogger) Warnf(string, ...interface{})  {}
func (c *countingLogger) Errorf(string, ...interface{}) {}

// permanentGatePage is Google's host-restriction screen that never lifts: it
// counts loads so the test can prove entry was re-requested, but it stays
// blocked for the whole wait (a real gate clears only when a host joins).
const permanentGatePage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Meet</title></head>
<body>
  <div id="gate">You can't join this video call &nbsp; No one can join a meeting unless invited or admitted by the host &nbsp; Return to home screen</div>
<script>
  var n = parseInt(sessionStorage.getItem('visits') || '0', 10) + 1;
  sessionStorage.setItem('visits', String(n));
  document.body.dataset.visits = n;
</script>
</body></html>`

// TestHostGateWaitIsNotABusyLoop is the regression for the production CPU burn:
// while a meeting is host-restricted, the join loop re-polled the Meet DOM every
// ~800ms and put the attempt counter into the dashboard detail, so every poll
// also emitted a state event, rewrote the state file and edited the Telegram
// card (a real session logged 178 attempts in 2m35s).
//
// The gate cannot lift without a re-request, so the loop must poll at the
// re-entry cadence and keep the card text stable, while still re-requesting
// entry (recovery) on a bounded cadence.
func TestHostGateWaitIsNotABusyLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	var visits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		visits++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(permanentGatePage))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, srv.URL+"/lookup/abc-defg-hij")

	log := &countingLogger{}
	mgr := &Manager{
		meetCfg: MeetOptions{
			GuestName:          "TeleMeet Bot",
			PresentMode:        "tab",
			JoinTimeoutSeconds: 3,
			AdmitWaitSeconds:   6,
		},
		log:      log,
		events:   make(chan Event, 4096),
		sessions: map[SessionID]*Session{},
	}
	s := &Session{
		ID:       "s-pacing",
		MeetURL:  srv.URL + "/lookup/abc-defg-hij",
		state:    StateJoining,
		ctx:      context.Background(),
		meetPage: page,
		log:      log,
		mgr:      mgr,
	}

	start := time.Now()
	err := s.joinMeeting()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("joinMeeting should report a timeout when no host ever starts the meeting")
	}
	if elapsed > 15*time.Second {
		t.Fatalf("join loop overran its wait budget: %v", elapsed)
	}
	t.Logf("join wait window: %s (polls are paced at %s while blocked)", elapsed.Round(time.Millisecond), blockedPollInterval)

	// Count emitted state events (= what the dashboard card and state file
	// consume) and the distinct waiting texts.
	states := 0
	waiting := map[string]int{}
	for {
		select {
		case ev := <-mgr.events:
			if ev.Kind != EventState {
				continue
			}
			states++
			if strings.Contains(ev.Text, "waiting for host") {
				waiting[ev.Text]++
			}
			continue
		default:
		}
		break
	}
	if states == 0 {
		t.Fatal("no state events emitted at all")
	}
	// Before the fix this window produced one event per ~800ms poll (>8).
	if states > 8 {
		t.Fatalf("host-gate wait emitted %d state events in %v (one per poll = busy loop)", states, elapsed)
	}
	if len(waiting) != 1 {
		t.Fatalf("the waiting card text changed while waiting (event storm): %v", waiting)
	}
	for text := range waiting {
		if strings.Contains(strings.ToLower(text), "attempt") {
			t.Fatalf("card detail must not carry a poll counter: %q", text)
		}
	}

	// Recovery must still work: entry is re-requested on a bounded cadence.
	mu.Lock()
	got := visits
	mu.Unlock()
	t.Logf("state events=%d distinct waiting texts=%d page loads=%d in %s", states, len(waiting), got, elapsed.Round(time.Millisecond))
	if got < 2 {
		t.Fatalf("entry was never re-requested (page loads=%d); recovery is broken", got)
	}
	if got > 4 {
		t.Fatalf("entry re-requested %d times in %v; reload cadence is too aggressive", got, elapsed)
	}
}

// TestHostWaitHeartbeatIsBounded pins that the wait still leaves a diagnostic
// trail (one line per hostWaitHeartbeat, not one per poll).
func TestHostWaitHeartbeatIsBounded(t *testing.T) {
	log := &countingLogger{}
	s := &Session{ID: "s-hb", log: log}
	var last time.Time
	start := time.Now().Add(-2 * time.Minute)
	for i := 1; i <= 50; i++ {
		s.logHostWait(&last, "blocked", "host-restricted meeting", i, start)
	}
	if len(log.infos) != 1 {
		t.Fatalf("50 polls in a burst produced %d heartbeat lines, want 1", len(log.infos))
	}
	if !strings.Contains(log.infos[0], "attempts=") {
		t.Fatalf("heartbeat should carry the re-entry attempt count: %q", log.infos[0])
	}
}
