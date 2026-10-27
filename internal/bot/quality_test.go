package bot

// Bot-level quality tests: the settings picker, the session quality menu,
// the info view's requested/selected/observed honesty, format detection and
// the per-item overrides.

import (
	"context"
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/media"
	"github.com/Opselon/TeleMeetBot/internal/store"
)

const probeJSON = `{"title":"Probe Video","duration":120,"formats":[
	{"format_id":"f137","height":1080,"width":1920,"fps":30,"vcodec":"avc1.640028","acodec":"none","ext":"mp4","tbr":4500,"filesize_approx":67000000},
	{"format_id":"f135","height":720,"width":1280,"fps":30,"vcodec":"avc1.64001f","acodec":"none","ext":"mp4","tbr":2500},
	{"format_id":"f18","height":360,"width":640,"vcodec":"avc1.42001E","acodec":"mp4a.40.2","ext":"mp4","tbr":600},
	{"format_id":"f140","height":0,"vcodec":"none","acodec":"mp4a.40.2","ext":"m4a","abr":129}
]}`

// harnessWithMediaManager wires a real media manager with a mocked yt-dlp so
// the format probe works without the binary.
func harnessWithMediaManager(t *testing.T, h *harness) *media.Manager {
	t.Helper()
	opts := media.DefaultOptions(h.cfg.Paths.DataDir + "/downloads")
	opts.Enabled = true
	opts.YTDLPPath = "yt-dlp-probe" // force the mock where no yt-dlp is installed
	m, err := media.NewManager(opts, &testLogger{h: h})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.SetYTDLPFnForTest(func(ctx context.Context, args []string) ([]byte, []byte, error) {
		for _, a := range args {
			if a == "-J" {
				return []byte(probeJSON), nil, nil
			}
		}
		return []byte("f137,1920,1080,30,avc1.640028,none,mp4,4500\n"), nil, nil
	})
	h.bot.mgr = m
	t.Cleanup(func() { m.Close() })
	return m
}

type testLogger struct{ h *harness }

func (l *testLogger) Debugf(string, ...interface{}) {}
func (l *testLogger) Infof(string, ...interface{})  {}
func (l *testLogger) Warnf(string, ...interface{})  {}
func (l *testLogger) Errorf(string, ...interface{}) {}

func TestQualitySettingsPicker(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.message(42, "/settings")
	h.callback(42, "set:quality")
	if got := h.api.lastSent().text; !strings.Contains(got, "Video quality policy") {
		t.Fatalf("picker not shown: %q", got)
	}

	// Pick Data Saver: persisted and re-rendered with the saved value.
	h.callback(42, "set:q:saver")
	if got := h.api.answers[len(h.api.answers)-1]; !strings.Contains(got, "Data Saver") {
		t.Fatalf("toast = %q", got)
	}
	if u := h.bot.userSettings(42); u.QualityPolicy != store.QualitySaver {
		t.Fatalf("saved policy = %q", u.QualityPolicy)
	}

	// Launch forwards the saved policy to the engine.
	runWizardToReview(t, h, 42)
	h.callback(42, cbLaunch)
	if len(h.eng.requests) != 1 || h.eng.requests[0].Quality != store.QualitySaver {
		t.Fatalf("launch request quality = %+v", h.eng.requests)
	}
}

func TestSessionQualityMenuAndPolicyChange(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	id := string(s.ID)

	// Open the menu: it states the requested policy and the direct-playback
	// limitation honestly.
	h.callback(42, "act:"+id+":quality")
	got := h.api.lastEdit().text
	if !strings.Contains(got, "Requested") {
		t.Fatalf("menu missing requested line: %q", got)
	}
	if !strings.Contains(got, "adaptive quality") {
		t.Fatalf("menu missing the YouTube limitation: %q", got)
	}

	// Switch the session policy: real engine change, menu re-rendered.
	h.callback(42, "act:"+id+":qset:best")
	if got := s.QualityPolicy(); got != store.QualityBest {
		t.Fatalf("session policy = %q, want best", got)
	}
	if got := h.api.lastEdit().text; !strings.Contains(got, "Best Available") {
		t.Fatalf("menu not refreshed with the new policy: %q", got)
	}
}

