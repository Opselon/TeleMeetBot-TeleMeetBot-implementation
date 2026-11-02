package media

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFileServerForgetRemovesTokens is the regression test for the loopback
// file server's unregistration path. forget() deleted using the path as the
// map key, but the map is keyed by token — the deletion was a silent no-op.
// Every re-download, re-registration or replay minted another valid token,
// the map grew for the process lifetime, and tokens for deleted or replaced
// files stayed resolvable (a replaced file answered under every old token).
func TestFileServerForgetRemovesTokens(t *testing.T) {
	f := newFileServer()
	defer f.close()

	dir := t.TempDir()
	path := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	entry := Entry{ID: "e1", Path: path, ContentType: "video/mp4"}

	url1 := f.serve(entry)
	if url1 == "" {
		t.Fatalf("serve returned no URL")
	}

	// forget() must actually remove the token: before the fix the deletion
	// used the path as the key against a token-keyed map and nothing was
	// ever removed.
	f.forget(path)
	if got := f.pathOf(url1); got != "" {
		t.Fatalf("after forget, the token still resolves to %q, want empty", got)
	}
	if len(f.tokens) != 0 {
		t.Fatalf("token map has %d entries after forget, want 0", len(f.tokens))
	}
}

// TestFileServerServeIsIdempotentPerPath keeps the re-registration path
// bounded: serving the same path twice must not mint a second token (the
// historical behaviour grew the map without limit), while a different path
// still gets its own token.
func TestFileServerServeIsIdempotentPerPath(t *testing.T) {
	f := newFileServer()
	defer f.close()

	dir := t.TempDir()
	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	ea := Entry{ID: "ea", Path: a, ContentType: "video/mp4"}
	eb := Entry{ID: "eb", Path: b, ContentType: "video/mp4"}

	u1 := f.serve(ea)
	u2 := f.serve(ea)
	if u1 != u2 {
		t.Fatalf("serve() on the same path returned %q then %q, want the same URL", u1, u2)
	}
	ub := f.serve(eb)
	if ub == u1 {
		t.Fatalf("a different path got the same URL %q", ub)
	}
	if len(f.tokens) != 2 {
		t.Fatalf("token map has %d entries for two distinct paths, want 2", len(f.tokens))
	}

	// forget(a) must remove only a's token.
	f.forget(a)
	if got := f.pathOf(u1); got != "" {
		t.Fatalf("forget left a's token resolvable: %q", got)
	}
	if got := f.pathOf(ub); got != b {
		t.Fatalf("forget(a) also removed b's token: pathOf(b) = %q, want %q", got, b)
	}
}
