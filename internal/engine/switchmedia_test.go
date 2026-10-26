package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

// ---- harness ---------------------------------------------------------------

// switchRecorder is a test stand-in for the browser half of a switch. It logs
// every attempt, can stall attempts so a test can race a second switch in, and
// can fail specific URLs to exercise the rollback path.
type switchRecorder struct {
	mu       sync.Mutex
	attempts []string
	stall    chan struct{} // when non-nil, each attempt waits for a close
	failURLs map[string]error
	inFlight int
	maxSeen  int
}

func newSwitchRecorder() *switchRecorder {
	return &switchRecorder{failURLs: map[string]error{}}
}

// hold makes every attempt park until release is called.
func (r *switchRecorder) hold() {
	r.mu.Lock()
	r.stall = make(chan struct{})
	r.mu.Unlock()
}

// release lets parked attempts (and every attempt after them) run.
func (r *switchRecorder) release() {
	r.mu.Lock()
	ch := r.stall
	r.stall = nil
	r.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (r *switchRecorder) fail(url string, err error) {
	r.mu.Lock()
	r.failURLs[url] = err
	r.mu.Unlock()
}

func (r *switchRecorder) run(target MediaTarget, stale func() bool) error {
	r.mu.Lock()
	r.attempts = append(r.attempts, target.URL)
	r.inFlight++
	if r.inFlight > r.maxSeen {
		r.maxSeen = r.inFlight
	}
	stall := r.stall
	fail := r.failURLs[target.URL]
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}()

	// Simulate the async window of a real switch (navigation, consent, buffer,
	// playback verification) so a concurrent SwitchMedia can supersede it.
	if stall != nil {
		select {
		case <-stall:
		case <-time.After(5 * time.Second):
		}
	}
	// A real navigation can fail at the same moment a newer switch arrives, so
	// the failure is reported before the supersede is noticed.
	if fail != nil {
		return fail
	}
	if stale() {
		return errSwitchSuperseded
	}
	return nil
}

func (r *switchRecorder) urls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.attempts))
	copy(out, r.attempts)
	return out
}

// concurrent returns the highest number of attempts that were ever inside the
// recorder at the same time (1 means they were serialized).
func (r *switchRecorder) concurrent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxSeen
}

// newSwitchSession builds a streaming session wired for unit tests: the media
// tab is marked as open with a bare (never-evaluated) page value, the state and
// URL fields are real, and switchRun is a recorder so no browser is launched.
func newSwitchSession(t *testing.T, startURL string) (*Session, *switchRecorder) {
	t.Helper()
	rec := newSwitchRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Session{
		ID:        "s-switch",
		MediaURL:  startURL,
		MediaKind: MediaPage,
		TabTitle:  "TeleMeet Media",
		state:     StateStreaming,
		detail:    "[6/6] Streaming live",
		created:   time.Now(),
		ctx:       ctx,
		// A non-nil page marks the tab as open; these tests never evaluate on it.
		mediaPage: &rod.Page{},
		switchRun: rec.run,
	}
	return s, rec
}

// ---- validation -----------------------------------------------------------

func TestSwitchMediaValidation(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"ftp://example.com/x.mp4",
		"javascript:alert(1)",
		"not a url at all",
		"https://",
	} {
		s, _ := newSwitchSession(t, "https://example.com/start")
		if err := s.SwitchMedia(in); err == nil {
			t.Fatalf("SwitchMedia(%q) = nil, want error", in)
		}
		// A rejected URL must not touch the session at all.
		if got := s.Snapshot().MediaURL; got != "https://example.com/start" {
			t.Fatalf("SwitchMedia(%q) moved MediaURL to %q", in, got)
		}
		if got := s.Snapshot().State; got != StateStreaming {
			t.Fatalf("SwitchMedia(%q) moved state to %q", in, got)
		}
	}
}

