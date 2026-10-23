package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

// MediaProbeSample is one periodic reading of the media tab: playback position
// plus the actual painted pixels of the media element. It is the same sample the
// session uses to gate streaming (playback.go), so the probe and the live path
// can never disagree about what "playing" means.
type MediaProbeSample = PlaybackSample

// MediaProbeReport is the verdict of ProbeMedia.
type MediaProbeReport struct {
	URL           string    `json:"url"`
	Kind          MediaKind `json:"kind"`
	Samples       int       `json:"samples"`
	Progressed    bool      `json:"progressed"`
	Painted       bool      `json:"painted"`
	MeanR         float64   `json:"meanR"`
	MeanG         float64   `json:"meanG"`
	MeanB         float64   `json:"meanB"`
	GreenFrames   bool      `json:"green"`
	BlackFrames   bool      `json:"black"`
	Position      float64   `json:"position"`
	Duration      float64   `json:"duration"`
	PlayerErr     string    `json:"playerErr"`
	PageProblem   string    `json:"pageProblem"`
	CodecSupport  string    `json:"codec"`
	BodyText      string    `json:"bodyText"`
	YTPlayerState string    `json:"ytState"`
	Verdict       string    `json:"verdict"`

	// Consent + surface diagnostics: what the tab looked like before playback.
	ConsentPresent bool    `json:"consentPresent"`
	Consent        string  `json:"consent"`
	Viewport       string  `json:"viewport"`
	OverlayFree    bool    `json:"overlayFree"`
	ReadyState     int     `json:"readyState"`
	Advance        float64 `json:"advance"`
	Verification   string  `json:"verification"`
	AudioTrack     string  `json:"audioTrack,omitempty"`
	RequestedLang  string  `json:"requestedLang,omitempty"`
	SelectedTrack  string  `json:"selectedTrack,omitempty"`
	FallbackReason string  `json:"fallbackReason,omitempty"`
}

// codecSupportJS reports which codecs this page's browser can decode via MSE —
// a false "supported" here is fine, a missing one explains green frames.
func codecSupportJS() string {
	return `() => {
  const out = {};
  const probes = [["h264", 'video/mp4; codecs="avc1.42E01E"'],
                  ["aac", 'audio/mp4; codecs="mp4a.40.2"'],
                  ["vp9", 'video/webm; codecs="vp9"'],
                  ["vp9-mp4", 'video/mp4; codecs="vp09.00.10.08"'],
                  ["av1", 'video/mp4; codecs="av01.0.00M.08"']];
  for (const [name, q] of probes) {
    try { out[name] = !!window.MediaSource && window.MediaSource.isTypeSupported(q); }
    catch (e) { out[name] = false; }
  }
  return JSON.stringify(out);
}`
}

// pageTextJS returns all visible text across shadow roots (YouTube renders in
// shadow DOM, so body.innerText alone comes back empty even on error walls).
func pageTextJS() string {
	return `() => {
  const parts = [];
  const walk = (node) => {
    if (!node) return;
    if (node.nodeType === 3) { parts.push(node.textContent); return; }
    if (node.nodeType !== 1 && node.nodeType !== 11) return;
    if (node.shadowRoot) walk(node.shadowRoot);
    for (const child of node.childNodes) walk(child);
  };
  walk(document.body);
  const text = parts.join(" ").replace(/\s+/g, " ").trim();
  return text.slice(0, 600);
}`
}

// youtubeStateJS reports what YouTube's own player thinks is wrong — its
// playabilityStatus carries the server-side verdict ("bot check", "unavailable",
// "age-restricted") that the UI may hide behind shadow DOM.
func youtubeStateJS() string {
	return `() => {
  const out = {player: false, state: -1, playability: "", videoData: "", bodyClass: ""};
  try { out.bodyClass = String(document.body.className || "").slice(0, 80); } catch (e) {}
  try {
    const p = document.querySelector("#movie_player");
    if (!p) return JSON.stringify(out);
    out.player = true;
    if (p.getPlayerState) out.state = p.getPlayerState();
    if (p.getVideoData) { try { out.videoData = JSON.stringify(p.getVideoData()).slice(0, 240); } catch (e) {} }
    if (p.getPlayerResponse) {
      try {
        const pr = p.getPlayerResponse() || {};
        const ps = pr.playabilityStatus || {};
        out.playability = JSON.stringify({status: ps.status, reason: ps.reason,
          subreason: ps.errorScreen && ps.errorScreen.subreasonText && ps.errorScreen.subreasonText.runs ?
            ps.errorScreen.subreasonText.runs.map(r => r.text).join(" ") : ""}).slice(0, 400);
      } catch (e) {}
    }
  } catch (e) { out.playability = "probe error: " + String(e); }
  return JSON.stringify(out);
}`
}

