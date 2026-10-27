package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testManager builds a manager rooted at a fresh temp dir with a TTL and
// limits suited to tests (seconds, not hours).
func testManager(t *testing.T, ttl time.Duration) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.TTL = ttl
	opts.Timeout = 30 * time.Second
	opts.HTTPTimeout = 10 * time.Second
	opts.MaxConcurrent = 2
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Pin DNS so the suite is deterministic on networks that resolve public
	// hosts to private addresses (captive networks intercept domain lookups,
	// which would trip the SSRF gate for reasons unrelated to the test).
	// Tests that exercise DNS behaviour override this seam themselves.
	m.lookupFn = func(host string) ([]net.IP, error) {
		if host == "internal.example.com" || host == "private.example.com" {
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	t.Cleanup(func() { m.Close() })
	return m, dir
}

type testLog struct{ t *testing.T }

func (l *testLog) Debugf(f string, a ...interface{}) { l.t.Logf("DEBUG "+f, a...) }
func (l *testLog) Infof(f string, a ...interface{})  { l.t.Logf("INFO "+f, a...) }
func (l *testLog) Warnf(f string, a ...interface{})  { l.t.Logf("WARN "+f, a...) }
func (l *testLog) Errorf(f string, a ...interface{}) { l.t.Logf("ERROR "+f, a...) }

// serveMedia starts an httptest server returning a small MP4 payload.
func serveMedia(t *testing.T, status int, contentType string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if status != 0 {
			w.WriteHeader(status)
		} else {
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// smallMP4 is a tiny but non-empty payload.
var smallMP4 = bytes()

func bytes() []byte {
	b := make([]byte, 4096)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// ---- the four mandated expiry proofs ---------------------------------------

// testAllowLoopback enables loopback destinations for the duration of a test
// so an httptest fixture can stand in for a remote media host. Production
// never sets this: a Telegram-supplied URL pointing at 127.0.0.1 must be
// refused.
func testAllowLoopback(t *testing.T) {
	t.Helper()
	allowLoopback = true
	t.Cleanup(func() { allowLoopback = false })
}

// TestFileDeletedAfterExpiry proves a downloaded file is gone once its TTL
// passes and nothing holds a lease on it, and that the manifest stops tracking
// it. This is the orphan-reclamation half of the TTL policy.
func TestFileDeletedAfterExpiry(t *testing.T) {
	testAllowLoopback(t)
	m, dir := testManager(t, 500*time.Millisecond)
	srv := serveMedia(t, 0, "video/mp4", smallMP4)

	res, err := m.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "clip"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("downloaded file missing immediately after fetch: %v", err)
	}
	if res.ExpiresAt.IsZero() {
		t.Fatal("expiry deadline not reported")
	}

	waitFor := func(cond func() bool, timeout time.Duration, msg string) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal(msg)
	}

	// Sweep after expiry: the file MUST be deleted (no lease holds it).
	waitFor(func() bool {
		n := m.Sweep(context.Background())
		_, err := os.Stat(res.Path)
		return n > 0 && os.IsNotExist(err)
	}, 5*time.Second, "expired file was not deleted by Sweep")

	if _, ok := m.entries.Get("clip"); ok {
		t.Fatal("manifest still tracks an expired entry")
	}
	if _, err := os.Stat(filepath.Join(dir, "downloads.json")); err == nil {
		// The manifest must still exist (it is valid, merely empty-ish) but
		// must not reference the deleted file.
		raw, _ := os.ReadFile(filepath.Join(dir, "downloads.json"))
		if strings.Contains(string(raw), `"clip"`) {
			t.Fatal("manifest still references the deleted file")
		}
	}
}

// TestLeaseKeepsExpiredFileAndReleaseReclaimsIt proves both halves of the
// lifecycle in one flow: an expired-but-leased file survives the janitor, and
// the moment its owner releases the lease, the file is reclaimed.
func TestLeaseKeepsExpiredFileAndReleaseReclaimsIt(t *testing.T) {
	testAllowLoopback(t)
	m, _ := testManager(t, 300*time.Millisecond)
	srv := serveMedia(t, 0, "video/mp4", smallMP4)

	res, err := m.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "pinned"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !m.Lease("pinned", "owner-a") {
		t.Fatal("Lease on a live entry must succeed")
	}

	waitFor := func(cond func() bool, msg string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			m.Sweep(context.Background())
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal(msg)
	}

	// Past the deadline the janitor must NOT delete the leased file.
	waitFor(func() bool { return time.Now().After(res.ExpiresAt) }, "deadline never passed")
	for i := 0; i < 5; i++ {
		if n := m.Sweep(context.Background()); n > 0 {
			t.Fatalf("janitor deleted a leased file (%d removed)", n)
		}
		time.Sleep(60 * time.Millisecond)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("leased file was deleted after its deadline: %v", err)
	}
	if owners := m.LeaseOwners("pinned"); len(owners) != 1 || owners[0] != "owner-a" {
		t.Fatalf("LeaseOwners = %v, want [owner-a]", owners)
	}

	// Releasing the expired entry removes it at once: a media switch is a
	// prompt cleanup, not a deferred one.
	if !m.Release("pinned", "owner-a") {
		t.Fatal("Release must remove an expired, unowned entry")
	}
	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Fatal("an expired file whose lease was released was not deleted")
	}
}

// TestReleaseKeepsFreshFileForJanitor proves a non-forced release does NOT
// delete a file whose deadline has not passed: a still-fresh file belongs to
// the janitor and its deadline, not to the releasing caller.
func TestReleaseKeepsFreshFileForJanitor(t *testing.T) {
	testAllowLoopback(t)
	m, _ := testManager(t, time.Minute) // comfortably longer than this test
	srv := serveMedia(t, 0, "video/mp4", smallMP4)

	res, err := m.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "fresh"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !m.Lease("fresh", "owner-a") {
		t.Fatal("Lease must succeed")
	}
	if m.Release("fresh", "owner-a") {
		t.Fatal("Release deleted a file whose deadline had not passed")
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("a fresh file was deleted by a non-forced release: %v", err)
	}
	if owners := m.LeaseOwners("fresh"); len(owners) != 0 {
		t.Fatalf("the lease was not dropped: %v", owners)
	}
	// The entry is unleased and fresh: the janitor still owns it, so it must
	// still be tracked.
	if _, ok := m.entries.Get("fresh"); !ok {
		t.Fatal("a fresh entry disappeared from the manifest after a release")
	}
}

// TestExpiredFilesCleanedAfterRestart proves a file that expired while the
// bot was down is removed on the next startup.
func TestExpiredFilesCleanedAfterRestart(t *testing.T) {
	testAllowLoopback(t)
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.TTL = time.Hour
	m1, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	srv := serveMedia(t, 0, "video/mp4", smallMP4)
	res, err := m1.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "clip"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	m1.Close()

	// Simulate the process being down past the deadline: backdate the entry
	// so it is already expired when the next process starts.
	manifestPath := filepath.Join(dir, "downloads.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var man manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	past := time.Now().Add(-2 * time.Hour)
	for i := range man.Entries {
		man.Entries[i].FetchedAt = past
		man.Entries[i].ExpiresAt = past.Add(time.Hour)
	}
	out, _ := json.MarshalIndent(manifest{Version: manifestVersion, Entries: man.Entries}, "", "  ")
	if err := os.WriteFile(manifestPath, out, 0o600); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}

	// A fresh manager (the "restart") sweeps on Start before anything else.
	m2, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager(restart): %v", err)
	}
	t.Cleanup(func() { m2.Close() })
	m2.Start(context.Background())

	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Fatalf("expired file survived a restart: %v", err)
	}
	if _, ok := m2.entries.Get("clip"); ok {
		t.Fatal("restart kept an expired entry in the manifest")
	}
}

