package engine

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

// YouTube skippable-ad fixtures.
//
// These documents reproduce the DOM shapes YouTube's player renders for a
// skippable ad: a button carrying the structural skip classes whose label is a
// short sentence, that is inert while the ad counts down and becomes enabled
// when it is skippable. The countdown state is what makes naive matchers click
// early (the click is a no-op) and the inert state is what makes them click
// forever.

// adFixture describes one skippable-ad variant.
type adFixture struct {
	Name        string // test name
	Locale      string // label language, for diagnostics
	Label       string // the enabled skip label ("Skip ads"); empty = class-only
	Countdown   string // the pending label ("Skip ads in 5"); empty = none
	Aria        string // full-sentence aria-label (the real YouTube shape); empty = none
	Shadow      bool   // mount the player chrome inside a shadow root
	ClassOnly   bool   // no accessible label at all; rely on ytp-ad-skip-button
	NonSkip     bool   // ad surface with NO skip control (bumper / non-skippable)
	SecondBtn   bool   // a second, non-skip control inside the player
	NotInPlayer bool   // the skip-shaped button lives OUTSIDE #movie_player
}

// adFixtureDoc renders a YouTube-shaped watch page carrying a skippable ad. The
// surface records what was clicked so tests can prove exactly which control
// dismissed the ad.
func adFixtureDoc(f adFixture) string {
	label := f.Label
	if label == "" && !f.ClassOnly && !f.NonSkip {
		label = "Skip ads"
	}
	btn := ""
	if !f.NonSkip {
		cls := "ytp-ad-skip-button"
		// The class-only shape has no DOM-visible label at all; the aria-label
		// is the only hint, and even it may be absent.
		text := label
		ariaAttr := ""
		if f.Aria != "" {
			ariaAttr = ` aria-label="` + f.Aria + `"`
		}
		btn = `<button id="skip" class="` + cls + `"` + ariaAttr + ` style="position:absolute;right:12px;bottom:42px;padding:10px 16px;background:#111;color:#fff">` + text + `</button>`
	}
	second := ""
	if f.SecondBtn {
		second = `<button id="visit" class="ytp-button" style="position:absolute;left:12px;bottom:42px;padding:10px 16px">Visit advertiser</button>`
	}
	player := `<div id="movie_player" class="html5-video-player" style="position:relative;width:900px;height:506px;background:#000">
  <video id="v" muted playsinline style="width:900px;height:506px"></video>
  <div class="ytp-ad-player-overlay" style="position:absolute;inset:0;background:rgba(0,0,0,.25)"></div>` + btn + second + `</div>`
	outOfPlayer := ""
	if f.NotInPlayer {
		player = `<div id="movie_player" class="html5-video-player" style="position:relative;width:900px;height:506px;background:#000">
  <video id="v" muted playsinline style="width:900px;height:506px"></video>
  <div class="ytp-ad-player-overlay" style="position:absolute;inset:0;background:rgba(0,0,0,.25)"></div></div>`
		// A skip-shaped control that is NOT inside the player: a different
		// surface (a card, an endscreen) and must never be clicked.
		outOfPlayer = `<button id="skip" class="ytp-ad-skip-button" aria-label="Skip ads" style="position:fixed;right:12px;top:12px;padding:10px 16px">Skip ads</button>`
	}

	script := `
window.__adskips = [];
window.__enabled = false;
const mark = (kind) => { window.__adskips.push(kind); };
const btn = document.getElementById('skip');
if (btn) {
  btn.addEventListener('click', () => mark('skip'));
  ` + enableScript(f) + `
}
const visit = document.getElementById('visit');
if (visit) visit.addEventListener('click', () => mark('visit'));
`
	shadow := ""
	if f.Shadow {
		shadow = `
const host = document.createElement('div');
host.id = 'ad-host';
host.style.cssText = 'position:absolute;inset:0';
document.getElementById('movie_player').appendChild(host);
const root = host.attachShadow({mode: 'open'});
const tpl = document.createElement('div');
tpl.innerHTML = ` + "`" + `<button id="skip" class="ytp-ad-skip-button" aria-label="Skip ads in 5 seconds" style="padding:10px 16px">Skip ads</button>` + "`" + `;
root.appendChild(tpl);
root.getElementById('skip').addEventListener('click', () => mark('skip'));
root.getElementById('skip').disabled = true;
window.__shadowBtn = root.getElementById('skip');
`
	}

	return `<!doctype html><html><head><meta charset="utf-8"><title>Ad test - YouTube</title>
<style>
 html,body{margin:0;background:#0f0f0f;color:#fff;font-family:sans-serif}
 #masthead-container{height:56px;background:#202020}
 #secondary{float:right;width:340px;height:600px;background:#181818}
 #primary{width:900px}
</style></head>
<body>
<div id="masthead-container">masthead</div>
<div id="secondary">related videos</div>
<div id="primary"><div id="player-container">` + player + `</div></div>` + outOfPlayer + `
<script>
` + script + shadow + `
</script>
</body></html>`
}

