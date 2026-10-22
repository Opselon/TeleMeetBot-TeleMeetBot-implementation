package engine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// testLogger routes engine logs into the test log.
type testLogger struct{ t *testing.T }

func (l testLogger) Debugf(f string, a ...interface{}) { l.t.Logf("[dbg] "+f, a...) }
func (l testLogger) Infof(f string, a ...interface{})  { l.t.Logf("      "+f, a...) }
func (l testLogger) Warnf(f string, a ...interface{})  { l.t.Logf("[warn] "+f, a...) }
func (l testLogger) Errorf(f string, a ...interface{}) { l.t.Logf("[err]  "+f, a...) }

// browserBin returns the Chromium path for integration tests, skipping when unset.
func browserBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("TELEMEETBOT_BROWSER")
	if bin == "" {
		t.Skip("set TELEMEETBOT_BROWSER=/path/to/chrome to run browser integration tests")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("browser path %s is not accessible: %v", bin, err)
	}
	return bin
}

// newTestBrowser launches a bare browser for integration tests.
func newTestBrowser(t *testing.T) (*rod.Browser, func()) {
	t.Helper()
	bin := browserBin(t)
	m := NewManager(
		BrowserConfig{Executable: bin, Headless: true, WindowWidth: 1280, WindowHeight: 800},
		MediaOptions{},
		MeetOptions{PresentMode: "tab", CaptureSource: "TeleMeet Media"},
		testLogger{t},
	)
	b, closeFn, err := launchBrowser(launchOptions{browser: m.browserCfg, meet: m.meetCfg}, testLogger{t})
	if err != nil {
		t.Fatalf("launch browser: %v", err)
	}
	return b, closeFn
}

// openPage creates a page and navigates it.
func openPage(t *testing.T, b *rod.Browser, url string) *rod.Page {
	t.Helper()
	p, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	if err := p.Timeout(30 * time.Second).Navigate(url); err != nil {
		t.Fatalf("navigate %s: %v", url, err)
	}
	_ = p.WaitLoad()
	return p
}

// evaluate returns the string value of a JS expression.
func evaluate(t *testing.T, p *rod.Page, js string) string {
	t.Helper()
	// rod requires function-form JS; wrap raw expressions.
	res, err := p.Timeout(10 * time.Second).Eval("() => (" + js + ")")
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	return res.Value.Str()
}

func probeStatus(p *rod.Page) (mediaStatus, error) {
	res, err := p.Timeout(10 * time.Second).Eval(mediaControlJS("status", 0))
	if err != nil {
		return mediaStatus{}, err
	}
	var st mediaStatus
	err = json.Unmarshal([]byte(res.Value.Str()), &st)
	return st, err
}

func probeAction(p *rod.Page, action string, arg float64) (mediaStatus, error) {
	res, err := p.Timeout(10 * time.Second).Eval(mediaControlJS(action, arg))
	if err != nil {
		return mediaStatus{}, err
	}
	var st mediaStatus
	err = json.Unmarshal([]byte(res.Value.Str()), &st)
	return st, err
}

// generateWAV makes a mono 16-bit PCM WAV tone.
func generateWAV(seconds, sampleRate int, freq, volume float64) []byte {
	n := seconds * sampleRate
	data := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		v := math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate)) * volume * math.MaxInt16
		buf := make([]byte, 2)
		binary.LittleEndian.PutUint16(buf, uint16(int16(v)))
		data = append(data, buf...)
	}
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+len(data)))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(header[22:24], 1) // mono
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(header[32:34], 2)
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(len(data)))
	return append(header, data...)
}

func htmlPageWithMedia(src string) string {
	return fmt.Sprintf(`<!doctype html><html><head><title>Test Player</title>
<meta property="og:title" content="Test Tone">
</head><body style="background:#000">
<audio id="tone" src="%s" controls autoplay loop></audio>
</body></html>`, src)
}