// TestActiveLeaseRetainsExpiredFile proves the fix, not the bug: a leased
// file survives past its deadline for as long as a session is using it, no
// matter how many janitor sweeps run. Expiry is an orphan-cleanup policy, not
// a hard deadline that destroys media an active session is streaming.
func TestActiveLeaseRetainsExpiredFile(t *testing.T) {
	testAllowLoopback(t)
	m, _ := testManager(t, 400*time.Millisecond)
	srv := serveMedia(t, 0, "video/mp4", smallMP4)
	res, err := m.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "clip"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !m.Lease(res.ID, "session-a") {
		t.Fatal("Lease must succeed for a live entry")
	}
	// "Playback" holds a handle for far longer than the TTL.
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(start)
		f, err := os.Open(res.Path)
		if err != nil {
			return
		}
		defer f.Close()
		time.Sleep(2 * time.Second) // simulate an active, long session
	}()
	<-start

	// Run the janitor every tick past the deadline: the file MUST survive.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n := m.Sweep(context.Background()); n > 0 {
			wg.Wait()
			t.Fatalf("janitor deleted an actively leased file (%d removed)", n)
		}
		if _, err := os.Stat(res.Path); os.IsNotExist(err) {
			wg.Wait()
			t.Fatal("an actively leased file was deleted after its deadline")
		}
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("leased file vanished while its session was still active: %v", err)
	}

	// And once the session ends, the file is reclaimed promptly.
	if !m.ReleaseAndDelete(res.ID, "session-a") {
		t.Fatal("ReleaseAndDelete must remove an unowned expired file")
	}
	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Fatal("expired file was not deleted after its owning session released it")
	}
}