func TestSwitchMediaRejectsSameURL(t *testing.T) {
	s, rec := newSwitchSession(t, "https://example.com/a")
	// A no-op switch is still a reload: the tab navigates to the same URL.
	if err := s.SwitchMedia("https://example.com/a"); err != nil {
		t.Fatalf("SwitchMedia(same URL) = %v", err)
	}
	if got := s.Snapshot().MediaURL; got != "https://example.com/a" {
		t.Fatalf("MediaURL = %q", got)
	}
	if len(rec.urls()) != 1 {
		t.Fatalf("expected one attempt, got %v", rec.urls())
	}
}

// ---- field updates --------------------------------------------------------

func TestSwitchMediaUpdatesFields(t *testing.T) {
	s, rec := newSwitchSession(t, "https://example.com/before")
	if err := s.SwitchMedia("https://youtu.be/dQw4w9WgXcQ"); err != nil {
		t.Fatalf("SwitchMedia: %v", err)
	}
	st := s.Snapshot()
	if st.MediaURL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ&autoplay=1" {
		t.Fatalf("MediaURL = %q, want the canonical YouTube watch URL", st.MediaURL)
	}
	if st.State != StateStreaming {
		t.Fatalf("State = %q, want streaming", st.State)
	}
	if !strings.Contains(st.Detail, "dQw4w9WgXcQ") {
		t.Fatalf("Detail = %q, want the new URL mentioned", st.Detail)
	}
	if got := rec.urls(); len(got) != 1 || got[0] != st.MediaURL {
		t.Fatalf("browser attempts = %v, want [%s]", got, st.MediaURL)
	}
}

func TestSwitchMediaUpdatesKindForDirectFile(t *testing.T) {
	s, _ := newSwitchSession(t, "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	if err := s.SwitchMedia("https://cdn.example.com/clip.mp4"); err != nil {
		t.Fatalf("SwitchMedia: %v", err)
	}
	if s.MediaKind != MediaDirect {
		t.Fatalf("MediaKind = %s, want direct", s.MediaKind)
	}
	if s.MediaURL != "https://cdn.example.com/clip.mp4" {
		t.Fatalf("MediaURL = %q", s.MediaURL)
	}
}

func TestSwitchMediaKeepsMeetAndTitle(t *testing.T) {
	s, _ := newSwitchSession(t, "https://example.com/before")
	s.MeetURL = "https://meet.google.com/abc-defg-hij"
	s.TabTitle = "TeleMeet Media"
	if err := s.SwitchMedia("https://example.com/after"); err != nil {
		t.Fatalf("SwitchMedia: %v", err)
	}
	if s.MeetURL != "https://meet.google.com/abc-defg-hij" {
		t.Fatalf("MeetURL changed: %q", s.MeetURL)
	}
	if s.TabTitle != "TeleMeet Media" {
		t.Fatalf("TabTitle changed: %q", s.TabTitle)
	}
}

// ---- rapid consecutive switches -------------------------------------------

