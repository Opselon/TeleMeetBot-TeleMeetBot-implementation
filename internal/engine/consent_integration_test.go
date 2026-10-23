package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

// TestIntegrationConsentDismissalAllLocales drives the real detector against a
// YouTube-shaped wall in every locale the bot meets, plus the shadow-DOM and
// text-only shapes. It is the regression test for the original bug: the old
// matcher compared aria-label+text for exact equality with "reject all", which
// never matched a real full-sentence label.
func TestIntegrationConsentDismissalAllLocales(t *testing.T) {
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	fixtures := append([]consentFixture{}, consentLocaleFixtures...)
	fixtures = append(fixtures,
		consentFixture{Name: "en-shadow", Locale: "en", Reject: "Reject all", Accept: "Accept all", Aria: true, Shadow: true},
		consentFixture{Name: "en-no-accept", Locale: "en", Reject: "Reject all", Aria: true, Decoys: true},
	)

	for _, f := range fixtures {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			url, err := players.playerURL(consentFixtureDoc(f))
			if err != nil {
				t.Fatalf("serve fixture: %v", err)
			}
			p := openPage(t, b, url)
			defer func() { _ = p.Close() }()

			st, err := probeConsent(p)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if !st.Present {
				t.Fatalf("wall not detected: %s", st.describe())
			}
			if st.Best == nil {
				t.Fatalf("no dismissal candidate: %s", st.describe())
			}
			if st.Best.Action != "reject" {
				t.Fatalf("picked %q (%q), want reject: %s", st.Best.Action, st.Best.Label, st.describe())
			}

			res, err := dismissConsent(p, testLogger{t}, 15*time.Second)
			if err != nil {
				t.Fatalf("dismiss: %v", err)
			}
			if !res.Dismissed || res.Action != "reject" {
				t.Fatalf("dismiss result = %s", res.describe())
			}
			after, err := probeConsent(p)
			if err != nil {
				t.Fatalf("probe after dismiss: %v", err)
			}
			if after.Present {
				t.Fatalf("wall still present after %s: %s", res.describe(), after.describe())
			}
			// The decoys ("Customize", "More options", "Learn more") and the
			// accept button must never be what dismissed the wall.
			var clicks []string
			if raw := evaluate(t, p, "JSON.stringify(window.__clicks||[])"); raw != "" {
				_ = json.Unmarshal([]byte(raw), &clicks)
			}
			if len(clicks) != 1 || clicks[0] != "reject" {
				t.Fatalf("clicks = %v, want exactly [reject]", clicks)
			}
		})
	}
}

// TestIntegrationConsentAbsentIsNotAnError: a page with no wall must not be
// treated as one, and must not be clicked.
func TestIntegrationConsentAbsentIsNotAnError(t *testing.T) {
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	url, err := players.playerURL(consentFixtureDoc(consentFixture{Name: "none", Locale: "en"}))
	if err != nil {
		t.Fatalf("serve fixture: %v", err)
	}
	p := openPage(t, b, url)
	defer func() { _ = p.Close() }()

	st, err := probeConsent(p)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if st.Present {
		t.Fatalf("false positive: %s", st.describe())
	}
	res, err := dismissConsent(p, testLogger{t}, 5*time.Second)
	if err != nil {
		t.Fatalf("dismiss on a clean page: %v", err)
	}
	if res.Present || !res.Dismissed {
		t.Fatalf("result = %s, want present=false dismissed=true", res.describe())
	}
	if got := evaluate(t, p, "String((window.__clicks||[]).length)"); got != "0" {
		t.Fatalf("clicked %s controls on a clean page", got)
	}
}

// TestIntegrationConsentInertWallReportsDiagnostics: a wall whose buttons do
// nothing must fail with a message that says which surface, which buttons and
// what was tried — a timeout without that is not actionable.
func TestIntegrationConsentInertWallReportsDiagnostics(t *testing.T) {
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	f := consentFixture{Name: "inert", Locale: "en", Reject: "Reject all", Accept: "Accept all", Aria: true, Inert: true}
	url, err := players.playerURL(consentFixtureDoc(f))
	if err != nil {
		t.Fatalf("serve fixture: %v", err)
	}
	p := openPage(t, b, url)
	defer func() { _ = p.Close() }()

	start := time.Now()
	res, err := dismissConsent(p, testLogger{t}, 5*time.Second)
	if err == nil {
		t.Fatalf("inert wall reported a dismissal: %s", res.describe())
	}
	if time.Since(start) < 4*time.Second {
		t.Fatalf("gave up after %s, before the budget", time.Since(start))
	}
	msg := err.Error()
	for _, want := range []string{"did not clear", "ytd-consent-bump-v2-lightbox", "reject all", "tried="} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q is missing %q", msg, want)
		}
	}
	if res.Attempts == 0 {
		t.Error("attempts not recorded")
	}
}