// TestMultipleSessionsShareLease proves two sessions can own the same entry:
// the first teardown keeps the file alive for the second, and only the last
// owner's release removes it. This is the multiple-session-ownership safety
// case (e.g. two sessions streaming the same Telegram-ingested file).
func TestMultipleSessionsShareLease(t *testing.T) {
	testAllowLoopback(t)
	m, _ := testManager(t, 400*time.Millisecond)
	srv := serveMedia(t, 0, "video/mp4", smallMP4)
	res, err := m.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "shared"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !m.Lease(res.ID, "owner-a") || !m.Lease(res.ID, "owner-b") {
		t.Fatal("both owners must be able to lease the entry")
	}
	if got := len(m.LeaseOwners(res.ID)); got != 2 {
		t.Fatalf(" LeaseOwners = %d, want 2", got)
	}

	// First session stops: the file must survive for the second session.
	if m.ReleaseAndDelete(res.ID, "owner-a") {
		t.Fatal("the file must not be removed while a second owner holds it")
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("file was deleted while a second session still held a lease: %v", err)
	}

	// Past the deadline the janitor still respects the remaining owner.
	for time.Now().Before(res.ExpiresAt) {
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		if n := m.Sweep(context.Background()); n > 0 {
			t.Fatalf("janitor deleted a file with one active lease left (%d removed)", n)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The last owner's teardown removes it.
	if !m.ReleaseAndDelete(res.ID, "owner-b") {
		t.Fatal("the last owner's release must remove the file")
	}
	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Fatal("the file survived its last owner's teardown")
	}
}

// TestLeaseOnUnknownEntryHarmless proves leases are best-effort pins: leasing
// an entry that no longer exists (janitor race, restart) is a no-op that
// never panics and never resurrects a file.
func TestLeaseOnUnknownEntryHarmless(t *testing.T) {
	m, _ := testManager(t, time.Minute)
	if m.Lease("does-not-exist", "owner-a") {
		t.Fatal("Lease on an unknown entry must report failure")
	}
	if owners := m.LeaseOwners("does-not-exist"); len(owners) != 0 {
		t.Fatalf("LeaseOwners on an unknown entry = %v, want none", owners)
	}
	// Releasing an unknown entry must not panic or report a removal.
	if m.Release("does-not-exist", "owner-a") {
		t.Fatal("Release on an unknown entry must report no removal")
	}
	if m.ReleaseAndDelete("does-not-exist", "owner-a") {
		t.Fatal("ReleaseAndDelete on an unknown entry must report no removal")
	}
	// Nil/empty args must be tolerated (defensive: the engine calls these on
	// every teardown).
	var nilMgr *Manager
	if nilMgr.Lease("x", "y") || nilMgr.Release("x", "y") || nilMgr.ReleaseAndDelete("x", "y") {
		t.Fatal("a nil manager must not report a removal")
	}
}

// TestRestartDropsStaleLeases proves crash recovery: leases live in memory
// only, so after a restart no pin protects an orphaned file and the startup
// sweep reclaims it.
func TestRestartDropsStaleLeases(t *testing.T) {
	testAllowLoopback(t)
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.TTL = time.Hour
	m1, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	srv := serveMedia(t, 0, "video/mp4", smallMP4)
	res, err := m1.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "clip"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !m1.Lease("clip", "owner-a") {
		t.Fatal("lease must succeed while the process is up")
	}
	// Backdate the entry so the restart sees it as expired.
	m1.entries.RemoveIf(func(e Entry) bool { return true })
	if err := m1.entries.Add(Entry{
		ID: "clip", Path: res.Path, URL: srv.URL, Kind: "direct",
		ContentType: "video/mp4", Bytes: 4096,
		FetchedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("backdate entry: %v", err)
	}
	m1.Close()

	// The "restart": a fresh manager shares no memory with the dead one, so
	// the orphaned lease does not protect the file.
	m2, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager(restart): %v", err)
	}
	t.Cleanup(func() { m2.Close() })
	m2.Start(context.Background())

	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Fatalf("an orphaned file protected by a stale (in-memory) lease survived a restart: %v", err)
	}
	if _, ok := m2.entries.Get("clip"); ok {
		t.Fatal("restart kept an expired entry in the manifest")
	}
}