// TestIntegrationMediaControls drives pause/play/volume/rate/seek on a real
// media element in a real Chromium.
func TestIntegrationMediaControls(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	wav := generateWAV(1, 44100, 440, 0.3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tone.wav":
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(wav)
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(htmlPageWithMedia("/tone.wav")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, srv.URL+"/page")

	st, err := probeStatus(page)
	if err != nil || st.Count == 0 {
		t.Fatalf("no media found: %v %+v", err, st)
	}
	if st.Duration < 0.9 {
		t.Fatalf("wav duration missing: %.2f", st.Duration)
	}

	if _, err := probeAction(page, "pause", 0); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if st, _ = probeStatus(page); !st.Paused {
		t.Fatal("expected paused after pause")
	}
	if _, err := probeAction(page, "play", 0); err != nil {
		t.Fatalf("play: %v", err)
	}
	if _, err := probeAction(page, "volume", 40); err != nil {
		t.Fatal(err)
	}
	if _, err := probeAction(page, "rate", 1.5); err != nil {
		t.Fatal(err)
	}
	st, _ = probeStatus(page)
	if st.Muted {
		t.Fatal("expected unmuted")
	}
	if st.PlaybackRate != 1.5 {
		t.Fatalf("rate = %.2f", st.PlaybackRate)
	}
	if st.Volume < 0.39 || st.Volume > 0.41 {
		t.Fatalf("volume = %.3f", st.Volume)
	}
	if _, err := probeAction(page, "seek", -1000); err != nil {
		t.Fatal(err)
	}
	if st, _ = probeStatus(page); st.CurrentTime > 1 {
		t.Fatalf("seek clamp failed: %.2f", st.CurrentTime)
	}
	if st.Title != "Test Tone" {
		t.Fatalf("og:title not used: %q", st.Title)
	}
}

// TestIntegrationClickByText verifies the click helper used by the Meet flow.
func TestIntegrationClickByText(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Prejoin</title></head>
<body>
<button id="b1" onclick="document.title='CLICKED'">Join now</button>
<div role="button" style="width:80px;height:30px;margin-top:40px" onclick="document.title='ASKED'">Ask to join</div>
</body></html>`))
	}))
	defer srv.Close()

	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, srv.URL)

	if clicked, label := clickByText(page, []string{"join now"}, 4*time.Second); !clicked {
		t.Fatalf("clickByText failed (label=%q)", label)
	}
	if got := evaluate(t, page, "document.title"); got != "CLICKED" {
		t.Fatalf("title = %q after click", got)
	}
	if clicked, _ := clickByText(page, []string{"ask to join"}, 4*time.Second); !clicked {
		t.Fatal("role=button click failed")
	}
	if got := evaluate(t, page, "document.title"); got != "ASKED" {
		t.Fatalf("title = %q after second click", got)
	}
}

// TestIntegrationTitleLock verifies the pinned tab title survives page updates.
func TestIntegrationTitleLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, `data:text/html,<html><head><title>Initial</title></head><body>ok</body></html>`)

	if _, err := page.Eval(titleLockJS("TeleMeet Media")); err != nil {
		t.Fatalf("title lock eval: %v", err)
	}
	evaluate(t, page, `document.title = 'Hijacked'`)
	time.Sleep(1400 * time.Millisecond)
	if got := evaluate(t, page, `document.title`); got != "TeleMeet Media" {
		t.Fatalf("title lock lost: %q", got)
	}
	if got := evaluate(t, page, `window.__tmOrigTitle`); got != "Initial" {
		t.Fatalf("original title not tracked: %q", got)
	}
}

// TestIntegrationPlayerDocument covers the embedded player page end to end.
func TestIntegrationPlayerDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	wav := generateWAV(2, 44100, 330, 0.2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wav)
	}))
	defer srv.Close()

	tgt, err := ClassifyMedia(srv.URL + "/tone.wav")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := PlayerDataURL(tgt, PlayerOptions{TabTitle: "TeleMeet Media", Autoplay: false, Volume: 70, Loop: true})
	if err != nil {
		t.Fatal(err)
	}
	b, closeFn := newTestBrowser(t)
	defer closeFn()
	page := openPage(t, b, doc)

	// The player locks its title and exposes exactly one media element.
	if got := evaluate(t, page, `document.title`); got != "TeleMeet Media" {
		t.Fatalf("player title = %q", got)
	}
	st, err := probeStatus(page)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Count != 1 {
		t.Fatalf("media count = %d", st.Count)
	}
	if _, err := probeAction(page, "play", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	st, _ = probeStatus(page)
	if st.Paused {
		t.Fatalf("player did not start: %+v", st)
	}
	if st.Volume < 0.69 || st.Volume > 0.71 {
		t.Fatalf("volume = %.2f", st.Volume)
	}
}

// TestIntegrationTabCapture runs the same display-capture probe the doctor uses.
func TestIntegrationTabCapture(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	browserBin(t) // skip without a browser
	// Serve the secure-context requester locally so the test needs no internet.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>probe</title><body>probe</body>`))
	}))
	defer srv.Close()
	old := captureProbeURL
	captureProbeURL = srv.URL
	defer func() { captureProbeURL = old }()

	m := NewManager(
		BrowserConfig{Executable: browserBin(t), Headless: true, WindowWidth: 1280, WindowHeight: 800},
		MediaOptions{},
		MeetOptions{PresentMode: "tab", CaptureSource: "TeleMeet Media"},
		testLogger{t},
	)
	rep, err := TestTabCapture(context.Background(), m.browserCfg, m.meetCfg, testLogger{t})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !rep.Video || rep.Frames == 0 {
		t.Fatalf("capture report incomplete: %+v", rep)
	}
	if strings.TrimSpace(rep.Label) == "" {
		t.Fatalf("no source label: %+v", rep)
	}
}
