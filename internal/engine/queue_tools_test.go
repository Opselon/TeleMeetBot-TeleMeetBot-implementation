package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-rod/rod"
)

func TestQueueToolsMoveAndReorder(t *testing.T) {
	s := &Session{ID: "s-tools-1", log: dummyLogger{}, mediaPage: &rod.Page{}}
	urls := []string{
		"https://www.youtube.com/watch?v=track01",
		"https://www.youtube.com/watch?v=track02",
		"https://www.youtube.com/watch?v=track03",
	}
	s.InitQueue(urls[0], urls[1:])

	items, curIdx := s.QueueSnapshot()
	if len(items) != 3 || curIdx != 0 {
		t.Fatalf("expected 3 items, curIdx 0; got len=%d curIdx=%d", len(items), curIdx)
	}

	// Move first item backwards (-1): should fail because it's already first
	if err := s.MoveQueueItem(items[0].ID, -1); err == nil {
		t.Fatal("expected error moving first item backwards")
	}

	// Move second item up (-1): swaps with first item
	item2ID := items[1].ID
	if err := s.MoveQueueItem(item2ID, -1); err != nil {
		t.Fatalf("MoveQueueItem -1: %v", err)
	}

	itemsAfterMove, curIdxAfterMove := s.QueueSnapshot()
	if itemsAfterMove[0].ID != item2ID {
		t.Fatalf("expected item2 to be at index 0, got %s", itemsAfterMove[0].ID)
	}
	// Since current item was index 0 and was swapped to index 1, curIdx should follow it to 1
	if curIdxAfterMove != 1 {
		t.Fatalf("expected curIdx to follow playing item to 1, got %d", curIdxAfterMove)
	}
}

func TestQueueToolsSortUpcoming(t *testing.T) {
	s := &Session{ID: "s-tools-sort", log: dummyLogger{}, mediaPage: &rod.Page{}}
	urls := []string{
		"https://www.youtube.com/watch?v=now_playing",
		"https://www.youtube.com/watch?v=zebra",
		"https://www.youtube.com/watch?v=apple",
		"https://www.youtube.com/watch?v=banana",
	}
	s.InitQueue(urls[0], urls[1:])

	// Manually set titles on upcoming items to test sort
	s.queueMu.Lock()
	s.queue[1].Title = "Zebra Track"
	s.queue[2].Title = "Apple Track"
	s.queue[3].Title = "Banana Track"
	s.queueMu.Unlock()

	n := s.SortUpcoming()
	if n != 3 {
		t.Fatalf("expected 3 items sorted, got %d", n)
	}

	items, _ := s.QueueSnapshot()
	if !strings.Contains(items[0].URL, "now_playing") {
		t.Fatalf("currently playing item should remain unchanged: %s", items[0].URL)
	}
	if items[1].Title != "Apple Track" || items[2].Title != "Banana Track" || items[3].Title != "Zebra Track" {
		t.Fatalf("upcoming items not sorted properly: 1=%s, 2=%s, 3=%s", items[1].Title, items[2].Title, items[3].Title)
	}
}

func TestQueueToolsEnqueueNextAndReplaceUpcoming(t *testing.T) {
	s := &Session{ID: "s-tools-enqueue", log: dummyLogger{}, mediaPage: &rod.Page{}}
	urls := []string{
		"https://www.youtube.com/watch?v=t1",
		"https://www.youtube.com/watch?v=t2",
	}
	s.InitQueue(urls[0], urls[1:])

	// EnqueueNext inserts right after currently playing item (index 0)
	added, err := s.EnqueueNext("https://www.youtube.com/watch?v=t_next")
	if err != nil {
		t.Fatalf("EnqueueNext: %v", err)
	}
	if len(added) != 1 {
		t.Fatalf("expected 1 item added, got %d", len(added))
	}

	items, _ := s.QueueSnapshot()
	if len(items) != 3 {
		t.Fatalf("expected 3 items total, got %d", len(items))
	}
	if !strings.Contains(items[1].URL, "t_next") {
		t.Fatalf("expected items[1] to be t_next, got %s", items[1].URL)
	}

	// ReplaceUpcoming: replaces everything after currently playing item
	replacedCount, err := s.ReplaceUpcoming([]string{
		"https://www.youtube.com/watch?v=new1",
		"https://www.youtube.com/watch?v=new2",
	})
	if err != nil {
		t.Fatalf("ReplaceUpcoming: %v", err)
	}
	if replacedCount != 2 {
		t.Fatalf("expected 2 replaced, got %d", replacedCount)
	}

	itemsAfterReplace, _ := s.QueueSnapshot()
	if len(itemsAfterReplace) != 3 {
		t.Fatalf("expected 3 items (1 current + 2 replaced), got %d", len(itemsAfterReplace))
	}
	if !strings.Contains(itemsAfterReplace[0].URL, "t1") {
		t.Fatalf("first item should still be t1, got %s", itemsAfterReplace[0].URL)
	}
	if !strings.Contains(itemsAfterReplace[1].URL, "new1") {
		t.Fatalf("second item should be new1, got %s", itemsAfterReplace[1].URL)
	}

	// QueueURLs
	qURLs := s.QueueURLs()
	if len(qURLs) != 3 || !strings.Contains(qURLs[1], "new1") {
		t.Fatalf("unexpected QueueURLs: %v", qURLs)
	}

	// QueueItemByID
	foundItem, pos, ok := s.QueueItemByID(itemsAfterReplace[1].ID)
	if !ok || pos != 1 || !strings.Contains(foundItem.URL, "new1") {
		t.Fatalf("QueueItemByID failed: ok=%v pos=%d item=%+v", ok, pos, foundItem)
	}

	// QueueTotalDuration
	s.queueMu.Lock()
	s.queue[0].Duration = 100.5
	s.queue[1].Duration = 200.5
	s.queueMu.Unlock()
	if dur := s.QueueTotalDuration(); dur != 301.0 {
		t.Fatalf("expected duration 301.0, got %f", dur)
	}
}

func TestQueueToolsShuffleUpcoming(t *testing.T) {
	s := &Session{ID: "s-tools-shuffle", log: dummyLogger{}, mediaPage: &rod.Page{}}
	extra := make([]string, 20)
	for i := 0; i < 20; i++ {
		extra[i] = fmt.Sprintf("https://www.youtube.com/watch?v=track%02d", i+2)
	}
	s.InitQueue("https://www.youtube.com/watch?v=track01", extra)

	n := s.ShuffleUpcoming()
	if n != 20 {
		t.Fatalf("expected 20 items shuffled, got %d", n)
	}
	items, curIdx := s.QueueSnapshot()
	if curIdx != 0 || !strings.Contains(items[0].URL, "track01") {
		t.Fatalf("current item must remain unchanged at index 0")
	}
	if len(items) != 21 {
		t.Fatalf("total items must remain 21, got %d", len(items))
	}
}