// TestUnrelatedFilesNeverDeleted proves the janitor only removes files it
// manages: sibling files, nested directories and an escape symlink survive.
func TestUnrelatedFilesNeverDeleted(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.TTL = 300 * time.Millisecond
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	// An unrelated file in the SAME directory, off-manifest.
	sibling := filepath.Join(dir, "operator-backup.mp4")
	if err := os.WriteFile(sibling, []byte("not mine"), 0o600); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	// An unrelated file OUTSIDE the directory.
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte("definitely not mine"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	// A symlink inside the managed dir pointing outside, registered as a
	// managed entry: the delete must refuse the symlink target.
	symlink := filepath.Join(dir, "linked.mp4")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	m.entries.Add(Entry{
		ID: "linked", Path: symlink, URL: "http://example/x", Kind: "direct",
		ContentType: "video/mp4", Bytes: 4, FetchedAt: time.Now().Add(-time.Hour),
		ExpiresAt: time.Now().Add(-time.Minute),
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.Sweep(context.Background())
		if _, err := os.Stat(sibling); os.IsNotExist(err) {
			t.Fatal("janitor deleted an unrelated sibling file")
		}
		if _, err := os.Stat(outside); os.IsNotExist(err) {
			t.Fatal("janitor deleted a file outside the managed dir (via symlink)")
		}
		if _, err := os.Lstat(symlink); !os.IsNotExist(err) {
			// The symlink itself may go (it is registered); its TARGET must not.
			_ = err
		}
		if _, ok := m.entries.Get("linked"); !ok {
			break // entry reaped; targets verified above each iteration
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling file was deleted: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file was deleted via a symlink: %v", err)
	}
}

// ---- security --------------------------------------------------------------

// TestSSRFRejected proves private destinations never reach the network.
func TestSSRFRejected(t *testing.T) {
	m, _ := testManager(t, time.Minute)

	for _, raw := range []string{
		"http://127.0.0.1:18080/secret",
		"http://localhost/x.mp4",
		"http://10.0.0.1/x.mp4",
		"http://192.168.1.1/x.mp4",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://[::1]/x.mp4",
		"ftp://example.com/x.mp4",       // non-http scheme
		"http://user:pass@example.com/", // embedded credentials
		"",
	} {
		if _, err := m.Download(context.Background(), DownloadRequest{URL: raw, Kind: "direct", ID: "x"}); err == nil {
			t.Fatalf("Download accepted unsafe URL %q", raw)
		}
	}
}

// TestSSRFViaDNS proves a friendly hostname resolving to a private address is
// refused before the fetch.
func TestSSRFViaDNS(t *testing.T) {
	m, _ := testManager(t, time.Minute)
	m.lookupFn = func(host string) ([]net.IP, error) {
		if host == "internal.example.com" {
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		}
		return net.LookupIP(host)
	}
	_, err := m.Download(context.Background(), DownloadRequest{URL: "http://internal.example.com/x.mp4", Kind: "direct", ID: "x"})
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("expected a private-resolution error, got %v", err)
	}
}

// TestPathTraversalRejected proves traversal and symlink escapes cannot make
// the manager delete (or write) outside the managed dir.
func TestPathTraversalRejected(t *testing.T) {
	_, dir := testManager(t, time.Minute)
	outsideDir := t.TempDir()
	victim := filepath.Join(outsideDir, "victim.txt")
	if err := os.WriteFile(victim, []byte("important"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../victim.txt", "..", ".", "/etc/passwd", "sub/dir/x", "a\\..\\b"} {
		if _, err := SafeJoin(dir, name); err == nil {
			t.Fatalf("SafeJoin accepted traversal name %q", name)
		}
	}
	// SecureDelete must refuse a path outside the managed dir.
	if _, err := SecureDelete(dir, victim); err == nil {
		t.Fatal("SecureDelete accepted a path outside the managed dir")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim file was removed by SecureDelete: %v", err)
	}
}

// TestMissingToolIsActionable proves a missing yt-dlp surfaces as an
// actionable ErrMissingTool, not a crash, and that the manager stays usable.
func TestMissingToolIsActionable(t *testing.T) {
	m, _ := testManager(t, time.Minute)
	m.opts.YTDLPPath = "/nonexistent/yt-dlp"
	_, err := m.Download(context.Background(), DownloadRequest{URL: "https://www.youtube.com/watch?v=abc123", Kind: "youtube", ID: "x"})
	if err == nil {
		t.Fatal("expected an error for a missing yt-dlp")
	}
	var missing *ErrMissingTool
	if !errors.As(err, &missing) {
		t.Fatalf("expected ErrMissingTool, got %T: %v", err, err)
	}
	if !strings.Contains(missing.Error(), "yt-dlp") {
		t.Fatalf("missing-tool error does not name yt-dlp: %v", missing)
	}
}

// TestSizeLimitEnforced proves a file larger than the ceiling is refused
// before the bytes are committed to disk.
func TestSizeLimitEnforced(t *testing.T) {
	testAllowLoopback(t)
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.TTL = time.Minute
	opts.MaxBytes = 1024
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	big := make([]byte, 64*1024)
	srv := serveMedia(t, 0, "video/mp4", big)
	_, err = m.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "x"})
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected a size-limit error, got %v", err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != "downloads.json" && !strings.HasPrefix(e.Name(), "downloads.json.") {
			t.Fatalf(" Oversized download left a file behind: %s", e.Name())
		}
	}
}

