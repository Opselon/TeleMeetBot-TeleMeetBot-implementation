package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

// WatchReport is the verdict of a session that joins a meeting as a *viewer* and
// measures what a remote participant actually sees: decoded presentation frames
// (inbound-rtp) plus the pixels rendered on screen (screenshot motion). It is the
// only way to verify a share end to end without asking a human to describe what
// their eyes see.
type WatchReport struct {
	Joined     bool    `json:"joined"`
	Presenting bool    `json:"presenting"` // someone in the call is presenting
	Samples    int     `json:"samples"`
	InVideo    float64 `json:"inVideoFrames"` // inbound-rtp video frames received
	InFPS      float64 `json:"inFps"`
	InBytes    float64 `json:"inBytes"`
	Decoded    float64 `json:"decodedFrames"`
	DecFPS     float64 `json:"decodedFps"`
	InWidth    int     `json:"inWidth"`
	InHeight   int     `json:"inHeight"`
	CenterMean float64 `json:"centerMean"`   // mean luminance of the middle of the call
	CenterMove float64 `json:"centerMotion"` // mean per-pixel change between samples
	FullMean   float64 `json:"fullMean"`
	FullMove   float64 `json:"fullMotion"`
	Console    string  `json:"console"`
	Error      string  `json:"error"`
}

// Moves reports whether the remote view is showing moving video.
func (r WatchReport) Moves() bool {
	return r.Joined && r.Samples > 2 && r.CenterMove >= 0.35 && r.CenterMean >= 3
}

func (r WatchReport) Describe() string {
	return fmt.Sprintf("joined=%t presenting=%t samples=%d inbound=%.0ff(%.1ffps) decoded=%.0f(%.1f/s) %dx%d sentKB=%.0f center=mean%.1f motion%.2f full=mean%.1f motion%.2f err=%q",
		r.Joined, r.Presenting, r.Samples, r.InVideo, r.InFPS, r.Decoded, r.DecFPS,
		r.InWidth, r.InHeight, r.InBytes/1024, r.CenterMean, r.CenterMove, r.FullMean, r.FullMove, r.Error)
}

// WatchOptions configures WatchMeet.
type WatchOptions struct {
	MeetURL string
	Name    string // guest display name
	Seconds int    // how long to watch once inside the call
}

