package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// titleCapturePage is a stand-in for the media tab: a document whose native
// title is NOT the capture title (exactly like a YouTube watch page).
const titleCapturePage = `<!doctype html><html><head><meta charset="utf-8">
<title>Some Page - YouTube</title></head><body><h1>media</h1></body></html>`

// probePageDoc asks for a display capture and reports the outcome.
const probePageDoc = `<!doctype html><html><head><meta charset="utf-8">
<title>Meet stand-in</title></head><body>meet</body></html>`

// targetTitles returns the browser-side (WebContents) titles Chromium uses when
// it matches --auto-select-tab-capture-source-by-title.
func targetTitles(t *testing.T, b *rod.Browser) []string {
	t.Helper()
	infos, err := proto.TargetGetTargets{}.Call(b)
	if err != nil {
		t.Fatalf("get targets: %v", err)
	}
	var out []string
	for _, ti := range infos.TargetInfos {
		if ti.Type != proto.TargetTargetInfoTypePage {
			continue
		}
		out = append(out, ti.Title)
	}
	return out
}

// TestLockedTabTitleIsVisibleToChromium is the regression test for the live
// "could not start screen sharing in Google Meet (present flow timed out)"
// failure: Chromium answers getDisplayMedia by matching a tab whose *native*
// title equals --auto-select-tab-capture-source-by-title. The title lock used
// to override document.title with a no-op setter, so the page reported the
// locked title to JS while the browser-side title stayed the original one —
// no tab matched, Chromium's picker waited forever (headless has no UI), and
// Meet sat on "Presentation is starting..." until the share flow timed out.
func TestLockedTabTitleIsVisibleToChromium(t *testing.T) {
	if os.Getenv("TELEMEETBOT_BROWSER") == "" {
		t.Skip("set TELEMEETBOT_BROWSER=/path/to/chrome to run browser integration tests")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/media":
			_, _ = w.Write([]byte(titleCapturePage))
		default:
			_, _ = w.Write([]byte(probePageDoc))
		}
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()

	media, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	if err := media.Timeout(30 * time.Second).Navigate(srv.URL + "/media"); err != nil {
		t.Fatalf("navigate media: %v", err)
	}
	_ = media.WaitLoad()

	got, err := media.Eval(titleLockJS("TeleMeet Media"))
	if err != nil {
		t.Fatalf("title lock: %v", err)
	}
	t.Logf("document.title after lock = %q", got.Value.Str())

	deadline := time.Now().Add(5 * time.Second)
	var titles []string
	for time.Now().Before(deadline) {
		titles = targetTitles(t, b)
		if strings.Contains(strings.Join(titles, "|"), "TeleMeet Media") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("chromium tab titles = %q", titles)
	if !strings.Contains(strings.Join(titles, "|"), "TeleMeet Media") {
		t.Fatalf("no Chromium tab is titled %q (browser sees %q): the title lock only changed JS, so "+
			"--auto-select-tab-capture-source-by-title cannot match the media tab and getDisplayMedia "+
			"waits forever in headless", "TeleMeet Media", titles)
	}

	// The capture must actually be granted to that tab.
	probe, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("new probe page: %v", err)
	}
	defer func() { _ = probe.Close() }()
	if err := probe.Timeout(30 * time.Second).Navigate(srv.URL + "/probe"); err != nil {
		t.Fatalf("navigate probe: %v", err)
	}
	res, err := probe.Timeout(15 * time.Second).Eval(`() => new Promise(async (resolve) => {
  const t = setTimeout(() => resolve(JSON.stringify({error: "getDisplayMedia pending after 10s"})), 10000);
  try {
    const s = await navigator.mediaDevices.getDisplayMedia({video: true});
    clearTimeout(t);
    const v = s.getVideoTracks()[0];
    const out = {label: v ? v.label : "", state: v ? v.readyState : ""};
    s.getTracks().forEach(x => x.stop());
    resolve(JSON.stringify(out));
  } catch (e) {
    clearTimeout(t);
    resolve(JSON.stringify({error: String(e)}));
  }
})`)
	if err != nil {
		t.Fatalf("capture probe: %v", err)
	}
	var rep struct{ Label, State, Error string }
	if err := json.Unmarshal([]byte(res.Value.Str()), &rep); err != nil {
		t.Fatalf("decode probe %q: %v", res.Value.Str(), err)
	}
	t.Logf("capture = %+v", rep)
	if rep.Error != "" {
		t.Fatalf("getDisplayMedia failed: %s", rep.Error)
	}
	if !strings.HasPrefix(rep.Label, "web-contents-media-stream") {
		t.Fatalf("captured %q, want the media tab (web-contents-media-stream)", rep.Label)
	}
}