// ProbeMedia opens a media link through the same pipeline a session uses (the
// embedded player for direct files, the raw page otherwise), starts playback,
// then samples playback position and rendered pixels once per second. It
// answers, WITHOUT a meeting: does the media actually play, and does it paint
// real frames (not the decoder-failure green or black)?
func ProbeMedia(ctx context.Context, browserCfg BrowserConfig, link string, seconds int, log Logger) (MediaProbeReport, error) {
	return ProbeMediaWithLang(ctx, browserCfg, link, seconds, "fa", log)
}

// ProbeMediaWithLang runs the probe enforcing the requested audio language ("fa", "en", "orig", "auto").
func ProbeMediaWithLang(ctx context.Context, browserCfg BrowserConfig, link string, seconds int, reqLang string, log Logger) (MediaProbeReport, error) {
	if log == nil {
		log = noopLogger{}
	}
	if seconds < 10 {
		seconds = 10
	}
	target, mt, err := mediaTargetFor(link)
	if err != nil {
		return MediaProbeReport{}, err
	}

	b, closeFn, err := launchBrowser(launchOptions{browser: browserCfg, forMeet: true}, log)
	if err != nil {
		return MediaProbeReport{}, err
	}
	defer closeFn()

	log.Infof("[PIPELINE:media-probe] browser=%s url=%s kind=%s reqLang=%s", browserCfg.Executable, target, mt.Kind, reqLang)

	p, err := newPage(b, browserCfg)
	if err != nil {
		return MediaProbeReport{}, err
	}
	defer func() { _ = p.Close() }()

	_, _ = p.EvalOnNewDocument(`
		Object.defineProperty(document, 'hidden', { value: false, writable: false });
		Object.defineProperty(document, 'visibilityState', { value: 'visible', writable: false });
		window.addEventListener('visibilitychange', e => e.stopImmediatePropagation(), true);

		window.__allowPause = false;
		const origPause = HTMLMediaElement.prototype.pause;
		HTMLMediaElement.prototype.pause = function() {
			if (!window.__allowPause) return;
			return origPause.apply(this, arguments);
		};
	`)

	if err := p.Context(ctx).Timeout(45 * time.Second).Navigate(target); err != nil {
		return MediaProbeReport{}, fmt.Errorf("load media %s: %w", link, err)
	}
	_ = p.WaitLoad()
	log.Infof("[PIPELINE:media-probe] media loaded")

	rep := MediaProbeReport{URL: target, Kind: mt.Kind}

	// Consent wall, then surface fit, then a buffered video: the same three
	// steps a session performs before it shares the tab.
	if res, err := dismissConsent(p, log, 25*time.Second); err != nil {
		rep.ConsentPresent = true
		rep.Consent = "not dismissed: " + err.Error()
		log.Warnf("[PIPELINE:media-probe] consent: %v", err)
	} else if res.Present {
		rep.ConsentPresent = false
		rep.Consent = res.describe()
		log.Infof("[PIPELINE:media-probe] consent %s", res.describe())
	} else {
		rep.Consent = "no consent wall"
	}
	if fit, err := fitMediaTab(p, browserCfg, log); err != nil {
		log.Warnf("[PIPELINE:media-probe] fit: %v", err)
	} else {
		rep.Viewport = fmt.Sprintf("%dx%d (screen %dx%d, video %dx%d)", fit.ViewportW, fit.ViewportH, fit.ScreenW, fit.ScreenH, fit.VideoW, fit.VideoH)
		rep.OverlayFree = fit.OverlayFree
		log.Infof("[PIPELINE:media-probe] surface %s", fit.describe())
	}
	if st, err := waitMediaReady(p, log, 30*time.Second); err != nil {
		rep.Verdict = err.Error()
		log.Warnf("[PIPELINE:media-probe] not ready: %v", err)
	} else {
		rep.ReadyState = st.Ready
		log.Infof("[PIPELINE:media-probe] video ready: %s", st.describe())
	}

	rep.RequestedLang = NormalizeAudioLang(reqLang)
	if atRep, atErr := ensureAudioTrack(p, mt.Kind, reqLang, log); atErr != nil {
		rep.AudioTrack = "error: " + atErr.Error()
		log.Warnf("[PIPELINE:media-probe] audio track: %v", atErr)
	} else {
		rep.AudioTrack = atRep.describe()
		rep.SelectedTrack = atRep.SelectedTrackLabel()
		rep.FallbackReason = atRep.FallbackReason
		if atRep.Original != nil || atRep.Available > 1 {
			log.Infof("[PIPELINE:media-probe] audio track %s", atRep.describe())
		}
	}

	// What did YouTube's own player decide? playabilityStatus names the real
	// blocker (bot check, unavailable, region, age) even when the UI hides it.
	if diag, err := p.Timeout(5 * time.Second).Eval(`() => JSON.stringify({yt: typeof window.YT, player: !!window.__tmYT, pending: !!window.__tmYTPending, ytErr: window.__tmYTError || "", iframes: document.querySelectorAll("iframe").length, scripts: Array.from(document.scripts).map(s => s.src).filter(Boolean).slice(0, 4), title: document.title})`); err == nil {
		log.Infof("[PIPELINE:media-probe] player wiring %s", diag.Value.Str())
	}
	if ys, err := p.Timeout(5 * time.Second).Eval(youtubeStateJS()); err == nil {
		rep.YTPlayerState = ys.Value.Str()
		log.Infof("[PIPELINE:media-probe] youtube player state %s", rep.YTPlayerState)
	} else {
		log.Debugf("media probe: youtube player state unavailable: %v", err)
	}

	// What did the media page actually render? YouTube renders inside shadow
	// DOM, so the reader has to walk shadow roots to expose error walls.
	if body, err := p.Timeout(5 * time.Second).Eval(pageTextJS()); err == nil {
		rep.BodyText = body.Value.Str()
		log.Infof("[PIPELINE:media-probe] page text: %q", rep.BodyText)
	}

	if codecs, err := p.Timeout(5 * time.Second).Eval(codecSupportJS()); err == nil {
		m := map[string]bool{}
		_ = json.Unmarshal([]byte(codecs.Value.Str()), &m)
		keys := []string{"h264", "aac", "vp9", "vp9-mp4", "av1"}
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%t", k, m[k]))
		}
		rep.CodecSupport = strings.Join(parts, " ")
		log.Infof("[PIPELINE:media-probe] codec support %s", rep.CodecSupport)
	} else {
		log.Debugf("media probe: codec probe failed: %v", err)
	}

	if problem := pageProblemOn(p); problem != "" {
		rep.PageProblem = problem
		rep.Verdict = "page shows an error: " + problem
		log.Infof("[PIPELINE:media-probe] page problem %s", problem)
	}

	// Start playback the same way playMedia does: press play, then require the
	// position to advance with real (non-black) frames before sampling.
	if _, err := p.Timeout(6 * time.Second).Eval(mediaControlJS("unmute", 0)); err != nil {
		log.Debugf("media probe: unmute failed: %v", err)
	}
	if ver, err := ensurePlayback(p, log, 1.0, 12*time.Second); err != nil {
		rep.Verification = err.Error()
		if rep.PageProblem == "" {
			rep.PageProblem = strings.TrimSpace(ver.Problem)
		}
		log.Warnf("[PIPELINE:media-probe] playback verification failed: %v", err)
	} else {
		rep.Advance = ver.Advance
		log.Infof("[PIPELINE:media-probe] playback verified: %s", ver.describe())
	}
	log.Infof("[PIPELINE:media-probe] play started, sampling %ds", seconds)

	var samples []MediaProbeSample
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return rep, ctx.Err()
		default:
		}
		s, err := readPlaybackSample(p)
		if err != nil {
			log.Debugf("media probe: sample failed: %v", err)
		} else {
			samples = append(samples, s)
			rep.Samples = len(samples)
			rep.ReadyState = s.Ready
			log.Infof("[PIPELINE:media-probe] sample %d cur=%.2f/%.2f paused=%t rgb=(%.0f,%.0f,%.0f) size=%dx%d consent=%t draw=%q elem=%q",
				len(samples), s.Current, s.Duration, s.Paused, s.MeanR, s.MeanG, s.MeanB, s.Width, s.Height, s.Consent, s.DrawErr, s.ElementErr)
			if s.Consent {
				// The wall can mount after playback starts; clear it mid-sample so
				// the verdict reflects the page the meeting would actually see.
				rep.Consent = fmt.Sprintf("wall on screen mid-probe (surface=%q)", s.Surface)
				log.Infof("[PIPELINE:media-probe] consent wall mid-probe; dismissing")
				if res, derr := dismissConsent(p, log, 12*time.Second); derr != nil {
					rep.ConsentPresent = true
					log.Warnf("[PIPELINE:media-probe] consent dismissal: %v", derr)
				} else if res.Dismissed {
					log.Infof("[PIPELINE:media-probe] consent %s", res.describe())
					rep.Consent = res.describe()
				} else {
					rep.ConsentPresent = true
				}
			}
			if s.ElementErr != "" {
				rep.PlayerErr = s.ElementErr
			}
			if s.DrawErr != "" && strings.Contains(s.DrawErr, "no media element") && rep.PageProblem == "" {
				prob := pageProblemOn(p)
				if prob != "" {
					rep.PageProblem = prob
				}
			}
		}
		select {
		case <-ctx.Done():
			return rep, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}

	classifyProbe(&rep, samples)
	log.Infof("[PIPELINE:media-probe] verdict %s", rep.Verdict)
	return rep, nil
}