// WatchMeet joins a meeting as a silent guest and measures the presentation a
// remote participant would see. It never shares anything itself.
func WatchMeet(ctx context.Context, browserCfg BrowserConfig, meet MeetOptions, opt WatchOptions, log Logger) (WatchReport, error) {
	if log == nil {
		log = noopLogger{}
	}
	if opt.Seconds < 5 {
		opt.Seconds = 5
	}
	name := strings.TrimSpace(opt.Name)
	if name == "" {
		name = "TeleMeet Viewer"
	}
	rep := WatchReport{}
	b, closeFn, err := launchBrowser(launchOptions{browser: browserCfg, meet: meet, forMeet: true}, log)
	if err != nil {
		return rep, err
	}
	defer closeFn()

	p, err := newPage(b, browserCfg)
	if err != nil {
		return rep, err
	}
	defer func() { _ = p.Close() }()
	if _, err := p.EvalOnNewDocument(meetPeerHookJS()); err != nil {
		log.Warnf("[PIPELINE:watch] install hook: %v", err)
	}
	if err := p.Context(ctx).Timeout(60 * time.Second).Navigate(opt.MeetURL); err != nil {
		return rep, fmt.Errorf("open meeting: %w", err)
	}
	_ = p.WaitLoad()
	time.Sleep(1500 * time.Millisecond)

	// Join as a guest: name, mute camera/mic (no fake-device green tile), click.
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		stage, info := watchStage(p)
		switch stage {
		case "meeting":
			rep.Joined = true
		case "prejoin":
			_, _ = p.Eval(watchFillNameJS(name))
			_, _ = p.Eval(watchMuteJS())
			clicked, label := watchClickJoin(p)
			if clicked {
				log.Infof("[PIPELINE:watch] clicked %q", label)
			}
			time.Sleep(1200 * time.Millisecond)
			continue
		case "lobby", "blocked":
			log.Infof("[PIPELINE:watch] %s: %s", stage, info)
		case "login":
			return rep, fmt.Errorf("the meeting requires a signed-in Google account")
		}
		if rep.Joined {
			break
		}
		time.Sleep(1500 * time.Millisecond)
	}
	if !rep.Joined {
		return rep, fmt.Errorf("never joined the meeting (last stage read)")
	}
	log.Infof("[PIPELINE:watch] joined the call; watching for %ds", opt.Seconds)

	var prev *image.Gray
	var prevFull *image.Gray
	lastStats := MeetPublishStats{}
	firstStats := MeetPublishStats{}
	end := time.Now().Add(time.Duration(opt.Seconds) * time.Second)
	for time.Now().Before(end) {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		st := readMeetStats(p)
		if st.Present {
			rep.Presenting = true
		}
		if st.InVideo > lastStats.InVideo || rep.Samples == 0 {
			lastStats = st
			if firstStats.InVideo == 0 && st.InVideo > 0 {
				firstStats = st
				rep.InWidth, rep.InHeight = st.EncWidth, st.EncHeight
			}
		}
		if st.InVideo > 0 && rep.InWidth == 0 {
			rep.InWidth, rep.InHeight = st.EncWidth, st.EncHeight
		}
		shot, err := p.Screenshot(false, nil)
		if err == nil {
			if img, derr := png.Decode(bytes.NewReader(shot)); derr == nil {
				c := cropCenter(img, 0.6)
				g := toGray(c)
				full := toGray(img)
				if prev != nil {
					m, n := diffGray(g, prev)
					if n > 0 {
						rep.CenterMove += m
						rep.CenterMean += meanGray(g)
						rep.Samples++
					}
					fm, _ := diffGray(full, prevFull)
					rep.FullMove += fm
					rep.FullMean += meanGray(full)
				}
				prev, prevFull = g, full
			}
		}
		time.Sleep(1200 * time.Millisecond)
	}
	if rep.Samples > 0 {
		rep.CenterMove /= float64(rep.Samples)
		rep.CenterMean /= float64(rep.Samples)
		rep.FullMove /= float64(rep.Samples)
		rep.FullMean /= float64(rep.Samples)
	}
	rep.InVideo = lastStats.InVideo
	rep.Decoded = lastStats.InVideo // framesReceived==decoded for the decoder path
	rep.InBytes = lastStats.BytesSent
	if st := readMeetStats(p); st.Console != "" {
		rep.Console = st.Console
	}
	log.Infof("[PIPELINE:watch] %s", rep.Describe())
	if !rep.Moves() {
		log.Warnf("[PIPELINE:watch] no moving remote video measured (center motion=%.2f mean=%.1f)", rep.CenterMove, rep.CenterMean)
	}
	return rep, nil
}

// watchStage classifies the viewer's page with the same rules as a session.
func watchStage(p *rod.Page) (string, string) {
	res, err := p.Timeout(5 * time.Second).Eval(meetStageJS())
	if err != nil {
		return "unknown", err.Error()
	}
	arr := res.Value.Arr()
	if len(arr) < 2 {
		return "unknown", ""
	}
	return strings.ToLower(arr[0].Str()), strings.TrimSpace(arr[1].Str())
}

func watchFillNameJS(name string) string {
	enc, _ := json.Marshal(name)
	return fmt.Sprintf(`() => {
  const NAME = %s;
  for (const el of document.querySelectorAll('input')) {
    const r = el.getBoundingClientRect();
    if (r.width < 5 || r.height < 5) continue;
    const ph = ((el.getAttribute('placeholder') || "") + " " + (el.getAttribute('aria-label') || "")).toLowerCase();
    if (!(ph.includes("name") || el.value === "")) continue;
    el.focus(); el.value = NAME;
    el.dispatchEvent(new Event('input', {bubbles: true}));
    el.dispatchEvent(new Event('change', {bubbles: true}));
    el.blur();
    return true;
  }
  return false;
}`, enc)
}