// TestDownloadCancel proves cancellation is honoured and the temp file gone.
func TestDownloadCancel(t *testing.T) {
	testAllowLoopback(t)
	m, dir := testManager(t, time.Minute)
	// A slow server: never finishes writing within the test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		flusher, _ := w.(http.Flusher)
		ctx := r.Context()
		for i := 0; i < 1000; i++ {
			select {
			case <-ctx.Done():
				return // the client cancelled: release the connection
			default:
			}
			_, _ = w.Write(make([]byte, 1024))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err := m.Download(ctx, DownloadRequest{URL: srv.URL, Kind: "direct", ID: "x"})
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				count++
			}
		}
		if count == 0 {
			return
		}
		m.sweepTempFiles()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("temp file from a cancelled download was not cleaned up")
}

// TestProgressReporting proves progress events are emitted mid-download.
func TestProgressReporting(t *testing.T) {
	testAllowLoopback(t)
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.TTL = time.Minute
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	var seen []Progress
	unsub := m.SubscribeProgress(func(p Progress) { seen = append(seen, p) })
	defer unsub()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			_, _ = w.Write(make([]byte, 64*1024))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	if _, err := m.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "x"}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("no progress events emitted")
	}
	last := seen[len(seen)-1]
	if last.Done != true {
		t.Errorf("expected the last progress event to be done")
	}
}

