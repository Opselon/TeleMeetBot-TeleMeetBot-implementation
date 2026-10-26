package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func newTestPlaylistStore(t *testing.T) *PlaylistStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "playlists.db")
	ps, err := NewPlaylistStore(context.Background(), path)
	if err != nil {
		t.Fatalf("NewPlaylistStore: %v", err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	return ps
}

func TestPlaylistStoreCRUD(t *testing.T) {
	ps := newTestPlaylistStore(t)
	ctx := context.Background()
	ownerID := int64(1001)

	// Create
	pl, err := ps.Create(ctx, ownerID, "My Chill List", "Relaxing tracks")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pl.ID == 0 || pl.Name != "My Chill List" || pl.Description != "Relaxing tracks" {
		t.Fatalf("unexpected playlist: %+v", pl)
	}

	// Get
	got, err := ps.Get(ctx, ownerID, pl.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != pl.ID || got.Name != pl.Name {
		t.Fatalf("got %+v want %+v", got, pl)
	}

	// Rename
	renamed, err := ps.Rename(ctx, ownerID, pl.ID, "Upbeat List")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "Upbeat List" {
		t.Fatalf("expected name 'Upbeat List', got %q", renamed.Name)
	}

	// Describe
	if err := ps.Describe(ctx, ownerID, pl.ID, "New description"); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	updated, err := ps.Get(ctx, ownerID, pl.ID)
	if err != nil {
		t.Fatalf("Get after describe: %v", err)
	}
	if updated.Description != "New description" {
		t.Fatalf("expected 'New description', got %q", updated.Description)
	}

	// List
	list, err := ps.List(ctx, ownerID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != pl.ID {
		t.Fatalf("unexpected list: %+v", list)
	}

	// Duplicate
	dup, err := ps.Duplicate(ctx, ownerID, pl.ID)
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if dup.ID == pl.ID || !strings.Contains(dup.Name, "copy") {
		t.Fatalf("unexpected duplicate: %+v", dup)
	}

	// Delete
	if err := ps.Delete(ctx, ownerID, pl.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := ps.Get(ctx, ownerID, pl.ID); err != ErrPlaylistNotFound {
		t.Fatalf("expected ErrPlaylistNotFound, got %v", err)
	}
}

func TestPlaylistStoreItems(t *testing.T) {
	ps := newTestPlaylistStore(t)
	ctx := context.Background()
	ownerID := int64(1002)

	pl, err := ps.Create(ctx, ownerID, "Test Items", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// AddItem
	it1, err := ps.AddItem(ctx, ownerID, pl.ID, PlaylistItemInput{URL: "https://example.com/video1.mp4", Title: "Video 1"})
	if err != nil {
		t.Fatalf("AddItem 1: %v", err)
	}
	if it1.Position != 0 {
		t.Fatalf("expected position 0, got %d", it1.Position)
	}

	it2, err := ps.AddItem(ctx, ownerID, pl.ID, PlaylistItemInput{URL: "https://example.com/video2.mp4", Title: "Video 2"})
	if err != nil {
		t.Fatalf("AddItem 2: %v", err)
	}
	if it2.Position != 1 {
		t.Fatalf("expected position 1, got %d", it2.Position)
	}

	_, err = ps.AddItem(ctx, ownerID, pl.ID, PlaylistItemInput{URL: "https://example.com/video3.mp4", Title: "Video 3"})
	if err != nil {
		t.Fatalf("AddItem 3: %v", err)
	}

	// Items list
	items, err := ps.Items(ctx, ownerID, pl.ID)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	// MoveItem (move it2 up: delta -1)
	if err := ps.MoveItem(ctx, ownerID, pl.ID, it2.ID, -1); err != nil {
		t.Fatalf("MoveItem: %v", err)
	}
	itemsAfterMove, err := ps.Items(ctx, ownerID, pl.ID)
	if err != nil {
		t.Fatalf("Items after move: %v", err)
	}
	if itemsAfterMove[0].ID != it2.ID || itemsAfterMove[1].ID != it1.ID {
		t.Fatalf("items after move unexpected: %+v", itemsAfterMove)
	}

	// RemoveItem
	if err := ps.RemoveItem(ctx, ownerID, pl.ID, it1.ID); err != nil {
		t.Fatalf("RemoveItem: %v", err)
	}
	itemsAfterRemove, err := ps.Items(ctx, ownerID, pl.ID)
	if err != nil {
		t.Fatalf("Items after remove: %v", err)
	}
	if len(itemsAfterRemove) != 2 {
		t.Fatalf("expected 2 items, got %d", len(itemsAfterRemove))
	}

	// ClearItems
	cleared, err := ps.ClearItems(ctx, ownerID, pl.ID)
	if err != nil {
		t.Fatalf("ClearItems: %v", err)
	}
	if cleared != 2 {
		t.Fatalf("expected 2 cleared, got %d", cleared)
	}
}

func TestPlaylistStoreMultiTenantIsolation(t *testing.T) {
	ps := newTestPlaylistStore(t)
	ctx := context.Background()
	userA := int64(2001)
	userB := int64(2002)

	plA, err := ps.Create(ctx, userA, "User A List", "Private")
	if err != nil {
		t.Fatalf("Create userA: %v", err)
	}
	itA, err := ps.AddItem(ctx, userA, plA.ID, PlaylistItemInput{URL: "https://example.com/a.mp4", Title: "Track A"})
	if err != nil {
		t.Fatalf("AddItem userA: %v", err)
	}

	// User B tries to read User A's playlist
	if _, err := ps.Get(ctx, userB, plA.ID); err != ErrPlaylistNotFound {
		t.Fatalf("user B should not see user A's playlist: %v", err)
	}

	// User B tries to list User A's items
	if _, err := ps.Items(ctx, userB, plA.ID); err != ErrPlaylistNotFound {
		t.Fatalf("user B should not see user A's items: %v", err)
	}

	// User B tries to rename User A's playlist
	if _, err := ps.Rename(ctx, userB, plA.ID, "Hacked"); err != ErrPlaylistNotFound {
		t.Fatalf("user B should not rename user A's playlist: %v", err)
	}

	// User B tries to add an item to User A's playlist
	if _, err := ps.AddItem(ctx, userB, plA.ID, PlaylistItemInput{URL: "https://example.com/b.mp4", Title: "Track B"}); err != ErrPlaylistNotFound {
		t.Fatalf("user B should not add item to user A's playlist: %v", err)
	}

	// User B tries to delete an item from User A's playlist
	if err := ps.RemoveItem(ctx, userB, plA.ID, itA.ID); err != ErrPlaylistItemNotFound {
		t.Fatalf("user B should not delete item from user A's playlist: %v", err)
	}

	// User B tries to delete User A's playlist
	if err := ps.Delete(ctx, userB, plA.ID); err != ErrPlaylistNotFound {
		t.Fatalf("user B should not delete user A's playlist: %v", err)
	}
}
