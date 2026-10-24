package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// Presentation surface handling
//
// The Meet share is a capture of the media tab's surface, so whatever the tab
// paints is what participants see. Three things have to hold for that to be the
// video and not a dialog: the consent wall is gone (consent.go), the viewport is
// the full capture surface, and the page chrome (masthead, related column, player
// gradients/overlays) is hidden. The CSS below is cosmetic and reversible; it is
// applied AFTER the consent wall is dismissed because a hidden dialog cannot be
// clicked.

// mediaSettleWindow is how long a buffered video must stay consent-free before
// the tab counts as ready to present. YouTube mounts the consent lightbox a few
// seconds after the watch page loads, so "the video is ready" alone would let a
// wall pop up over the share right after it starts.
const mediaSettleWindow = 5 * time.Second

// mediaFitCSS hides YouTube's chrome so the video fills the shared surface.
const mediaFitCSS = `
html, body { margin: 0 !important; padding: 0 !important; overflow: hidden !important; background: #000 !important; }
#masthead-container, ytd-masthead, #secondary, ytd-watch-next-secondary-results-renderer,
ytd-watch-flexy #secondary, #chat, #related { display: none !important; }
tp-yt-iron-overlay-backdrop, ytd-popup-container { display: none !important; }
#columns, #primary, #player-container, #player-container-inner, #player, ytd-player, #movie_player {
  max-width: none !important; max-height: 100vh !important; width: 100vw !important;
  height: 100vh !important; margin: 0 !important; padding: 0 !important;
  left: 0 !important; top: 0 !important; background: #000 !important; border-radius: 0 !important;
}
.html5-video-container { top: 0 !important; left: 0 !important; width: 100% !important; height: 100% !important; }
#movie_player video, .html5-main-video, video {
  width: 100% !important; height: 100% !important; max-height: 100vh !important;
  left: 0 !important; top: 0 !important; object-fit: contain !important; background: #000 !important;
}
.ytp-chrome-top, .ytp-chrome-bottom, .ytp-gradient-top, .ytp-gradient-bottom, .ytp-pause-overlay,
.ytp-ce-element, .ytp-endscreen-content, .ytp-bezel, .ytp-tooltip, .ytp-spinner, .ytp-scrubber-container,
.ytp-cued-thumbnail-overlay, .ytp-unmute, .ytp-large-play-button, .ytp-share-button-visible { display: none !important; }
`

// mediaFitJS installs the cosmetic CSS and reports the resulting geometry so the
// caller can log (and test) that the video really owns the surface.
func mediaFitJS() string {
	css, _ := json.Marshal(mediaFitCSS)
	return "() => {\n" + consentPreludeJS() + fmt.Sprintf(`
  const CSS = %s;
  let style = document.getElementById("tm-media-fit");
  if (!style) { style = document.createElement("style"); style.id = "tm-media-fit"; (document.head || document.documentElement).appendChild(style); }
  style.textContent = CSS;

  const box = (sel) => {
    try { const el = document.querySelector(sel); if (!el) return null;
      const r = el.getBoundingClientRect(); return {w: Math.round(r.width), h: Math.round(r.height), x: Math.round(r.left), y: Math.round(r.top)};
    } catch (e) { return null; }
  };
  const hidden = (sel) => {
    try { const el = document.querySelector(sel); if (!el) return true;
      const cs = getComputedStyle(el); return cs.display === "none" || cs.visibility === "hidden";
    } catch (e) { return true; }
  };
  const vids = Array.from(document.querySelectorAll("video"));
  let v = vids.find(function (x) { return x.readyState >= 2 && !x.paused; }) || vids[0] || null;
  const vbox = v ? box("video") : null;
  const playerBox = box("#movie_player") || box("ytd-player") || box("#player");
  // Overlay-free: the topmost element at the centre of the video belongs to the
  // player, and no consent surface is still on screen.
  let overlayFree = false, topTag = "";
  try {
    const w = (vbox && vbox.w) || innerWidth, h = (vbox && vbox.h) || innerHeight;
    const cx = Math.max(1, Math.min(innerWidth - 2, Math.round(((vbox && vbox.x) || 0) + w / 2)));
    const cy = Math.max(1, Math.min(innerHeight - 2, Math.round(((vbox && vbox.y) || 0) + h / 2)));
    const top = document.elementFromPoint(cx, cy);
    topTag = top ? (top.tagName || "").toLowerCase() : "";
    const player = document.querySelector("#movie_player") || document.querySelector("ytd-player") || document.querySelector("#player");
    overlayFree = !!top && (top === v || topTag === "video" || (player && (deepHas(player, top) || deepHas(top, player))));
    if (consentSurfaces().length || onConsentHost()) overlayFree = false;
  } catch (e) {}
  return JSON.stringify({
    css: true,
    viewport: {w: innerWidth, h: innerHeight},
    screen: {w: screen.availWidth || 0, h: screen.availHeight || 0},
    video: vbox, player: playerBox,
    hiddenMasthead: hidden("#masthead-container") && hidden("ytd-masthead"),
    hiddenRelated: hidden("#secondary"),
    hiddenConsent: hidden("ytd-consent-bump-v2-lightbox") && !onConsentHost(),
    overlayFree: overlayFree, topTag: topTag
  });
}`, string(css))
}