// TestIntegrationPresentationSurvivesLateConsentWall is the end-to-end case the
// bug report describes: the media tab is prepared and captured by a Meet-shaped
// page (real getDisplayMedia), then YouTube mounts the consent wall late. The
// recovery must clear the wall and bring the video back WITHOUT asking for a new
// capture — the same track keeps delivering frames.
func TestIntegrationPresentationSurvivesLateConsentWall(t *testing.T) {
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	cfg := BrowserConfig{Executable: browserBin(t), Headless: true, WindowWidth: 1280, WindowHeight: 800}
	mediaURL, err := players.playerURL(mediaPresentationDoc())
	if err != nil {
		t.Fatalf("serve media fixture: %v", err)
	}
	media := openPage(t, b, mediaURL)
	defer func() { _ = media.Close() }()

	// Prepare the tab exactly like openMedia does.
	if fit, err := fitMediaTab(media, cfg, testLogger{t}); err != nil {
		t.Fatalf("fit: %v", err)
	} else {
		t.Logf("surface: %s", fit.describe())
	}
	if _, err := waitMediaReady(media, testLogger{t}, 25*time.Second); err != nil {
		t.Fatalf("media never got ready: %v", err)
	}
	rep, err := ensurePlayback(media, testLogger{t}, 1.0, 20*time.Second)
	if err != nil {
		t.Fatalf("playback: %v", err)
	}
	if rep.Advance < 1 || rep.Black {
		t.Fatalf("video not presentable before share: %s", rep.describe())
	}

	// The Meet side starts sharing the tab (real getDisplayMedia, auto-selected
	// by tab title — the same path the bot uses).
	meetURL, err := players.playerURL(captureMeetDoc())
	if err != nil {
		t.Fatalf("serve meet fixture: %v", err)
	}
	meet := openPage(t, b, meetURL)
	defer func() { _ = meet.Close() }()

	waitFor := func(t *testing.T, p *rod.Page, expr string, want string, d time.Duration) {
		t.Helper()
		deadline := time.Now().Add(d)
		last := ""
		for time.Now().Before(deadline) {
			last = evaluate(t, p, expr)
			if last == want {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("%s = %q, want %q", expr, last, want)
	}
	waitFor(t, meet, "String(window.__cap && window.__cap.calls)", "1", 30*time.Second)
	waitFor(t, meet, "String((window.__cap.samples||[]).length >= 3)", "true", 20*time.Second)

	before := captureSamples(t, meet)
	if len(before) < 3 {
		t.Fatalf("capture produced %d samples", len(before))
	}
	if meanOf(before[len(before)-1]) < 8 {
		t.Fatalf("captured frames are black before the wall: %v", before)
	}
	trackID := evaluate(t, meet, "String(window.__cap.id)")

	// YouTube mounts the wall late — after the share is already running.
	if got := evaluate(t, media, "String(window.__showConsent(['Reject all','Accept all']))"); got != "true" {
		t.Fatalf("could not mount the late wall: %s", got)
	}
	time.Sleep(700 * time.Millisecond)
	if st, err := probeConsent(media); err != nil {
		t.Fatalf("probe media: %v", err)
	} else if !st.Present {
		t.Fatalf("late wall not detected: %s", st.describe())
	}

	// Recovery must fix the tab while the share keeps running.
	s := &Session{
		ID:        "s-present",
		MediaURL:  mediaURL,
		state:     StateStreaming,
		log:       testLogger{t},
		mediaPage: media,
		meetPage:  meet,
		mgr:       &Manager{browserCfg: cfg, log: testLogger{t}},
	}
	if err := s.recoverMedia("consent wall over the share"); err != nil {
		t.Fatalf("recoverMedia: %v", err)
	}

	// 1) the wall is gone from the presented tab
	if st, err := probeConsent(media); err != nil {
		t.Fatalf("probe after recovery: %v", err)
	} else if st.Present {
		t.Fatalf("wall still on the presented tab: %s", st.describe())
	}
	// 2) the share never restarted: one capture, same track
	if calls := evaluate(t, meet, "String(window.__cap.calls)"); calls != "1" {
		t.Fatalf("getDisplayMedia called %s times, want 1 (recovery must not re-present)", calls)
	}
	if id := evaluate(t, meet, "String(window.__cap.id)"); id != trackID {
		t.Fatalf("capture track changed (%s → %s): the share was restarted", trackID, id)
	}
	// 3) frames keep arriving, live and non-black, with the video on screen
	time.Sleep(2 * time.Second)
	after := captureSamples(t, meet)
	if len(after) <= len(before) {
		t.Fatalf("capture stopped producing frames during recovery (%d → %d)", len(before), len(after))
	}
	tail := after[len(after)-3:]
	for _, s := range tail {
		if s.Live != "live" {
			t.Fatalf("capture track is %q after recovery", s.Live)
		}
		if s.Mean < 8 {
			t.Fatalf("captured frames are black after recovery: %+v", s)
		}
	}
	// 4) the surface is still fitted: video fills the capture surface
	if got := evaluate(t, media, "String(document.getElementById('v').getBoundingClientRect().width)"); got != "1280" {
		t.Fatalf("video width = %s, want 1280 (surface no longer fitted)", got)
	}
	if got := evaluate(t, media, "String(getComputedStyle(document.getElementById('masthead-container')).display)"); got != "none" {
		t.Fatalf("masthead is %q after recovery, want none", got)
	}
}

// captureSample is one frame reading from the Meet-shaped capture page.
type captureSample struct {
	Mean float64 `json:"mean"`
	Live string  `json:"live"`
	W    int     `json:"w"`
	H    int     `json:"h"`
}

func captureSamples(t *testing.T, p *rod.Page) []captureSample {
	t.Helper()
	raw := evaluate(t, p, "JSON.stringify((window.__cap && window.__cap.samples)||[])")
	var out []captureSample
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("decode capture samples: %v", err)
		}
	}
	return out
}

func meanOf(s captureSample) float64 { return s.Mean }