func watchMuteJS() string {
	return `() => {
  const norm = x => (x || "").toLowerCase().trim();
  let cam = false, mic = false;
  for (const b of document.querySelectorAll('button, [role=button], div[role=button]')) {
    const label = norm((b.getAttribute('aria-label') || "") + " " + (b.getAttribute('data-tooltip') || ""));
    if (!cam && (label.includes("turn off camera") || label.includes("turn off video"))) { try { b.click(); cam = true; } catch (e) {} }
    if (!mic && (label.includes("turn off microphone") || label.includes("turn off mic"))) { try { b.click(); mic = true; } catch (e) {} }
  }
  return JSON.stringify({cam: cam, mic: mic});
}`
}

func watchClickJoin(p *rod.Page) (bool, string) {
	res, err := p.Timeout(5 * time.Second).Eval(`() => {
  const norm = x => (x || "").replace(/\s+/g, " ").trim().toLowerCase();
  let best = null, bestScore = 0;
  for (const el of document.querySelectorAll('button, [role=button], div[role=button]')) {
    const t = norm((el.getAttribute('aria-label') || "") + " " + (el.innerText || el.textContent || ""));
    let score = 0;
    if (t === "join now" || t === "ask to join") score = 3;
    else if (t.startsWith("join now")) score = 2;
    else if (t.includes("ask to join")) score = 2;
    if (score > bestScore) { best = el; bestScore = score; }
  }
  if (!best) return "";
  const r = best.getBoundingClientRect();
  if (r.width < 2 || r.height < 2) return "";
  best.click();
  return norm(best.innerText || best.textContent || "").slice(0, 40);
}`)
	if err != nil {
		return false, ""
	}
	label := strings.TrimSpace(res.Value.Str())
	return label != "", label
}

// readMeetStats reuses the publisher hook's reader (it reports inbound too).
func readMeetStats(p *rod.Page) MeetPublishStats {
	res, err := p.Timeout(6 * time.Second).Eval(meetPublishStatsJS())
	if err != nil {
		return MeetPublishStats{}
	}
	var st MeetPublishStats
	if json.Unmarshal([]byte(res.Value.Str()), &st) != nil {
		return MeetPublishStats{}
	}
	return st
}

func cropCenter(img image.Image, frac float64) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	cw, ch := int(float64(w)*frac), int(float64(h)*frac)
	x0 := b.Min.X + (w-cw)/2
	y0 := b.Min.Y + (h-ch)/2
	return img.(interface {
		SubImage(r image.Rectangle) image.Image
	}).SubImage(image.Rect(x0, y0, x0+cw, y0+ch))
}

func toGray(img image.Image) *image.Gray {
	b := img.Bounds()
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, gg, bb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			// Rec. 601 luma, 8-bit.
			g.SetGray(x, y, color.Gray{Y: uint8((299*float64(r>>8) + 587*float64(gg>>8) + 114*float64(bb>>8)) / 1000)})
		}
	}
	return g
}

func meanGray(g *image.Gray) float64 {
	if len(g.Pix) == 0 {
		return 0
	}
	var sum float64
	for _, v := range g.Pix {
		sum += float64(v)
	}
	return sum / float64(len(g.Pix))
}

// diffGray returns the mean absolute per-pixel change and the pixel count.
func diffGray(a, b *image.Gray) (float64, int) {
	if len(a.Pix) != len(b.Pix) || len(a.Pix) == 0 {
		return 0, 0
	}
	var sum float64
	for i := range a.Pix {
		sum += math.Abs(float64(a.Pix[i]) - float64(b.Pix[i]))
	}
	return sum / float64(len(a.Pix)), len(a.Pix)
}