// enableScript produces the availability transition for a fixture. The default
// shape (no countdown) starts enabled; a countdown fixture arms it later so the
// test can prove the detector waits instead of clicking early.
func enableScript(f adFixture) string {
	if f.NonSkip {
		return ""
	}
	if f.Countdown != "" {
		return `
btn.disabled = true;
btn.textContent = ` + "`" + f.Countdown + "`" + `;
setTimeout(() => {
  btn.disabled = false;
  btn.classList.add('ytp-ad-skip-button-enabled');
  btn.textContent = ` + "`" + (f.Label) + "`" + `;
}, 1200);`
	}
	return `btn.classList.add('ytp-ad-skip-button-enabled');`
}

// TestIntegrationAdSkipClicksEnabledButton drives the real detector against a
// YouTube-shaped ad whose skip button is already available: the button must be
// pressed exactly once, and no other control may be touched.
func TestIntegrationAdSkipClicksEnabledButton(t *testing.T) {
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	for _, f := range []adFixture{
		{Name: "en-ready", Locale: "en", Label: "Skip ads", Aria: "Skip ads"},
		{Name: "en-aria-sentence", Locale: "en", Label: "Skip ads", Aria: "Skip ads in 5 seconds"},
		{Name: "de", Locale: "de", Label: "Werbung überspringen", Aria: "Werbung überspringen"},
		{Name: "fa", Locale: "fa", Label: "رد کردن تبلیغات", Aria: "رد کردن تبلیغات"},
		{Name: "class-only", ClassOnly: true},
		{Name: "shadow", Shadow: true},
	} {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			url, err := players.playerURL(adFixtureDoc(f))
			if err != nil {
				t.Fatalf("serve fixture: %v", err)
			}
			p := openPage(t, b, url)
			defer func() { _ = p.Close() }()
			// Shadow fixtures arm after load; give the page a beat.
			time.Sleep(300 * time.Millisecond)

			st, err := probeAdSkip(p)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if !st.YouTube {
				t.Fatalf("not recognised as a YouTube player: %s", st.describe())
			}
			if !st.Ad {
				t.Fatalf("ad not detected: %s", st.describe())
			}
			rep, err := scanSkipAds(p, testLogger{t})
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if !rep.Skipped {
				t.Fatalf("skip did not fire: %s", rep.describe())
			}
			clicks := readAdSkips(t, p)
			if len(clicks) != 1 || clicks[0] != "skip" {
				t.Fatalf("clicks = %v, want exactly [skip]", clicks)
			}
		})
	}
}