func TestQualityApplyReportsFailureHonestly(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	id := string(s.ID)

	// The fake session has no media tab, so the reload fails; the button
	// must say so instead of claiming success.
	h.callback(42, "act:"+id+":qapply")
	if got := h.api.lastEdit().text; !strings.Contains(got, "Apply failed") {
		t.Fatalf("apply failure not reported: %q", got)
	}
}

func TestQualityInfoViewSeparatesRequestedSelectedObserved(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	_ = s.SetQualityPolicy(store.QualityFHD1080)

	h.callback(42, "act:"+string(s.ID)+":qinfo")
	got := h.api.lastEdit().text
	for _, want := range []string{
		"Requested policy",
		"unavailable", // no extraction ladder in browser mode
		"unavailable", // player has not reported a resolution
		"separate measurements",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("info view missing %q: %q", want, got)
		}
	}
}

func TestFormatDetectionShowsLadder(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	harnessWithMediaManager(t, h)
	s := h.addSessionWithQueue(42, 2)
	s.MediaURL = "https://www.youtube.com/watch?v=abcdefghijk"

	h.callback(42, "act:"+string(s.ID)+":qformats")
	got := h.api.lastEdit().text
	for _, want := range []string{
		"Available formats",
		"Probe Video",
		"1080p", "720p", "360p",
		"~64 MB", // the filesize approximation of the top rendition
		"ffmpeg",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("format view missing %q: %q", want, got)
		}
	}
	// Every detected height became a cap button.
	kbData := allCallbackData(h.api.lastSent().kb)
	for _, want := range []string{"act:" + string(s.ID) + ":qfmt:1080", "act:" + string(s.ID) + ":qfmt:720", "act:" + string(s.ID) + ":qfmt:360"} {
		if !kbData[want] {
			t.Fatalf("missing cap button %q", want)
		}
	}

	// Choosing a height caps the policy — a real engine change.
	h.callback(42, "act:"+string(s.ID)+":qfmt:1080")
	if got := s.QualityPolicy(); got != store.QualityFHD1080 {
		t.Fatalf("policy after height pick = %q, want fhd1080", got)
	}
}

func TestFormatDetectionWithoutManager(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)

	// No media manager (feature disabled): the probe answers honestly and
	// the user lands back on the quality menu.
	h.callback(42, "act:"+string(s.ID)+":qformats")
	if got := h.api.answers[len(h.api.answers)-1]; !strings.Contains(got, "disabled") {
		t.Fatalf("probe answer = %q", got)
	}
	if got := h.api.lastEdit().text; !strings.Contains(got, "Video Quality") {
		t.Fatalf("menu not restored: %q", got)
	}
}

func TestPerItemQualityOverride(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 3)
	id := string(s.ID)

	h.callback(42, "q:itemq:"+id)
	if got := h.api.lastEdit().text; !strings.Contains(got, "Per-item quality") {
		t.Fatalf("item menu not shown: %q", got)
	}

	// Set item-2 to Data Saver through the queue callback.
	h.callback(42, "q:itemqset:"+id+":item-2:saver")
	if got := s.QueueItemQuality("item-2"); got != store.QualitySaver {
		t.Fatalf("item quality = %q", got)
	}

	// The item list marks the overridden item.
	h.callback(42, "q:itemq:"+id)
	if got := h.api.lastEdit().text; !strings.Contains(got, "🎞️") {
		t.Fatalf("override not marked in the item list: %q", got)
	}

	// A stale item id is answered instead of silently ignored.
	h.callback(42, "q:itemqset:"+id+":item-999:best")
	if got := h.api.answers[len(h.api.answers)-1]; !strings.Contains(got, "not found") {
		t.Fatalf("stale item answer = %q", got)
	}
}

func TestQualityPanelUnauthorized(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)

	h.callback(43, "act:"+string(s.ID)+":quality") // 43 not authorized
	if got := h.api.allSentText(); !strings.Contains(got, "Access denied") {
		t.Fatalf("unauthorized quality callback not denied: %q", got)
	}
}