// FitReport is the outcome of fitting the media tab to the capture surface.
type FitReport struct {
	CSS            bool `json:"css"`
	ViewportW      int  `json:"viewportW"`
	ViewportH      int  `json:"viewportH"`
	ScreenW        int  `json:"screenW"`
	ScreenH        int  `json:"screenH"`
	VideoW         int  `json:"videoW"`
	VideoH         int  `json:"videoH"`
	HiddenMasthead bool `json:"hiddenMasthead"`
	HiddenRelated  bool `json:"hiddenRelated"`
	HiddenConsent  bool `json:"hiddenConsent"`
	OverlayFree    bool `json:"overlayFree"`
	TopTag         string
}

// describe renders the fit for logs.
func (f FitReport) describe() string {
	return fmt.Sprintf("viewport=%dx%d screen=%dx%d video=%dx%d masthead_hidden=%t related_hidden=%t consent_hidden=%t overlay_free=%t top=%s",
		f.ViewportW, f.ViewportH, f.ScreenW, f.ScreenH, f.VideoW, f.VideoH,
		f.HiddenMasthead, f.HiddenRelated, f.HiddenConsent, f.OverlayFree, f.TopTag)
}

// maximizeViewport makes the tab's surface as large as the browser's screen and
// confirms the layout viewport actually grew. Headless Chromium needs the
// device-metrics override (there is no window manager); headed builds also get
// a real window maximize.
func maximizeViewport(p *rod.Page, cfg BrowserConfig, log Logger) (int, int, error) {
	if log == nil {
		log = noopLogger{}
	}
	var dims []int
	res, err := p.Timeout(5 * time.Second).Eval(`() => [window.screen && screen.availWidth || 0, window.screen && screen.availHeight || 0, innerWidth || 0, innerHeight || 0]`)
	if err == nil {
		for _, v := range res.Value.Arr() {
			dims = append(dims, int(v.Num()))
		}
	}
	w, h := widthOf(cfg), heightOf(cfg)
	if len(dims) >= 2 && dims[0] > 0 && dims[1] > 0 {
		w, h = dims[0], dims[1]
	}
	// Never shrink below the configured window: the capture is the tab surface.
	if w < widthOf(cfg) {
		w = widthOf(cfg)
	}
	if h < heightOf(cfg) {
		h = heightOf(cfg)
	}
	if !cfg.Headless {
		if win, werr := (proto.BrowserGetWindowForTarget{}).Call(p); werr == nil {
			bounds := &proto.BrowserBounds{WindowState: proto.BrowserWindowStateMaximized}
			if err := (proto.BrowserSetWindowBounds{WindowID: win.WindowID, Bounds: bounds}).Call(p); err != nil {
				log.Debugf("[PIPELINE:media] window maximize failed: %v", err)
			}
		}
	}
	sw, sh := w, h
	err = p.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: w, Height: h, DeviceScaleFactor: 1,
		ScreenWidth: &sw, ScreenHeight: &sh,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("set the media viewport to %dx%d: %w", w, h, err)
	}
	got := []int{}
	if res, err := p.Timeout(5 * time.Second).Eval(`() => [innerWidth, innerHeight]`); err == nil {
		for _, v := range res.Value.Arr() {
			got = append(got, int(v.Num()))
		}
	}
	if len(got) == 2 {
		if got[0] < w || got[1] < h {
			log.Warnf("[PIPELINE:media] viewport %dx%d is smaller than the requested %dx%d", got[0], got[1], w, h)
		}
		w, h = got[0], got[1]
	}
	return w, h, nil
}

