package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/media"
	"github.com/go-rod/rod"
)

type dummyLogger struct{}

func (dummyLogger) Debugf(format string, args ...any) {}
func (dummyLogger) Infof(format string, args ...any)  {}
func (dummyLogger) Warnf(format string, args ...any)  {}
func (dummyLogger) Errorf(format string, args ...any) {}

// TestSessionQueue50Sequential verifies creating a 50-item queue and stepping
// sequentially through all 50 items.
func TestSessionQueue50Sequential(t *testing.T) {
	s := &Session{ID: "s-seq-50", log: dummyLogger{}, mediaPage: &rod.Page{}}
	extra := make([]string, 49)
	for i := 0; i < 49; i++ {
		extra[i] = fmt.Sprintf("https://www.youtube.com/watch?v=track%02d", i+2)
	}
	s.InitQueue("https://www.youtube.com/watch?v=track01", extra)

	switched := make([]string, 0, 50)
	var mu sync.Mutex
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error {
		mu.Lock()
		switched = append(switched, target.URL)
		mu.Unlock()
		return nil
	})

	items, curIdx := s.QueueSnapshot()
	if len(items) != 50 {
		t.Fatalf("queue len = %d, want 50", len(items))
	}
	if curIdx != 0 {
		t.Fatalf("curIdx = %d, want 0", curIdx)
	}

	// Advance through all 50 items
	for i := 1; i < 50; i++ {
		if err := s.Next(); err != nil {
			t.Fatalf("Next to track %d: %v", i+1, err)
		}
		_, cur := s.QueueSnapshot()
		if cur != i {
			t.Fatalf("curIdx = %d, want %d", cur, i)
		}
	}

	// Boundary: advancing past end returns ErrQueueEnd
	if err := s.Next(); err != ErrQueueEnd {
		t.Fatalf("Next at end = %v, want ErrQueueEnd", err)
	}

	// Step backwards
	for i := 48; i >= 0; i-- {
		if err := s.Previous(); err != nil {
			t.Fatalf("Previous to track %d: %v", i+1, err)
		}
		_, cur := s.QueueSnapshot()
		if cur != i {
			t.Fatalf("curIdx = %d, want %d", cur, i)
		}
	}

	// Boundary: moving past start returns ErrQueueStart
	if err := s.Previous(); err != ErrQueueStart {
		t.Fatalf("Previous at start = %v, want ErrQueueStart", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(switched) != 98 { // 49 forward + 49 backward
		t.Fatalf("switched count = %d, want 98", len(switched))
	}
}

// TestSessionQueueJumpAndRemoval tests direct index jumping and removing items.
func TestSessionQueueJumpAndRemoval(t *testing.T) {
	s := &Session{ID: "s-jump-rm", log: dummyLogger{}, mediaPage: &rod.Page{}}
	s.InitQueue("https://example.com/1.mp4", []string{
		"https://example.com/2.mp4",
		"https://example.com/3.mp4",
		"https://example.com/4.mp4",
		"https://example.com/5.mp4",
	})
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error {
		return nil
	})

	// Jump to index 3 (item 4)
	if err := s.JumpTo(3); err != nil {
		t.Fatalf("JumpTo(3): %v", err)
	}
	items, cur := s.QueueSnapshot()
	if cur != 3 {
		t.Fatalf("curIdx = %d, want 3", cur)
	}
	if items[cur].URL != "https://example.com/4.mp4" {
		t.Fatalf("cur URL = %q, want 4.mp4", items[cur].URL)
	}

	// Jump out of bounds
	if err := s.JumpTo(10); err == nil {
		t.Fatal("JumpTo(10) must fail for out of bounds")
	}

	// Remove item before current (item 2)
	id2 := items[1].ID
	if err := s.RemoveQueueItem(id2); err != nil {
		t.Fatalf("RemoveQueueItem: %v", err)
	}
	items, cur = s.QueueSnapshot()
	if len(items) != 4 {
		t.Fatalf("len = %d, want 4", len(items))
	}
	if cur != 2 { // shifted down from 3 to 2
		t.Fatalf("curIdx after rm before = %d, want 2", cur)
	}
	if items[cur].URL != "https://example.com/4.mp4" {
		t.Fatalf("current item shifted incorrectly: %q", items[cur].URL)
	}

	// Clear upcoming
	if err := s.ClearUpcoming(); err != nil {
		t.Fatalf("ClearUpcoming: %v", err)
	}
	items, cur = s.QueueSnapshot()
	if len(items) != 3 { // only 0, 1, 2 remain
		t.Fatalf("len after clear = %d, want 3", len(items))
	}
	if cur != 2 {
		t.Fatalf("curIdx after clear = %d, want 2", cur)
	}
}

