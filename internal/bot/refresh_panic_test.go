package bot

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/media"
)

// TestRefreshLoopSurvivesPanic proves the +60s crash class is fixed: if the
// card refresh path panics (a session snapshot taken mid-teardown while a
// +60s seek press is in flight, a nil card reference), the loop must log the
// panic, keep ticking, and keep serving — instead of dying silently and
// freezing the dashboard, which is what the operator experiences as "the
// whole app crashed".
func TestRefreshLoopSurvivesPanic(t *testing.T) {
	b := &Bot{log: noopLogger{}}
	b.cards = map[string]*cardRef{}

	var ticks int32
	// refreshAll -> eng.List() that panics on the first two calls, then
	// behaves. The loop must survive all three phases.
	b.eng = &panicEngine{ticks: &ticks}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.refreshLoop(ctx, 20*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&ticks) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("refresh loop died after panic: only %d ticks completed (expected the loop to keep going)", atomic.LoadInt32(&ticks))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type panicEngine struct {
	ticks *int32
}

func (p *panicEngine) List() []*engine.Session {
	atomic.AddInt32(p.ticks, 1)
	n := atomic.LoadInt32(p.ticks)
	if n <= 2 {
		// The panic that used to kill the loop: a snapshot of a session that
		// is being torn down while a +60s seek press is in flight.
		panic("simulated mid-teardown snapshot panic")
	}
	return nil
}
func (p *panicEngine) Start(ctx context.Context, req engine.StartRequest) (*engine.Session, error) {
	return nil, nil
}
func (p *panicEngine) Get(id engine.SessionID) *engine.Session { return nil }
func (p *panicEngine) Active() []*engine.Session               { return nil }
func (p *panicEngine) Events() <-chan engine.Event             { return nil }
func (p *panicEngine) MediaManager() *media.Manager            { return nil }