// TestIntegrationAdSkipWaitsForCountdown proves the detector does NOT click a
// skip button that is present but not yet enabled: the click must be deferred
// until YouTube arms the control, then land exactly once.
func TestIntegrationAdSkipWaitsForCountdown(t *testing.T) {
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	f := adFixture{Name: "countdown", Locale: "en", Label: "Skip ads", Countdown: "Skip ads in 5", Aria: "Skip ads in 5 seconds"}
	url, err := players.playerURL(adFixtureDoc(f))
	if err != nil {
		t.Fatalf("serve fixture: %v", err)
	}
	p := openPage(t, b, url)
	defer func() { _ = p.Close() }()
	time.Sleep(250 * time.Millisecond)

	// While the countdown is live the scan must see the ad and NOT click.
	st, err := probeAdSkip(p)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !st.Ad {
		t.Fatalf("ad not detected during countdown: %s", st.describe())
	}
	if st.Enabled {
		t.Fatalf("button reported enabled during the countdown: %s", st.describe())
	}
	rep, err := scanSkipAds(p, testLogger{t})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rep.Skipped {
		t.Fatalf("clicked during the countdown: %s", rep.describe())
	}
	if got := readAdSkips(t, p); len(got) != 0 {
		t.Fatalf("clicked %v while the button was not yet available", got)
	}

	// Once YouTube arms it the skip must land within one scan.
	if _, err := waitForCondition(p, func() bool {
		s, err := probeAdSkip(p)
		return err == nil && s.Enabled
	}, 8*time.Second); err != nil {
		t.Fatalf("button never became enabled: %v", err)
	}
	rep, err = scanSkipAds(p, testLogger{t})
	if err != nil {
		t.Fatalf("scan after arm: %v", err)
	}
	if !rep.Skipped {
		t.Fatalf("skip did not fire after the button was armed: %s", rep.describe())
	}
	if got := readAdSkips(t, p); len(got) != 1 || got[0] != "skip" {
		t.Fatalf("clicks = %v, want exactly [skip]", got)
	}
}

// TestIntegrationAdSkipIgnoresNonSkippableAd: an ad surface with no skip
// control must be detected but never touched (no injection, no blocking, no
// playback interference). "visit advertiser" is not a skip and must survive.
func TestIntegrationAdSkipIgnoresNonSkippableAd(t *testing.T) {
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	f := adFixture{Name: "non-skippable", Locale: "en", NonSkip: true, SecondBtn: true}
	url, err := players.playerURL(adFixtureDoc(f))
	if err != nil {
		t.Fatalf("serve fixture: %v", err)
	}
	p := openPage(t, b, url)
	defer func() { _ = p.Close() }()
	time.Sleep(300 * time.Millisecond)

	st, err := probeAdSkip(p)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !st.YouTube {
		t.Fatalf("not recognised as YouTube: %s", st.describe())
	}
	rep, err := scanSkipAds(p, testLogger{t})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rep.Skipped {
		t.Fatalf("clicked something on a non-skippable ad: %s", rep.describe())
	}
	if got := readAdSkips(t, p); len(got) != 0 {
		t.Fatalf("clicked %v on a non-skippable ad", got)
	}
	// The video element must be untouched: no pause, no seek, no blocking.
	if paused := evaluate(t, p, "String(document.getElementById('v').paused)"); paused != "true" {
		t.Fatalf("video paused state changed: paused=%s", paused)
	}
	if t_ := evaluate(t, p, "String(document.getElementById('v').currentTime)"); t_ != "0" {
		t.Fatalf("video currentTime moved: %s", t_)
	}
}

// TestIntegrationAdSkipIgnoresOutOfPlayerButton: a skip-shaped button that is
// not inside the YouTube player must never be clicked.
func TestIntegrationAdSkipIgnoresOutOfPlayerButton(t *testing.T) {
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	f := adFixture{Name: "out-of-player", Locale: "en", Label: "Skip ads", Aria: "Skip ads", NotInPlayer: true}
	url, err := players.playerURL(adFixtureDoc(f))
	if err != nil {
		t.Fatalf("serve fixture: %v", err)
	}
	p := openPage(t, b, url)
	defer func() { _ = p.Close() }()
	time.Sleep(300 * time.Millisecond)

	rep, err := scanSkipAds(p, testLogger{t})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rep.Skipped {
		t.Fatalf("clicked an out-of-player control: %s", rep.describe())
	}
	if got := readAdSkips(t, p); len(got) != 0 {
		t.Fatalf("clicked %v (must stay untouched)", got)
	}
}

// TestIntegrationAdSkipNonYouTubeTabIsUntouched: a plain media page (no
// YouTube player markers) must scan as out of scope, and its playback controls
// must be untouched. This is the "non-YouTube media must remain unaffected"
// bar.
func TestIntegrationAdSkipNonYouTubeTabIsUntouched(t *testing.T) {
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	// The loopback player document has a <video> and no #movie_player.
	doc, err := PlayerHTML(MediaTarget{URL: "about:blank", Kind: MediaDirect}, PlayerOptions{Src: "about:blank", TabTitle: "TeleMeet Media"})
	if err != nil {
		t.Fatalf("build player: %v", err)
	}
	url, err := players.playerURL(doc)
	if err != nil {
		t.Fatalf("serve player: %v", err)
	}
	p := openPage(t, b, url)
	defer func() { _ = p.Close() }()
	time.Sleep(300 * time.Millisecond)

	st, err := probeAdSkip(p)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if st.YouTube {
		t.Fatalf("plain player recognised as YouTube: %s", st.describe())
	}
	rep, err := scanSkipAds(p, testLogger{t})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rep.YouTube || rep.AdPlaying || rep.Skipped {
		t.Fatalf("non-YouTube tab was touched: %s", rep.describe())
	}
}