// TestSessionQueueMediaLeases verifies that queued local files receive leases,
// transition to session leases on playback, and release leases on removal.
func TestSessionQueueMediaLeases(t *testing.T) {
	dir := t.TempDir()
	opts := media.DefaultOptions(dir)
	mgr, err := media.NewManager(opts, dummyLogger{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	// Create 3 dummy local files
	p1 := filepath.Join(dir, "t1.mp3")
	p2 := filepath.Join(dir, "t2.mp3")
	p3 := filepath.Join(dir, "t3.mp3")
	_ = os.WriteFile(p1, []byte("dummy1"), 0o644)
	_ = os.WriteFile(p2, []byte("dummy2"), 0o644)
	_ = os.WriteFile(p3, []byte("dummy3"), 0o644)

	e1, err := mgr.RegisterLocal("t1", p1, "t1", "audio/mpeg", 100)
	if err != nil {
		t.Fatalf("RegisterLocal 1: %v", err)
	}
	e2, err := mgr.RegisterLocal("t2", p2, "t2", "audio/mpeg", 100)
	if err != nil {
		t.Fatalf("RegisterLocal 2: %v", err)
	}
	e3, err := mgr.RegisterLocal("t3", p3, "t3", "audio/mpeg", 100)
	if err != nil {
		t.Fatalf("RegisterLocal 3: %v", err)
	}

	s := &Session{ID: "s-lease-test", log: dummyLogger{}, mgr: &Manager{mediaMgr: mgr}, mediaPage: &rod.Page{}}
	s.InitQueue(e1, []string{e2, e3})
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error {
		return nil
	})

	qOwner := s.queueOwner()
	// e2 and e3 must be leased by queueOwner
	owners2 := mgr.LeaseOwners("t2")
	if len(owners2) == 0 || owners2[0] != qOwner {
		t.Fatalf("t2 lease owners = %v, want [%s]", owners2, qOwner)
	}
	owners3 := mgr.LeaseOwners("t3")
	if len(owners3) == 0 || owners3[0] != qOwner {
		t.Fatalf("t3 lease owners = %v, want [%s]", owners3, qOwner)
	}

	// Advance to e2: e2 lease should transfer to session, e3 stays queueOwner
	if err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	sessOwner := s.leaseOwner()
	owners2 = mgr.LeaseOwners("t2")
	hasSess := false
	for _, o := range owners2 {
		if o == sessOwner {
			hasSess = true
			break
		}
	}
	if !hasSess {
		t.Fatalf("t2 after Next should have session lease %s, got %v", sessOwner, owners2)
	}

	// Remove e3 from queue: its queue lease must be released
	items, _ := s.QueueSnapshot()
	id3 := items[2].ID
	if err := s.RemoveQueueItem(id3); err != nil {
		t.Fatalf("RemoveQueueItem: %v", err)
	}
	owners3 = mgr.LeaseOwners("t3")
	if len(owners3) != 0 {
		t.Fatalf("t3 owners after removal = %v, want empty", owners3)
	}

	// Re-enqueue e3 and test ClearAllExceptCurrent
	_, err = s.Enqueue(e3)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	items, _ = s.QueueSnapshot()
	if len(items) != 3 {
		t.Fatalf("queue len = %d, want 3", len(items))
	}
	// ClearAllExceptCurrent should keep only e2 (the active one)
	if err := s.ClearAllExceptCurrent(); err != nil {
		t.Fatalf("ClearAllExceptCurrent: %v", err)
	}
	items, curIdx := s.QueueSnapshot()
	if len(items) != 1 || curIdx != 0 || items[0].URL != e2 {
		t.Fatalf("after ClearAllExceptCurrent: len=%d curIdx=%d items=%v", len(items), curIdx, items)
	}

	// Cleanup session: all remaining queue leases released
	s.cleanupQueue()
	owners2 = mgr.LeaseOwners("t2")
	for _, o := range owners2 {
		if o == qOwner {
			t.Fatalf("t2 still has queue lease after cleanup: %v", owners2)
		}
	}
}

// TestSessionQueueConcurrency validates concurrent calls to Next, Previous,
// Enqueue, and Snapshot do not trigger race conditions or deadlocks.
func TestSessionQueueConcurrency(t *testing.T) {
	s := &Session{ID: "s-race", log: dummyLogger{}, mediaPage: &rod.Page{}}
	initialExtra := make([]string, 10)
	for i := range initialExtra {
		initialExtra[i] = fmt.Sprintf("https://example.com/init-%d.mp4", i)
	}
	s.InitQueue("https://example.com/base.mp4", initialExtra)
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error {
		time.Sleep(2 * time.Millisecond)
		return nil
	})

	var wg sync.WaitGroup
	// 5 concurrent advance/prev workers
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if id%2 == 0 {
					_ = s.Next()
				} else {
					_ = s.Previous()
				}
				time.Sleep(1 * time.Millisecond)
			}
		}(w)
	}

	// 2 concurrent Enqueuers
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				_, _ = s.Enqueue(fmt.Sprintf("https://example.com/extra-%d-%d.mp4", id, i))
				time.Sleep(2 * time.Millisecond)
			}
		}(w)
	}

	// 2 concurrent readers
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				_, _ = s.QueueSnapshot()
				_ = s.Snapshot()
				time.Sleep(1 * time.Millisecond)
			}
		}()
	}

	wg.Wait()
	items, cur := s.QueueSnapshot()
	if len(items) == 0 {
		t.Fatal("queue should not be empty")
	}
	if cur < 0 || cur >= len(items) {
		t.Fatalf("curIdx %d out of bounds (0..%d)", cur, len(items)-1)
	}
}