// fitMediaTab maximizes the viewport, installs the cosmetic CSS and verifies the
// video (not chrome or a dialog) owns the surface.
func fitMediaTab(p *rod.Page, cfg BrowserConfig, log Logger) (FitReport, error) {
	var rep FitReport
	if _, _, err := maximizeViewport(p, cfg, log); err != nil {
		return rep, err
	}
	res, err := p.Timeout(8 * time.Second).Eval(mediaFitJS())
	if err != nil {
		return rep, fmt.Errorf("fit the media tab: %w", err)
	}
	var raw struct {
		CSS      bool `json:"css"`
		Viewport struct {
			W int `json:"w"`
			H int `json:"h"`
		} `json:"viewport"`
		Screen struct {
			W int `json:"w"`
			H int `json:"h"`
		} `json:"screen"`
		Video          *struct{ W, H int } `json:"video"`
		Player         *struct{ W, H int } `json:"player"`
		HiddenMasthead bool                `json:"hiddenMasthead"`
		HiddenRelated  bool                `json:"hiddenRelated"`
		HiddenConsent  bool                `json:"hiddenConsent"`
		OverlayFree    bool                `json:"overlayFree"`
		TopTag         string              `json:"topTag"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &raw); err != nil {
		return rep, fmt.Errorf("decode fit report: %w", err)
	}
	rep = FitReport{
		CSS: raw.CSS, ViewportW: raw.Viewport.W, ViewportH: raw.Viewport.H,
		ScreenW: raw.Screen.W, ScreenH: raw.Screen.H,
		HiddenMasthead: raw.HiddenMasthead, HiddenRelated: raw.HiddenRelated,
		HiddenConsent: raw.HiddenConsent, OverlayFree: raw.OverlayFree, TopTag: raw.TopTag,
	}
	if raw.Video != nil {
		rep.VideoW, rep.VideoH = raw.Video.W, raw.Video.H
	}
	return rep, nil
}

// playbackSampleJS reads playback state and the media element's rendered pixels
// in one round-trip. Consent state rides along: a wall that appears mid-stream
// looks exactly like a frozen video, and the fix is different.
func playbackSampleJS() string {
	return "() => {\n" + consentPreludeJS() + `
  const out = {cur: 0, dur: 0, paused: true, muted: true, ready: 0, w: 0, h: 0,
               r: 0, g: 0, b: 0, painted: false, drawErr: "", elementErr: "",
               src: "", title: "", videos: 0, consent: false, surface: "", playerState: -1,
               problem: "", url: location.href};
  const surfaces = consentSurfaces();
  out.consent = onConsentHost() || surfaces.length > 0;
  out.surface = surfaces.length ? surfaces[0] : (onConsentHost() ? "consent-host" : "");

  const items = [];
  const walk = (doc, depth) => {
    if (!doc || depth > 6) return;
    let vids = [];
    try { vids = Array.from(doc.querySelectorAll('video,audio')); } catch (e) { return; }
    for (const v of vids) {
      let r = {width: 0, height: 0};
      try { r = v.getBoundingClientRect(); } catch (e) {}
      const area = Math.max(0, r.width | 0) * Math.max(0, r.height | 0);
      const dur = isFinite(v.duration) ? v.duration : 0;
      let score = area + (dur > 0 ? 500000 : 0) + (v.paused ? 0 : 250000);
      if (v.ended) score -= 1000;
      items.push({v: v, score: score});
    }
    let frames = [];
    try { frames = Array.from(doc.querySelectorAll('iframe')); } catch (e) {}
    for (const f of frames) {
      try { walk(f.contentDocument, depth + 1); } catch (e) {}
    }
  };
  walk(document, 0);
  out.videos = items.length;
  try {
    const mp = document.querySelector('#movie_player');
    if (mp && mp.getPlayerState) out.playerState = mp.getPlayerState();
  } catch (e) {}
  let v = null;
  if (items.length) { items.sort((a, b) => b.score - a.score); v = items[0].v; }
  const probe = document.querySelector('#movie_player');
  if (!v && probe) {
    try { const ps = probe.getPlayerResponse ? probe.getPlayerResponse() : null;
      if (ps && ps.playabilityStatus) out.problem = String(ps.playabilityStatus.status || "") + " " + String(ps.playabilityStatus.reason || ""); } catch (e) {}
  }
  if (!v) { out.drawErr = "no media element"; return JSON.stringify(out); }
  const r = v.getBoundingClientRect();
  out.w = Math.round(r.width); out.h = Math.round(r.height);
  out.paused = !!v.paused; out.muted = !!v.muted;
  out.cur = v.currentTime || 0;
  out.dur = isFinite(v.duration) ? v.duration : 0;
  out.ready = v.readyState || 0;
  out.src = String(v.currentSrc || v.src || "").slice(0, 200);
  if (v.error) out.elementErr = "media error " + (v.error.code || "?") + " " + (v.error.message || "");
  try {
    const og = document.querySelector('meta[property="og:title"], meta[name="og:title"]');
    out.title = (og && og.content) || window.__tmOrigTitle || document.title || "";
  } catch (e) {}
  try {
    const c = document.createElement("canvas");
    c.width = 96; c.height = 54;
    const ctx = c.getContext("2d", {willReadFrequently: true});
    if (v.readyState < 2) {
      out.drawErr = "readyState " + v.readyState + " (not enough data)";
    } else {
      ctx.drawImage(v, 0, 0, c.width, c.height);
      const d = ctx.getImageData(0, 0, c.width, c.height).data;
      let rs = 0, gs = 0, bs = 0;
      for (let p = 0; p < d.length; p += 4) { rs += d[p]; gs += d[p+1]; bs += d[p+2]; }
      const n = d.length / 4;
      out.r = rs / n; out.g = gs / n; out.b = bs / n;
      out.painted = n > 0;
    }
  } catch (e) { out.drawErr = String(e).slice(0, 160); }
  return JSON.stringify(out);
}`
}

// PlaybackSample is one reading of the media tab: position, readiness and the
// actual painted pixels of the media element.
type PlaybackSample struct {
	Current     float64 `json:"cur"`
	Duration    float64 `json:"dur"`
	Paused      bool    `json:"paused"`
	Muted       bool    `json:"muted"`
	Ready       int     `json:"ready"`
	Videos      int     `json:"videos"`
	Width       int     `json:"w"`
	Height      int     `json:"h"`
	MeanR       float64 `json:"r"`
	MeanG       float64 `json:"g"`
	MeanB       float64 `json:"b"`
	Painted     bool    `json:"painted"`
	DrawErr     string  `json:"drawErr"`
	ElementErr  string  `json:"elementErr"`
	Src         string  `json:"src"`
	Title       string  `json:"title"`
	Consent     bool    `json:"consent"`
	Surface     string  `json:"surface"`
	PlayerState int     `json:"playerState"`
	Problem     string  `json:"problem"`
	URL         string  `json:"url"`
}

// IsBlack reports a frame with (almost) no light at all.
func (s PlaybackSample) IsBlack() bool {
	return s.Painted && s.MeanR < 12 && s.MeanG < 12 && s.MeanB < 12
}

// IsGreen reports the decoder-failure signature: green dominant, red/blue near
// zero.
func (s PlaybackSample) IsGreen() bool {
	return s.Painted && s.MeanG > 120 && s.MeanR < 80 && s.MeanB < 80
}

// PlaybackReport is the verdict of ensurePlayback.
type PlaybackReport struct {
	Progressed  bool    `json:"progressed"`
	Advance     float64 `json:"advance"`
	Current     float64 `json:"current"`
	Duration    float64 `json:"duration"`
	Paused      bool    `json:"paused"`
	Ready       int     `json:"ready"`
	Painted     bool    `json:"painted"`
	Black       bool    `json:"black"`
	Green       bool    `json:"green"`
	MeanR       float64 `json:"meanR"`
	MeanG       float64 `json:"meanG"`
	MeanB       float64 `json:"meanB"`
	Samples     int     `json:"samples"`
	Consent     bool    `json:"consent"`
	Surface     string  `json:"surface"`
	DrawErr     string  `json:"drawErr"`
	PlayerState int     `json:"playerState"`
	PageProblem string  `json:"pageProblem"`
	Problem     string  `json:"problem"`
	URL         string  `json:"url"`
	Title       string  `json:"title"`
}

// describe renders the report for logs and error messages.
func (r PlaybackReport) describe() string {
	if r.Consent {
		surface := r.Surface
		if surface == "" {
			surface = "unknown"
		}
		return fmt.Sprintf("advance=%.2fs pos=%.2fs/%.2fs paused=%t ready=%d painted=%t black=%t green=%t rgb=(%.0f,%.0f,%.0f) samples=%d consent=true(surface=%q) drawErr=%q playerState=%d title=%q url=%s",
			r.Advance, r.Current, r.Duration, r.Paused, r.Ready, r.Painted, r.Black, r.Green,
			r.MeanR, r.MeanG, r.MeanB, r.Samples, surface, r.DrawErr, r.PlayerState, truncateStr(r.Title, 60), r.URL)
	}
	return fmt.Sprintf("advance=%.2fs pos=%.2fs/%.2fs paused=%t ready=%d painted=%t black=%t green=%t rgb=(%.0f,%.0f,%.0f) samples=%d consent=%t drawErr=%q playerState=%d title=%q",
		r.Advance, r.Current, r.Duration, r.Paused, r.Ready, r.Painted, r.Black, r.Green,
		r.MeanR, r.MeanG, r.MeanB, r.Samples, r.Consent, r.DrawErr, r.PlayerState, truncateStr(r.Title, 60))
}

// failure builds the actionable error for a verification that did not pass.
func (r PlaybackReport) failure(stage string) error {
	var why []string
	switch {
	case r.Consent:
		why = append(why, fmt.Sprintf("a consent wall is on screen (surface=%q) — the share is showing the dialog, not the video", r.Surface))
	case r.Samples == 0:
		why = append(why, "no readable sample came back from the media tab (page busy, crashed or detached)")
	case r.Green:
		why = append(why, "the video paints GREEN frames — this build cannot decode the stream")
	case r.Black:
		why = append(why, "the video paints BLACK frames — decoder/compositor failure, so the share is blank")
	case r.Paused:
		why = append(why, "the player stayed paused after the play request (autoplay policy or Kevlar pause)")
	case !r.Progressed:
		why = append(why, fmt.Sprintf("the player is not advancing (stuck at %.2fs of %.2fs)", r.Current, r.Duration))
	case r.DrawErr != "":
		why = append(why, fmt.Sprintf("frames could not be sampled: %s", r.DrawErr))
	default:
		why = append(why, "playback did not reach the verification bar")
	}
	if r.PageProblem != "" {
		why = append(why, "page problem: "+r.PageProblem)
	}
	return fmt.Errorf("%s: %s [%s] at %s", stage, strings.Join(why, "; "), r.describe(), r.URL)
}

// readPlaybackSample reads one playback + pixel sample from the media tab.
func readPlaybackSample(p *rod.Page) (PlaybackSample, error) {
	res, err := p.Timeout(8 * time.Second).Eval(playbackSampleJS())
	if err != nil {
		return PlaybackSample{}, err
	}
	var s PlaybackSample
	if err := json.Unmarshal([]byte(res.Value.Str()), &s); err != nil {
		return PlaybackSample{}, fmt.Errorf("decode playback sample: %w", err)
	}
	return s, nil
}

// MediaState is the readiness snapshot of the media tab.
type MediaState struct {
	Consent     bool    `json:"consent"`
	Surface     string  `json:"surface"`
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Videos      int     `json:"videos"`
	Ready       int     `json:"ready"`
	Duration    float64 `json:"dur"`
	Current     float64 `json:"cur"`
	Paused      bool    `json:"paused"`
	Width       int     `json:"w"`
	Height      int     `json:"h"`
	PlayerState int     `json:"playerState"`
	Problem     string  `json:"problem"`
	ViewportW   int     `json:"vw"`
	ViewportH   int     `json:"vh"`
	TopTag      string  `json:"topTag"`
}

// describe renders the state for logs and errors.
func (m MediaState) describe() string {
	return fmt.Sprintf("url=%s videos=%d ready=%d dur=%.2fs paused=%t playerState=%d consent=%t surface=%q viewport=%dx%d top=%q problem=%q title=%q",
		m.URL, m.Videos, m.Ready, m.Duration, m.Paused, m.PlayerState, m.Consent, m.Surface,
		m.ViewportW, m.ViewportH, m.TopTag, truncateStr(m.Problem, 80), truncateStr(m.Title, 60))
}

// readyStateJS reports the media tab's readiness (video page fully loaded) plus
// consent state, so a wait loop can tell "still loading" from "walled".
func readyStateJS() string {
	return "() => {\n" + consentPreludeJS() + `
  const out = {consent: false, surface: "", url: location.href, title: String(document.title || "").slice(0, 120),
               videos: 0, ready: 0, dur: 0, cur: 0, paused: true, w: 0, h: 0, playerState: -1,
               problem: "", vw: innerWidth, vh: innerHeight, topTag: ""};
  const surfaces = consentSurfaces();
  out.consent = onConsentHost() || surfaces.length > 0;
  out.surface = surfaces.length ? surfaces[0] : (onConsentHost() ? "consent-host" : "");
  const vids = Array.from(document.querySelectorAll('video'));
  out.videos = vids.length;
  let best = null, bestScore = -1;
  for (const v of vids) {
    const r = v.getBoundingClientRect();
    const dur = isFinite(v.duration) ? v.duration : 0;
    const score = Math.max(0, r.width | 0) * Math.max(0, r.height | 0) + (dur > 0 ? 500000 : 0);
    if (score > bestScore) { bestScore = score; best = v; }
  }
  if (best) {
    const r = best.getBoundingClientRect();
    out.ready = best.readyState || 0;
    out.dur = isFinite(best.duration) ? best.duration : 0;
    out.cur = best.currentTime || 0;
    out.paused = !!best.paused;
    out.w = Math.round(r.width); out.h = Math.round(r.height);
    if (best.error) out.problem = "media error " + best.error.code;
  }
  try {
    const mp = document.querySelector('#movie_player');
    if (mp) {
      if (mp.getPlayerState) out.playerState = mp.getPlayerState();
      const pr = mp.getPlayerResponse ? mp.getPlayerResponse() : null;
      const ps = pr && pr.playabilityStatus;
      if (ps && ps.status && ps.status !== "OK") out.problem = String(ps.status) + " " + String(ps.reason || "");
    }
  } catch (e) {}
  try {
    const cx = Math.round(innerWidth / 2), cy = Math.round(innerHeight / 2);
    const top = document.elementFromPoint(cx, cy);
    out.topTag = top ? String(top.tagName || "").toLowerCase() : "";
  } catch (e) {}
  return JSON.stringify(out);
}`
}

// readMediaState reads the readiness snapshot.
func readMediaState(p *rod.Page) (MediaState, error) {
	res, err := p.Timeout(8 * time.Second).Eval(readyStateJS())
	if err != nil {
		return MediaState{}, err
	}
	var m MediaState
	if err := json.Unmarshal([]byte(res.Value.Str()), &m); err != nil {
		return MediaState{}, fmt.Errorf("decode media state: %w", err)
	}
	return m, nil
}

// waitMediaReady waits until the video page is really playable: consent cleared,
// a media element with metadata and enough buffered data. It dismisses any wall
// that appears while waiting instead of failing the session on it.
func waitMediaReady(p *rod.Page, log Logger, timeout time.Duration) (MediaState, error) {
	if log == nil {
		log = noopLogger{}
	}
	if timeout <= 0 {
		timeout = 35 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var last MediaState
	var lastErr error
	walls := 0
	found := false
	var readySince time.Time
	for time.Now().Before(deadline) {
		st, err := readMediaState(p)
		if err != nil {
			lastErr = err
			time.Sleep(400 * time.Millisecond)
			continue
		}
		last = st
		if st.Consent {
			walls++
			if walls > 4 {
				return st, fmt.Errorf("consent wall keeps coming back (%d times, surface=%q) on %s — this page cannot reach the video", walls, st.Surface, st.URL)
			}
			log.Infof("[PIPELINE:media] consent wall before playback (surface=%q); dismissing", st.Surface)
			res, derr := dismissConsent(p, log, 15*time.Second)
			if derr != nil {
				log.Warnf("[PIPELINE:media] consent dismissal: %v", derr)
			} else if res.Dismissed && res.Present {
				log.Infof("[PIPELINE:media] consent %s", res.describe())
			}
			// The page has to prove it stays consent-free before it counts as
			// ready: YouTube remounts the wall right after it is dismissed.
			readySince = time.Time{}
			time.Sleep(400 * time.Millisecond)
			continue
		}
		if st.Videos > 0 {
			if !found {
				found = true
				log.Infof("[PIPELINE:media] video element found (ready=%d dur=%.1fs paused=%t size=%dx%d)", st.Ready, st.Duration, st.Paused, st.Width, st.Height)
			}
			// readyState >= 2 (HAVE_CURRENT_DATA) + known duration: the first
			// play() then lands on real media instead of an empty frame, which
			// is the difference between a blank share and a real one.
			if st.Ready >= 2 && (st.Duration > 0 || st.Paused == false) {
				if readySince.IsZero() {
					readySince = time.Now()
				}
				// YouTube mounts "Before you continue to YouTube" a few seconds
				// AFTER the watch page reports a buffered video, so a video that
				// is merely ready is not yet presentable. Hold for a settle
				// window; any wall that appears inside it is dismissed above.
				if time.Since(readySince) >= mediaSettleWindow {
					return st, nil
				}
			} else {
				readySince = time.Time{}
			}
		} else {
			readySince = time.Time{}
		}
		if st.Problem != "" && st.Videos == 0 {
			return st, fmt.Errorf("the media page cannot play this link: %s (%s)", st.Problem, st.describe())
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		return last, fmt.Errorf("video page never became ready within %s (%v): %s", timeout, lastErr, last.describe())
	}
	if last.Videos == 0 {
		return last, fmt.Errorf("no video element appeared within %s: %s (%s)", timeout, last.describe(), errHelpURL)
	}
	return last, fmt.Errorf("video element never buffered within %s: %s", timeout, last.describe())
}

// pressPlay asks the page to play: the YouTube player API when present, then the
// element itself. Pause is not blocked here (Kevlar calls pause() on background
// tabs; the media tab is forced visible, so a real pause means a real stall).
func pressPlay(p *rod.Page) {
	_, _ = p.Timeout(6 * time.Second).Eval(mediaControlJS("play", 0))
}

// ensurePlayback starts playback and verifies it with real frames: the position
// must advance AND the sampled pixels must be neither black nor green. Anything
// less is reported with the numbers needed to fix it.
func ensurePlayback(p *rod.Page, log Logger, minAdvance float64, timeout time.Duration) (PlaybackReport, error) {
	if log == nil {
		log = noopLogger{}
	}
	if minAdvance <= 0 {
		minAdvance = 1.0
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	var rep PlaybackReport
	pressPlay(p)
	deadline := time.Now().Add(timeout)
	first := -1.0
	var samples []PlaybackSample
	plays := 1
	extensions := 0
	for time.Now().Before(deadline) {
		s, err := readPlaybackSample(p)
		if err != nil {
			log.Debugf("[PIPELINE:playback] sample failed: %v", err)
			time.Sleep(600 * time.Millisecond)
			continue
		}
		if s.Consent {
			log.Infof("[PIPELINE:playback] consent wall appeared during playback (surface=%q); dismissing without touching the share", s.Surface)
			if _, derr := dismissConsent(p, log, 15*time.Second); derr != nil {
				log.Warnf("[PIPELINE:playback] consent dismissal: %v", derr)
			}
			// A wall costs the sample window we spent clearing it; restart the
			// clock (twice at most) so a slow dismissal cannot fail a good video.
			if extensions < 2 {
				extensions++
				deadline = time.Now().Add(timeout)
			}
			pressPlay(p)
			continue
		}
		samples = append(samples, s)
		if first < 0 {
			first = s.Current
		}
		if s.Paused && plays < 5 {
			plays++
			pressPlay(p)
		}
		last := s
		advance := last.Current - first
		rep = PlaybackReport{
			Advance: advance, Current: last.Current, Duration: last.Duration, Paused: last.Paused,
			Ready: last.Ready, Painted: last.Painted, MeanR: last.MeanR, MeanG: last.MeanG, MeanB: last.MeanB,
			Samples: len(samples), Consent: last.Consent, Surface: last.Surface, DrawErr: last.DrawErr,
			PlayerState: last.PlayerState, PageProblem: last.Problem, URL: last.URL, Title: last.Title,
			Black: last.IsBlack(), Green: last.IsGreen(),
		}
		// A single black frame right after a seek is normal; require the last
		// two readable samples to be real pixels.
		blackRun := 0
		for i := len(samples) - 1; i >= 0 && i >= len(samples)-2; i-- {
			if samples[i].Painted && samples[i].IsBlack() {
				blackRun++
			}
		}
		if advance >= minAdvance && rep.Painted && blackRun < 2 && !rep.Green {
			rep.Progressed = true
			log.Infof("[PIPELINE:playback] playback verified: %s", rep.describe())
			return rep, nil
		}
		time.Sleep(700 * time.Millisecond)
	}
	if rep.Samples == 0 {
		rep.Consent, rep.Surface = false, ""
		rep.URL = p.MustInfo().URL
	}
	// One last look so the error names the current blocker, not a stale one.
	if s, err := readPlaybackSample(p); err == nil {
		rep.Current, rep.Duration, rep.Paused, rep.Ready = s.Current, s.Duration, s.Paused, s.Ready
		rep.Painted, rep.MeanR, rep.MeanG, rep.MeanB = s.Painted, s.MeanR, s.MeanG, s.MeanB
		rep.Black, rep.Green = s.IsBlack(), s.IsGreen()
		rep.Consent, rep.Surface, rep.DrawErr, rep.Problem = s.Consent, s.Surface, s.DrawErr, s.Problem
		rep.PlayerState, rep.URL, rep.Title = s.PlayerState, s.URL, s.Title
	}
	rep.Progressed = rep.Advance >= minAdvance
	return rep, rep.failure("playback verification failed")
}

// prepareMedia makes the media tab presentation-ready: consent cleared, viewport
// maximized, chrome hidden and the video buffered.
func (s *Session) prepareMedia(p *rod.Page) error {
	res, err := dismissConsent(p, s.log, 25*time.Second)
	if err != nil {
		return fmt.Errorf("consent wall: %w", err)
	}
	if res.Present {
		s.log.Infof("[PIPELINE:media] consent %s", res.describe())
	}
	fit, ferr := fitMediaTab(p, s.mgr.browserCfg, s.log)
	if ferr != nil {
		s.log.Warnf("[PIPELINE:media] could not fit the media tab: %v", ferr)
	} else {
		s.log.Infof("[PIPELINE:media] surface fitted: %s", fit.describe())
		if !fit.OverlayFree {
			s.log.Warnf("[PIPELINE:media] an element still covers the video centre (top=%q)", fit.TopTag)
		}
	}
	st, err := waitMediaReady(p, s.log, 35*time.Second)
	if err != nil {
		if prob := s.playerProblem(); prob != "" {
			return fmt.Errorf("media unavailable: %s (%s)", prob, err)
		}
		return err
	}
	s.log.Infof("[PIPELINE:media] media ready: %s", st.describe())

	audioRep, atErr := ensureAudioTrack(p, s.MediaKind, s.AudioLang, s.log)
	if atErr != nil {
		s.log.Warnf("[PIPELINE:audio-track] session %s: requested_lang=%s: %v", s.ID, s.AudioLang, atErr)
		return fmt.Errorf("select audio track (%s): %w", s.AudioLang, atErr)
	}
	s.mu.Lock()
	s.audioRep = audioRep
	s.mu.Unlock()
	if s.MediaKind == MediaYouTube {
		s.log.Infof("[PIPELINE:audio-track] session %s: %s", s.ID, audioRep.describe())
	}
	// A skippable pre-roll that was still counting down during waitMediaReady
	// may have become available by now: press it before the share sees it.
	// This is best-effort diagnostics: the monitor already covers this path.
	if s.MediaKind == MediaYouTube {
		if rep, err := s.skipAdsOnce(); err != nil {
			s.log.Debugf("[PIPELINE:adskip] session %s: prepare scan: %v", s.ID, err)
		} else if rep.Skipped {
			s.log.Infof("[PIPELINE:adskip] session %s: skipped a YouTube ad before playback (%s)", s.ID, rep.describe())
		}
	}
	return nil
}

// verifyPlayback is the post-share gate: the video must advance with real
// frames before the session claims to be streaming.
func (s *Session) verifyPlayback() (PlaybackReport, error) {
	media := s.mediaTab()
	if media == nil {
		return PlaybackReport{}, errors.New("media page is not available")
	}
	rep, err := ensurePlayback(media, s.log, 1.0, 15*time.Second)
	if err != nil {
		return rep, err
	}
	s.mu.Lock()
	s.cur = rep.Current
	s.dur = rep.Duration
	s.paused = rep.Paused
	if rep.Title != "" {
		s.title = rep.Title
	}
	s.paused = false
	s.mu.Unlock()
	return rep, nil
}

// mediaHealth reports why the media tab is unhealthy ("" when it is fine). It
// reads the tab once, so the streaming watchdog can poll it cheaply.
func (s *Session) mediaHealth() (string, MediaState, error) {
	media := s.mediaTab()
	if media == nil {
		return "media tab is gone", MediaState{}, errors.New("media tab is gone")
	}
	st, err := readMediaState(media)
	if err != nil {
		return "cannot read the media tab", MediaState{}, err
	}
	switch {
	case st.Consent:
		return fmt.Sprintf("consent dialog is on screen (surface=%q)", st.Surface), st, nil
	case st.Videos == 0:
		return "the media tab has no video element", st, nil
	case st.Ready < 2:
		return fmt.Sprintf("the player is not ready (readyState=%d)", st.Ready), st, nil
	case st.Paused:
		return "the player is paused", st, nil
	}
	s.mu.RLock()
	cur := s.cur
	s.mu.RUnlock()
	if st.Current <= 0.01 && cur <= 0.01 && st.Duration > 0 {
		return "the player has not started", st, nil
	}
	return "", st, nil
}

// recoverMedia repairs the media tab while the Meet share keeps running. It
// never re-clicks Share Screen and never replaces the tab: getDisplayMedia is
// bound to this tab, so clearing the wall / re-fitting / replaying is invisible
// to participants apart from the video coming back.
func (s *Session) recoverMedia(reason string) error {
	media := s.mediaTab()
	if media == nil {
		return errors.New("media page is not available")
	}
	s.log.Warnf("[PIPELINE:recovery] media tab unhealthy (%s); recovering without touching the share", reason)
	s.setDetail("[6/6] Recovering media: " + reason)

	// A mid-roll ad can be the actual reason the tab looks unhealthy: a playing
	// ad freezes the media position while still painting. Pressing YouTube's own
	// skip button (only when it is available) is strictly cheaper than a refit
	// or a reload, so it goes first and never blocks recovery on failure.
	if s.MediaKind == MediaYouTube {
		if rep, err := s.skipAdsOnce(); err != nil {
			s.log.Debugf("[PIPELINE:adskip] session %s: recovery scan: %v", s.ID, err)
		} else if rep.Skipped {
			s.log.Infof("[PIPELINE:adskip] session %s: skipped a YouTube ad during recovery (%s)", s.ID, rep.describe())
		} else if rep.AdPlaying {
			s.log.Infof("[PIPELINE:adskip] session %s: ad playing during recovery, no usable skip button (%s)", s.ID, rep.Reason)
		}
	}

	if res, err := dismissConsent(media, s.log, 20*time.Second); err != nil {
		s.log.Warnf("[PIPELINE:recovery] consent dismissal: %v", err)
	} else if res.Present {
		s.log.Infof("[PIPELINE:recovery] consent %s", res.describe())
	}

	if fit, err := fitMediaTab(media, s.mgr.browserCfg, s.log); err != nil {
		s.log.Warnf("[PIPELINE:recovery] refit failed: %v", err)
	} else {
		s.log.Infof("[PIPELINE:recovery] surface refitted: %s", fit.describe())
	}

	if _, err := waitMediaReady(media, s.log, 20*time.Second); err != nil {
		s.log.Warnf("[PIPELINE:recovery] video not ready (%v); reloading the media tab (the share follows the tab)", err)
		if rerr := s.reopenMedia(); rerr != nil {
			return fmt.Errorf("recover media: %w (reload also failed: %v)", err, rerr)
		}
	} else {
		if atRep, atErr := ensureAudioTrack(media, s.MediaKind, s.AudioLang, s.log); atErr != nil {
			s.log.Warnf("[PIPELINE:audio-track] session %s recovery: requested_lang=%s: %v", s.ID, s.AudioLang, atErr)
		} else {
			s.mu.Lock()
			s.audioRep = atRep
			s.mu.Unlock()
			if s.MediaKind == MediaYouTube {
				s.log.Infof("[PIPELINE:audio-track] session %s recovery: %s", s.ID, atRep.describe())
			}
		}
	}

	if _, err := s.mediaAction("unmute", 0); err != nil {
		s.log.Debugf("[PIPELINE:recovery] unmute: %v", err)
	}
	rep, err := ensurePlayback(media, s.log, 1.0, 15*time.Second)
	if err != nil {
		return fmt.Errorf("recover media playback: %w", err)
	}
	s.mu.Lock()
	s.cur, s.dur, s.paused = rep.Current, rep.Duration, false
	s.mu.Unlock()
	s.setDetail("[6/6] Streaming live")
	s.log.Infof("[PIPELINE:recovery] media recovered, share stayed up: %s", rep.describe())
	return nil
}