// TestIntegrationAdSkipMonitorMidRoll drives the session monitor: an ad that
// appears mid-stream must be skipped by the background loop, without the caller
// doing anything.
func TestIntegrationAdSkipMonitorMidRoll(t *testing.T) {
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	// A watch page that plays, then mounts an enabled skip button 1.5s in.
	url, err := players.playerURL(adMidRollDoc())
	if err != nil {
		t.Fatalf("serve fixture: %v", err)
	}
	p := openPage(t, b, url)
	defer func() { _ = p.Close() }()

	cfg := BrowserConfig{Executable: browserBin(t), Headless: true, WindowWidth: 1280, WindowHeight: 800}
	s := &Session{
		ID:        "s-adskip",
		MediaURL:  url,
		MediaKind: MediaYouTube,
		state:     StateStreaming,
		log:       testLogger{t},
		mediaPage: p,
		mgr:       &Manager{browserCfg: cfg, log: testLogger{t}},
	}
	stop := make(chan struct{})
	go s.MonitorSkipAds(stop)
	defer func() {
		close(stop)
		time.Sleep(300 * time.Millisecond)
	}()

	// The button mounts at 1.5s; the monitor polls every second, so the skip
	// must land well inside this window.
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if got := readAdSkips(t, p); len(got) >= 1 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	clicks := readAdSkips(t, p)
	if len(clicks) != 1 || clicks[0] != "skip" {
		t.Fatalf("clicks = %v, want exactly [skip]", clicks)
	}
}

// adMidRollDoc renders a watch page that mounts a skip button after playback
// begins, the mid-roll shape.
func adMidRollDoc() string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>Mid roll - YouTube</title>
<style>
 html,body{margin:0;background:#0f0f0f;color:#fff;font-family:sans-serif}
 #movie_player{position:relative;width:900px;height:506px;background:#000}
 #movie_player video{width:900px;height:506px}
</style></head>
<body>
<div id="movie_player" class="html5-video-player">
  <video id="v" muted playsinline></video>
  <div class="ytp-ad-player-overlay" style="position:absolute;inset:0;background:rgba(0,0,0,.25)"></div>
</div>
<script>
window.__adskips = [];
// The mid-roll: the control appears after the page settles, exactly like a
// mid-roll ad slot a real stream inserts.
setTimeout(() => {
  const btn = document.createElement('button');
  btn.id = 'skip';
  btn.className = 'ytp-ad-skip-button ytp-ad-skip-button-enabled';
  btn.setAttribute('aria-label', 'Skip ads');
  btn.textContent = 'Skip ads';
  btn.style.cssText = 'position:absolute;right:12px;bottom:42px;padding:10px 16px';
  document.getElementById('movie_player').appendChild(btn);
  btn.addEventListener('click', () => { window.__adskips.push('skip'); btn.remove(); });
}, 1500);
</script>
</body></html>`
}

// readAdSkips returns what the fixture recorded as clicked.
func readAdSkips(t *testing.T, p *rod.Page) []string {
	t.Helper()
	raw := evaluate(t, p, "JSON.stringify(window.__adskips||[])")
	var out []string
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("decode adskips: %v", err)
		}
	}
	return out
}

// waitForCondition polls until cond is true or d elapses.
func waitForCondition(p *rod.Page, cond func() bool, d time.Duration) (bool, error) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false, errTimeout(d)
}

type errTimeout time.Duration

func (e errTimeout) Error() string { return "timed out after " + time.Duration(e).String() }

// unused: keep the imports honest for future fixtures
var (
	_ = strings.TrimSpace
	_ = sync.Mutex{}
	_ = httptest.NewServer
)