// TestSwitchMediaRapidConsecutive is the race the generation counter exists
// for. Two switches arrive back to back: the first is already driving the tab
// when the second is requested. The stale attempt must unwind without rolling
// back over the winner, and the session must end on the newest URL, streaming.
func TestSwitchMediaRapidConsecutive(t *testing.T) {
	s, rec := newSwitchSession(t, "https://example.com/orig")
	rec.hold()

	loserDone := make(chan error, 1)
	go func() { loserDone <- s.SwitchMedia("https://example.com/loser") }()
	waitFor(t, func() bool { return len(rec.urls()) >= 1 }, "first switch attempt never started")

	// The winner is requested while the loser is parked mid-navigation. It
	// bumps the generation before queuing on switchMu, which is exactly what
	// makes the loser abort at its next checkpoint.
	winnerDone := make(chan error, 1)
	go func() { winnerDone <- s.SwitchMedia("https://example.com/winner") }()

	// The loser gets to finish its parked attempt; it must notice the newer
	// generation and bail out instead of completing a stale switch.
	rec.release()

	select {
	case err := <-winnerDone:
		if err != nil {
			t.Fatalf("winner SwitchMedia: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("winner switch never returned")
	}
	select {
	case err := <-loserDone:
		if err != nil {
			t.Fatalf("superseded switch reported %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("superseded switch never returned")
	}
	if got := s.Snapshot().MediaURL; got != "https://example.com/winner" {
		t.Fatalf("MediaURL = %q, want the winner", got)
	}
	if got := s.Snapshot().State; got != StateStreaming {
		t.Fatalf("State = %q, want streaming", got)
	}
	// Both attempts reached the browser half, but only one ever ran at a time.
	if urls := rec.urls(); len(urls) != 2 {
		t.Fatalf("browser attempts = %v, want both", urls)
	}
	if n := rec.concurrent(); n != 1 {
		t.Fatalf("concurrent attempts = %d, want 1 (switchMu must serialize)", n)
	}
}

// TestSwitchMediaGenerationGuardsRollback proves a superseded switch leaves the
// rollback path alone: the loser's navigation failed, but because a newer
// switch owns the tab, no restore navigation is issued and the winner's URL
// stands.
func TestSwitchMediaGenerationGuardsRollback(t *testing.T) {
	s, rec := newSwitchSession(t, "https://example.com/orig")
	rec.fail("https://example.com/loser", errors.New("navigation failed"))
	rec.hold()

	loserDone := make(chan error, 1)
	go func() { loserDone <- s.SwitchMedia("https://example.com/loser") }()
	waitFor(t, func() bool { return len(rec.urls()) >= 1 }, "loser never started")

	winnerDone := make(chan error, 1)
	go func() { winnerDone <- s.SwitchMedia("https://example.com/winner") }()
	// The loser must see the winner's generation when it resumes, otherwise it
	// cannot tell it was superseded: wait for the bump before releasing it.
	waitFor(t, func() bool {
		g := s.switchGen.Load()
		return g >= 2
	}, "winner never claimed a generation")
	rec.release()

	select {
	case <-winnerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("winner never returned")
	}
	select {
	case <-loserDone:
	case <-time.After(10 * time.Second):
		t.Fatal("loser never returned")
	}

	// The winner's URL stands and no third (rollback) navigation happened.
	if got := s.Snapshot().MediaURL; got != "https://example.com/winner" {
		t.Fatalf("MediaURL = %q, want the winner", got)
	}
	if urls := rec.urls(); len(urls) != 2 {
		t.Fatalf("browser attempts = %v, want exactly the two attempts (no rollback)", urls)
	}
}

func TestSwitchMediaSerializesAttempts(t *testing.T) {
	s, rec := newSwitchSession(t, "https://example.com/orig")
	rec.hold()
	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.SwitchMedia("https://example.com/n" + string(rune('0'+i)))
		}(i)
	}
	waitFor(t, func() bool { return len(rec.urls()) >= 1 }, "no attempt started")
	time.Sleep(30 * time.Millisecond) // let the rest queue on switchMu
	rec.release()
	wg.Wait()

	// The tab was never driven by two attempts at once, and the session ends on
	// whichever URL was requested last (it is the only one that can be current).
	if got := rec.concurrent(); got != 1 {
		t.Fatalf("concurrent attempts = %d, want 1", got)
	}
	if got := s.Snapshot().State; got != StateStreaming {
		t.Fatalf("State = %q, want streaming", got)
	}
	if got := s.Snapshot().MediaURL; !strings.HasPrefix(got, "https://example.com/n") {
		t.Fatalf("MediaURL = %q, want one of the requested URLs", got)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// ---- real-Chromium integration --------------------------------------------

// switchFixturePage is a player-shaped page whose media is a real <video>
// element fed by a canvas captureStream: it plays, it paints real frames, and
// its title, hue and declared duration all encode which URL was loaded.
func switchFixturePage(title string, hue int, dur float64) string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>` + title + `</title>
<style>html,body{margin:0;background:#000;overflow:hidden}
#movie_player video{width:1280px;height:720px;display:block;background:#000}</style></head>
<body><div id="movie_player"><video id="v" muted playsinline></video></div>
<script>
const c = document.createElement('canvas');
c.width = 640; c.height = 360;
const ctx = c.getContext('2d');
let frame = ` + strconv.Itoa(hue) + `;
function draw() {
  frame++;
  ctx.fillStyle = 'hsl(' + (frame % 360) + ',80%,45%)';
  ctx.fillRect(0, 0, c.width, c.height);
  ctx.fillStyle = '#fff';
  ctx.fillRect((frame * 7) % c.width, 140, 80, 80);
  requestAnimationFrame(draw);
}
draw();
const v = document.getElementById('v');
v.srcObject = c.captureStream(30);
try { Object.defineProperty(v, 'duration', { value: ` + strconv.FormatFloat(dur, 'f', 1, 64) + `, configurable: true }); } catch (e) {}
v.play().catch(() => {});
</script></body></html>`
}

// TestIntegrationSwitchMediaSwitchesTheSameTab is the browser proof of the
// feature: the media tab the session already owns is navigated to a second,
// different URL by SwitchMedia, and the same tab — the surface Meet captures
// with getDisplayMedia — ends up showing and playing the new media. Two
// distinct tones make "switched" distinguishable from "reloaded": the title,
// the media duration and the audible track all change.
func TestIntegrationSwitchMediaSwitchesTheSameTab(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: real Chromium required (TELEMEETBOT_BROWSER)")
	}
	// The fixture is a real <video> fed by a canvas captureStream, because
	// prepareMedia/ensurePlayback pick the best <video> and verify painted
	// frames: an audio-only page has no video element at all (videos=0) and no
	// surface to render, so it can never prove a switch. The two namespaces
	// differ in title, hue and declared duration, so "switched" is provable
	// from the rendered page rather than from the session fields.
	pageA := switchFixturePage("Test Player A", 20, 1.5)
	pageB := switchFixturePage("Test Player B", 200, 3.0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page-a":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(pageA))
		case "/page-b":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(pageB))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := NewManager(
		BrowserConfig{Executable: browserBin(t), Headless: true, WindowWidth: 1280, WindowHeight: 800},
		MediaOptions{},
		MeetOptions{PresentMode: "tab", CaptureSource: "TeleMeet Media"},
		testLogger{t},
	)
	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, srv.URL+"/page-a")

	before, err := probeStatus(page)
	if err != nil {
		t.Fatalf("status A: %v", err)
	}
	if before.Count == 0 || before.Duration < 0.9 {
		t.Fatalf("url A not loaded: %+v", before)
	}

	s := &Session{
		ID:        "s-switch-int",
		MeetURL:   srv.URL + "/meet-unused",
		MediaURL:  srv.URL + "/page-a",
		MediaKind: MediaPage,
		TabTitle:  "TeleMeet Media",
		state:     StateStreaming,
		ctx:       context.Background(),
		browser:   b,
		mgr:       m,
		mediaPage: page,
		log:       testLogger{t},
	}

	if err := s.SwitchMedia(srv.URL + "/page-b"); err != nil {
		t.Fatalf("SwitchMedia: %v", err)
	}

	after, err := probeStatus(page)
	if err != nil {
		t.Fatalf("status B: %v", err)
	}
	if after.Count == 0 {
		t.Fatalf("no media element after the switch: %+v", after)
	}
	if after.Duration < 2.5 {
		t.Fatalf("duration = %.2fs, want the new clip's ~3s: the old media is still loaded", after.Duration)
	}
	if after.Paused {
		t.Fatalf("switched media is paused: %+v", after)
	}
	// Same tab object: the share has nothing new to attach to.
	if info, err := page.Info(); err != nil || !strings.HasSuffix(info.URL, "/page-b") {
		t.Fatalf("tab url = %v (%v), want the switched URL", info, err)
	}
	if got := s.Snapshot().MediaURL; got != srv.URL+"/page-b" {
		t.Fatalf("MediaURL = %q, want the switched URL", got)
	}
	if got := s.Snapshot().State; got != StateStreaming {
		t.Fatalf("State = %q, want streaming after the switch", got)
	}
	t.Logf("switch ok: same tab now at %s playing %.1fs of the new %.1fs clip", srv.URL+"/page-b", after.CurrentTime, after.Duration)
}

// ---- rollback after failure -----------------------------------------------

func TestSwitchMediaRollsBackOnFailure(t *testing.T) {
	s, rec := newSwitchSession(t, "https://example.com/good")
	rec.fail("https://example.com/bad", errors.New("navigation failed"))

	if err := s.SwitchMedia("https://example.com/bad"); err == nil {
		t.Fatal("SwitchMedia(bad) = nil, want error")
	}
	st := s.Snapshot()
	if st.MediaURL != "https://example.com/good" {
		t.Fatalf("MediaURL = %q, want the original after rollback", st.MediaURL)
	}
	if st.State != StateStreaming {
		t.Fatalf("State = %q, want streaming restored", st.State)
	}
	if !strings.Contains(st.Detail, "restored") {
		t.Fatalf("Detail = %q, want a restore notice", st.Detail)
	}
	// The attempt ran, then the rollback re-ran with the old URL.
	urls := rec.urls()
	if len(urls) != 2 || urls[0] != "https://example.com/bad" || urls[1] != "https://example.com/good" {
		t.Fatalf("attempts = %v, want [bad good]", urls)
	}
}

func TestSwitchMediaFailureWithoutRollbackTarget(t *testing.T) {
	// No media tab: the switch must fail and leave the session's URL alone
	// instead of pointing it at a URL nothing is playing.
	s, rec := newSwitchSession(t, "https://example.com/good")
	s.mediaPage = nil
	if err := s.SwitchMedia("https://example.com/next"); err == nil {
		t.Fatal("SwitchMedia with no tab = nil, want error")
	}
	if got := s.Snapshot().MediaURL; got != "https://example.com/good" {
		t.Fatalf("MediaURL = %q, want unchanged", got)
	}
	if len(rec.urls()) != 0 {
		t.Fatalf("browser was touched: %v", rec.urls())
	}
}

func TestSwitchMediaPausedSessionStaysPaused(t *testing.T) {
	s, _ := newSwitchSession(t, "https://example.com/before")
	s.state = StatePaused
	if err := s.SwitchMedia("https://example.com/after"); err != nil {
		t.Fatalf("SwitchMedia: %v", err)
	}
	if got := s.Snapshot().State; got != StatePaused {
		t.Fatalf("State = %q, want paused", got)
	}
	if got := s.Snapshot().MediaURL; got != "https://example.com/after" {
		t.Fatalf("MediaURL = %q, want after", got)
	}
}

// ---- classification of switch targets --------------------------------------

func TestSwitchMediaClassifyMedia(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		kind  MediaKind
		match string
	}{
		{"youtube watch", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", MediaYouTube, "watch?v=dQw4w9WgXcQ"},
		{"youtube short link", "https://youtu.be/dQw4w9WgXcQ", MediaYouTube, "watch?v=dQw4w9WgXcQ"},
		{"youtube shorts", "https://youtube.com/shorts/dQw4w9WgXcQ", MediaYouTube, "watch?v=dQw4w9WgXcQ"},
		{"mp4 direct", "https://cdn.example.com/clip.mp4", MediaDirect, "clip.mp4"},
		{"web page", "https://example.com/slides", MediaPage, "slides"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyMedia(tc.in)
			if err != nil {
				t.Fatalf("ClassifyMedia(%q): %v", tc.in, err)
			}
			if got.Kind != tc.kind {
				t.Fatalf("kind = %s, want %s", got.Kind, tc.kind)
			}
			if !strings.Contains(got.URL, tc.match) {
				t.Fatalf("url = %q, want it to contain %q", got.URL, tc.match)
			}
		})
	}
}
