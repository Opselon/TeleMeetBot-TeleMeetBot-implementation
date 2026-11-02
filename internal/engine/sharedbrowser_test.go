package engine

import (
	"sync"
	"testing"

	"github.com/go-rod/rod"
)

// TestSharedBrowserRefcountIsOnTheStruct is the regression test for the
// shared-browser refcount. The release closure snapshotted the counter into a
// per-acquire local and decremented that copy, so:
//
//   - the FIRST acquirer of a shared browser captured refs == 1. Releasing it
//     made its copy reach zero and closed the whole browser — while every
//     other session holding a reference was still using it;
//   - the struct's own counter was never decremented at all, so after that it
//     only ever grew and no later release could ever close the browser.
//
// Two sessions on one shared browser must keep it alive until BOTH release,
// and one close must happen exactly once. The fake launcher stands in for
// Chromium through the Manager's launchFn seam.
func TestSharedBrowserRefcountIsOnTheStruct(t *testing.T) {
	var mu sync.Mutex
	launches := 0
	closes := 0

	mgr := NewManager(BrowserConfig{PerSession: false}, MediaOptions{}, MeetOptions{
		GuestName: "Test Bot",
	}, dummyLogger{})
	mgr.launchFn = func(opts launchOptions) (*rod.Browser, func(), error) {
		mu.Lock()
		launches++
		mu.Unlock()
		return &rod.Browser{}, func() {
			mu.Lock()
			closes++
			mu.Unlock()
		}, nil
	}

	// Two sessions on the one shared browser.
	_, rel1, err := mgr.acquireBrowser()
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	_, rel2, err := mgr.acquireBrowser()
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if launches != 1 {
		t.Fatalf("expected one shared browser launch, got %d", launches)
	}
	if mgr.shared.refs != 2 {
		t.Fatalf("after two acquires the struct counter is %d, want 2", mgr.shared.refs)
	}

	// The first release must NOT close the browser: the second session is
	// still using it. This is exactly where the historical code failed —
	// the first acquirer's captured copy was 1.
	rel1()
	mu.Lock()
	closedAfterFirst := closes
	mu.Unlock()
	if closedAfterFirst != 0 {
		t.Fatalf("the first release closed the shared browser while another session was still using it")
	}
	mgr.sharedMu.Lock()
	refsAfterFirst := mgr.shared.refs
	mgr.sharedMu.Unlock()
	if refsAfterFirst != 1 {
		t.Fatalf("after one release the struct counter is %d, want 1", refsAfterFirst)
	}

	// The last release closes it exactly once.
	rel2()
	mu.Lock()
	closedAfterSecond := closes
	mu.Unlock()
	if closedAfterSecond != 1 {
		t.Fatalf("after the last release the browser was closed %d times, want exactly 1", closedAfterSecond)
	}
	mgr.sharedMu.Lock()
	sharedNil := mgr.shared == nil
	mgr.sharedMu.Unlock()
	if !sharedNil {
		t.Fatal("the shared slot was not cleared after the last release")
	}

	// A later acquire starts a fresh browser (the slot was cleared), so the
	// cycle repeats rather than wedging on a stale counter.
	_, rel3, err := mgr.acquireBrowser()
	if err != nil {
		t.Fatalf("acquire 3: %v", err)
	}
	rel3()
	mu.Lock()
	finalCloses := closes
	mu.Unlock()
	if finalCloses != 2 {
		t.Fatalf("after the second cycle the browser was closed %d times, want 2", finalCloses)
	}
}

// TestSharedBrowserReleaseIsIdempotent pins the sync.Once: a caller that
// invokes the release twice (a double-defer, a cleanup race) must not
// decrement the counter twice or close twice.
func TestSharedBrowserReleaseIsIdempotent(t *testing.T) {
	var mu sync.Mutex
	closes := 0

	mgr := NewManager(BrowserConfig{PerSession: false}, MediaOptions{}, MeetOptions{}, dummyLogger{})
	mgr.launchFn = func(opts launchOptions) (*rod.Browser, func(), error) {
		return &rod.Browser{}, func() {
			mu.Lock()
			closes++
			mu.Unlock()
		}, nil
	}

	_, rel, err := mgr.acquireBrowser()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel()
	rel()
	mu.Lock()
	c := closes
	mu.Unlock()
	if c != 1 {
		t.Fatalf("double release closed the browser %d times, want 1", c)
	}
	mgr.sharedMu.Lock()
	sharedNil := mgr.shared == nil
	mgr.sharedMu.Unlock()
	if !sharedNil {
		t.Fatal("the shared slot survived the releases")
	}
}