// TestDownloadsAreBounded proves the concurrency gate serializes fetches.
func TestDownloadsAreBounded(t *testing.T) {
	testAllowLoopback(t)
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.TTL = time.Minute
	opts.MaxConcurrent = 1
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Close() })

	var active, peak int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(smallMP4)
		mu.Lock()
		active--
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = m.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: fmt.Sprintf("c%d", i)})
		}(i)
	}
	wg.Wait()
	if peak > 1 {
		t.Fatalf("concurrency gate failed: %d downloads ran in parallel (limit 1)", peak)
	}
}

// TestDisabledKeepsBrowserMode proves a disabled manager reports ErrDisabled
// instead of affecting anything.
func TestDisabledKeepsBrowserMode(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Enabled = false
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m.Enabled() {
		t.Fatal("manager reports enabled while configured off")
	}
	_, err = m.Download(context.Background(), DownloadRequest{URL: "http://example.com/x.mp4", Kind: "direct"})
	if err != ErrDisabled {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
}

// TestCorruptManifestQuarantined proves a damaged downloads.json does not stop
// the bot from starting; it is renamed aside and the store starts empty.
func TestCorruptManifestQuarantined(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "downloads.json")
	if err := os.WriteFile(manifest, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(DefaultOptions(dir), &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager with a corrupt manifest: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	if got := len(m.entries.All()); got != 0 {
		t.Fatalf("expected an empty store after quarantine, got %d entries", got)
	}
	ents, _ := os.ReadDir(dir)
	found := false
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "downloads.json.corrupt-") {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt manifest was not quarantined")
	}
}

// ---- helpers ---------------------------------------------------------------

// ytDLPFake helpers ----------------------------------------------------------

func TestYTDLPCommandIsShellFree(t *testing.T) {
	// The argv handed to exec.Command must be exactly the vetted strings:
	// no shell, no concatenation of user text.
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.TTL = time.Minute
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	// Force the fake to run even where no yt-dlp is installed (CI), and
	// record the exact argv handed to the would-be subprocess.
	var got []string
	m.opts.YTDLPPath = "yt-dlp-probe"
	// Pin DNS like testManager: the fetch must reach yt-dlp regardless of
	// the network's domain interception.
	m.lookupFn = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	m.retryDelay = func(int) time.Duration { return 0 }
	m.ytdlpFn = func(ctx context.Context, args []string) ([]byte, []byte, error) {
		got = args
		return nil, nil, exec.ErrNotFound
	}
	_, err = m.Download(context.Background(), DownloadRequest{URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid"})
	if err == nil {
		t.Fatal("expected an error from a missing tool")
	}
	joined := strings.Join(got, " ")
	if strings.Contains(joined, ";") || strings.Contains(joined, "|") || strings.Contains(joined, "&&") {
		t.Fatalf("shell metacharacters reached the argv: %q", joined)
	}
	// The URL must appear verbatim (one argv element), not interpolated.
	found := false
	for _, a := range got {
		if a == "https://www.youtube.com/watch?v=abcdefghijk" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the vetted URL is not a standalone argv element: %q", got)
	}
	// The output template must point INSIDE the managed dir.
	for _, a := range got {
		if strings.HasPrefix(a, dir) {
			if strings.Contains(a, "..") {
				t.Fatalf("output template escapes the managed dir: %q", a)
			}
			return
		}
	}
	t.Fatalf("no argv element pointed inside the managed dir %q: %q", dir, got)
}

// TestManagerRemoveFreesFileImmediately proves m.Remove removes the file, drops
// the entry from downloads.json, and clears the loopback route immediately.
func TestManagerRemoveFreesFileImmediately(t *testing.T) {
	testAllowLoopback(t)
	m, dir := testManager(t, time.Hour) // 1-hour TTL
	srv := serveMedia(t, 0, "video/mp4", smallMP4)

	res, err := m.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "immed"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("file should exist on disk: %v", err)
	}

	// Remove must delete the file immediately, not waiting for the 1-hour TTL.
	if err := m.Remove("immed"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Fatalf("file still exists after Remove: %v", err)
	}
	if _, ok := m.entries.Get("immed"); ok {
		t.Fatal("manifest still tracks entry after Remove")
	}

	// Verify manifest file on disk
	raw, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err == nil && strings.Contains(string(raw), `"immed"`) {
		t.Fatal("manifest file on disk still contains entry")
	}
}

// TestSweepOrphanFiles proves Sweep deletes untracked orphan files left in the download dir.
func TestSweepOrphanFiles(t *testing.T) {
	m, dir := testManager(t, time.Hour)

	orphan := filepath.Join(dir, "ses-s-orphan.mp4")
	if err := os.WriteFile(orphan, []byte("stale-media"), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	// Backdate the orphan file so it passes the grace cutoff
	old := time.Now().Add(-1 * time.Minute)
	_ = os.Chtimes(orphan, old, old)

	n := m.Sweep(context.Background())
	if n == 0 {
		t.Fatal("Sweep did not report any removed files")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan file was not deleted by Sweep: %v", err)
	}
}

// TestDiskCheckAllows1Point5GiBWith256MiBBuffer proves that when MinFreeBytes is
// 256 MiB, 1.5 GiB free space passes download disk checks even with MaxBytes = 2 GiB.
func TestDiskCheckAllows1Point5GiBWith256MiBBuffer(t *testing.T) {
	testAllowLoopback(t)
	m, _ := testManager(t, time.Hour)
	srv := serveMedia(t, 0, "video/mp4", smallMP4)

	// Simulate 1.5 GiB free space
	m.diskFree = func(path string) (int64, bool) {
		return int64(1.5 * 1024 * 1024 * 1024), true
	}
	m.opts.MaxBytes = 2 * 1024 * 1024 * 1024 // 2 GiB ceiling
	m.opts.MinFreeBytes = 256 * 1024 * 1024  // 256 MiB requirement

	res, err := m.Download(context.Background(), DownloadRequest{URL: srv.URL, Kind: "direct", ID: "clip-disk-ok"})
	if err != nil {
		t.Fatalf("Download failed with sufficient disk space: %v", err)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("downloaded file missing: %v", err)
	}
}

// TestManagerRemoveCleansUpGenerationsAndTempFiles proves Remove with a base ID
// removes generated files (e.g. id-g1.mp4, id.part, .tmp-id) and manifest entries.
func TestManagerRemoveCleansUpGenerationsAndTempFiles(t *testing.T) {
	testAllowLoopback(t)
	m, dir := testManager(t, time.Hour)

	// Create generation and part files matching base ID "ses-test1234"
	baseID := "ses-test1234"
	f1 := filepath.Join(dir, baseID+"-g1.mp4")
	f2 := filepath.Join(dir, baseID+".mp4.part")
	f3 := filepath.Join(dir, ".tmp-"+baseID)
	_ = os.WriteFile(f1, []byte("gen1"), 0o600)
	_ = os.WriteFile(f2, []byte("part"), 0o600)
	_ = os.WriteFile(f3, []byte("tmp"), 0o600)

	_ = m.entries.Add(Entry{ID: baseID + "-g1", Path: f1, FetchedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})

	if err := m.Remove(baseID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	for _, p := range []string{f1, f2, f3} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("file %s still exists after Remove(%s)", p, baseID)
		}
	}
	if _, ok := m.entries.Get(baseID + "-g1"); ok {
		t.Fatal("generation entry still exists in manifest")
	}
}