// classifyProbe reduces the samples into report booleans + a human verdict.
func classifyProbe(rep *MediaProbeReport, samples []MediaProbeSample) {
	if len(samples) == 0 {
		if rep.Verdict == "" {
			rep.Verdict = "no readable samples from the media tab"
		}
		return
	}
	minCur, maxCur := samples[0].Current, samples[0].Current
	var rSum, gSum, bSum float64
	painted := 0
	for _, s := range samples {
		if s.Current < minCur {
			minCur = s.Current
		}
		if s.Current > maxCur {
			maxCur = s.Current
		}
		rSum += s.MeanR
		gSum += s.MeanG
		bSum += s.MeanB
		if s.Painted {
			painted++
		}
		if strings.Contains(s.DrawErr, "no media element") {
			continue
		}
	}
	rep.MeanR = rSum / float64(len(samples))
	rep.MeanG = gSum / float64(len(samples))
	rep.MeanB = bSum / float64(len(samples))
	rep.Painted = painted >= len(samples)/2 && painted > 0
	rep.Position, rep.Duration = samples[len(samples)-1].Current, samples[len(samples)-1].Duration
	rep.Advance = maxCur - minCur
	rep.Progressed = (maxCur-minCur) >= 0.4 || maxCur > 2
	if s := samples[len(samples)-1]; s.Consent {
		rep.ConsentPresent = true
		if rep.Consent == "" {
			rep.Consent = fmt.Sprintf("consent wall on screen (surface=%q)", s.Surface)
		}
	}

	// Green: decode failed but the compositor still pushes frames — the archetypal
	// signature is a channel dominated by green while red/blue stay near zero.
	last2 := samples
	if len(last2) > 2 {
		last2 = last2[len(last2)-2:]
	}
	greenOf := func(s MediaProbeSample) bool {
		return s.MeanG > 120 && s.MeanR < 80 && s.MeanB < 80
	}
	blackOf := func(s MediaProbeSample) bool {
		return s.MeanR < 12 && s.MeanG < 12 && s.MeanB < 12
	}
	green := 0
	black := 0
	for _, s := range last2 {
		if !s.Painted {
			continue
		}
		if greenOf(s) {
			green++
		}
		if blackOf(s) {
			black++
		}
	}
	// Only claim a colour verdict when pixels were actually readable; a
	// cross-origin iframe (YouTube) has no sampler access at all.
	rep.GreenFrames = painted > 0 && green >= 1
	rep.BlackFrames = painted > 0 && !rep.Progressed && black == len(last2)

	switch {
	case rep.ConsentPresent:
		rep.Verdict = "a consent wall is on screen — the share would show the cookie dialog, not the video (" + rep.Consent + ")"
	case rep.PageProblem != "":
		rep.Verdict = "page shows an error: " + rep.PageProblem
	case len(samples) > 0 && strings.Contains(samples[0].DrawErr, "no media element"):
		rep.Verdict = "no media element found on the page"
	case rep.PlayerErr != "" && !rep.Progressed:
		rep.Verdict = "media element error: " + rep.PlayerErr
	case rep.GreenFrames:
		rep.Verdict = "video paints GREEN frames — this build cannot decode the stream (missing codecs)"
	case rep.BlackFrames:
		rep.Verdict = "video paints BLACK frames — decoder/compositor failure"
	case rep.Progressed:
		rep.Verdict = fmt.Sprintf("playing: position moved to %.1fs of %.1fs (mean rgb=%.0f,%.0f,%.0f)",
			rep.Position, rep.Duration, rep.MeanR, rep.MeanG, rep.MeanB)
	default:
		rep.Verdict = fmt.Sprintf("not advancing: frozen at %.1fs after %d samples (draw=%q)", rep.Position, len(samples), samples[len(samples)-1].DrawErr)
	}
}

// mediaTargetFor resolves a user link to the navigable URL + classification.
func mediaTargetFor(link string) (string, MediaTarget, error) {
	t, err := ClassifyMedia(link)
	if err != nil {
		return "", MediaTarget{}, err
	}
	target := t.URL
	if t.Kind == MediaDirect {
		doc, err := PlayerHTML(MediaTarget{URL: t.URL, Kind: t.Kind}, PlayerOptions{TabTitle: "TeleMeet Media", Autoplay: true, Volume: 80})
		if err != nil {
			return "", MediaTarget{}, fmt.Errorf("build player: %w", err)
		}
		target, err = players.playerURL(doc)
		if err != nil {
			return "", MediaTarget{}, fmt.Errorf("serve player: %w", err)
		}
	}
	return target, t, nil
}

// interstitialPage dismisses a consent wall if the probe landed on one.
func pageProblemOn(p *rod.Page) string {
	res, err := p.Timeout(5 * time.Second).Eval(playerProblemJS())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Value.Str())
}
