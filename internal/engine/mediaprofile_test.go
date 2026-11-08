package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPersistentProfileDirIsolatedPerUser pins the trust boundary of the
// persistent Chromium profile: two users must resolve to two different
// directories, a hostile owner string must never escape the root, and an
// empty root or owner must keep the historical throwaway profile.
func TestPersistentProfileDirIsolatedPerUser(t *testing.T) {
	root := t.TempDir()

	a, ok := persistentProfileDir(root, "5094837833")
	if !ok {
		t.Fatal("a configured root and owner must resolve")
	}
	b, ok := persistentProfileDir(root, "6922171628")
	if !ok {
		t.Fatal("a second owner must resolve")
	}
	if a == b {
		t.Fatalf("two users share one profile directory: %q", a)
	}
	if dir, _ := os.Stat(a); dir == nil || !dir.IsDir() {
		t.Fatalf("the profile directory must be created 0700: %s", a)
	}

	// A hostile owner cannot escape the root through path traversal or case
	// tricks: everything outside [a-z0-9-_] collapses to an underscore.
	if got, ok := persistentProfileDir(root, "../../../etc/passwd"); !ok || !filepath.HasPrefix(got, root+string(os.PathSeparator)) {
		t.Fatalf("a traversal-shaped owner escaped the root: %q (ok=%v)", got, ok)
	}
	if got := persistentProfileOf(t, root, "UPPER-CASE"); filepath.Clean(got) != filepath.Clean(filepath.Join(root, "upper-case")) {
		t.Fatalf("an upper-case owner must be lower-cased, got %q", got)
	}

	// Empty root or owner = the historical throwaway profile, so the flag is
	// strictly opt-in.
	if _, ok := persistentProfileDir("", "5094837833"); ok {
		t.Fatal("an empty root must keep the throwaway profile")
	}
	if _, ok := persistentProfileDir(root, ""); ok {
		t.Fatal("an empty owner must keep the throwaway profile")
	}
}

func persistentProfileOf(t *testing.T, root, owner string) string {
	t.Helper()
	dir, ok := persistentProfileDir(root, owner)
	if !ok {
		t.Fatalf("owner %q must resolve", owner)
	}
	return dir
}

// TestApplyPersistentProfileUsesOwnerKey pins the launch wiring: with
// browser.profile_dir configured, the launch's user-data-dir must be the
// owner's directory; without an owner it must stay on the temp profile.
func TestApplyPersistentProfileUsesOwnerKey(t *testing.T) {
	root := t.TempDir()
	mgr := NewManager(
		BrowserConfig{PerSession: true, ProfileDir: root},
		MediaOptions{},
		MeetOptions{GuestName: "T"},
		dummyLogger{},
	)
	mgr.mu.Lock()
	mgr.profileOwnerKey = mgr.cleanProfileOwner("5094837833")
	mgr.mu.Unlock()

	opts := launchOptions{browser: mgr.browserCfg}
	mgr.applyPersistentProfile(&opts)
	want := filepath.Join(root, "5094837833")
	if opts.userData != want {
		t.Fatalf("launch userData = %q, want the owner profile %q", opts.userData, want)
	}

	// No owner (fresh manager) and the temp profile must survive untouched.
	mgr2 := NewManager(
		BrowserConfig{PerSession: true, ProfileDir: root},
		MediaOptions{},
		MeetOptions{GuestName: "T"},
		dummyLogger{},
	)
	opts2 := launchOptions{browser: mgr2.browserCfg, userData: "/tmp/profile-x"}
	mgr2.applyPersistentProfile(&opts2)
	if opts2.userData != "/tmp/profile-x" {
		t.Fatalf("persistent profile must not overwrite an explicit profile when no owner is set, got %q", opts2.userData)
	}
}

// TestCleanProfileOwnerSanitises pins the boundary sanitiser: only
// [a-z0-9-_] survives, so an owner tag can never become a path.
func TestCleanProfileOwnerSanitises(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{}, dummyLogger{})
	for _, in := range []string{
		"5094837833",
		"5094837833 ",
		"../etc/passwd",
		"aCme.USER-1_x",
		"",
	} {
		got := mgr.cleanProfileOwner(in)
		for _, r := range got {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' {
				t.Fatalf("cleanProfileOwner(%q) = %q: only [a-z0-9-_] may survive", in, got)
			}
		}
	}
	if got := mgr.cleanProfileOwner("5094837833"); got != "5094837833" {
		t.Fatalf("an already-clean owner must round-trip, got %q", got)
	}
	// Dots collapse to underscores too: the owner is a directory NAME, never a
	// path, and "acme.user" would otherwise be a hidden directory.
	if got := mgr.cleanProfileOwner("aCme.USER-1_x"); got != "acme_user-1_x" {
		t.Fatalf("upper case must be folded and punctuation collapsed, got %q", got)
	}
}
