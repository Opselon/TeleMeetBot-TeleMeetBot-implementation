package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// YouTube skippable ads
//
// YouTube inserts skippable pre-roll and mid-roll ads into the watch page's own
// player. They play inside the same #movie_player / <video> the session
// captures, so the Meet share broadcasts the ad until it is dismissed. YouTube
// renders the dismissal as a button that only becomes clickable once the ad has
// played for a few seconds:
//
//   - while it counts down, the element holds class "ytp-ad-skip-button" with
//     the pending label ("Skip ads in 5") and a disabled-looking presentation;
//   - once it becomes available, the class list gains ytp-ad-skip-button-enabled
//     and the label becomes "Skip ads" / "Skip ad" (locale dependent), with an
//     aria-label that is a full sentence ("Skip ads in 5 seconds" style), which
//     is why exact text matching never fires.
//
// The detector below is therefore structural: it walks the light DOM AND the
// player's shadow roots (polymer web components), requires a candidate that is
// a leaf button, visible, inside the player, whose class carries the skip
// marker, whose label is on the skip side, and that is explicitly ENABLED.
// Only then does the click ladder fire (synthetic JS click → real CDP mouse
// click → keyboard Enter), mirroring dismissConsent. Ads without a skippable
// control (bumper ads, non-skippable pre-rolls) are never injected into,
// blocked or touched: the detector simply reports "no usable skip button".
//
// Scope is enforced twice: the caller only invokes this for MediaYouTube tabs,
// and the script itself refuses to scan when no YouTube player marker exists.
//
// Nothing here blocks ads at the network level, hides them with CSS or
// manipulates playback controls: skip ad is YouTube's own dismissal, invoked
// exactly the way a human viewer would.

// adSkipPollInterval is how often the monitor samples the media tab while an ad
// might be playing. 1s keeps the worst case (button enabled but unclicked) at
// one second of extra ad audio, and is still only one short eval per second.
const adSkipPollInterval = 1 * time.Second

// adSkipScanDeadline bounds one scan of the media tab for a skip button: a scan
// covers a probe plus the click ladder, so a slow render cannot wedge it.
const adSkipScanDeadline = 8 * time.Second

// adSkipCooldown bounds repeated clicks on one ad generation: after a click we
// stay quiet for a while so a re-paint of the same button cannot be pressed
// twice (which YouTube reads as "double click" and may reverse).
const adSkipCooldown = 12 * time.Second

// adSkipMaxFailures is how many consecutive eval failures the monitor tolerates
// before it logs once and waits for the next successful sample: a single CDP
// hiccup must never stop a live session's ad watch.
const adSkipMaxFailures = 3

// skipButtonPhrases are normalised ("skip ads" style) labels and aria-label
// fragments. Labels are compared after norm(), which strips punctuation, so
// "Skip ads in 5" folds to the same prefix space as "Skip ads". Plain "skip"
// alone is intentionally absent from the exact tier: some builds render an
// unrelated "Skip" on trailers/cards.
var skipButtonPhrases = []string{
	"skip ads", "skip ad", "skip video ads", "skip video ad",
	// de
	"werbung überspringen", "werbung ueberspringen", "anzeige überspringen", "anzeige ueberspringen",
	// fr
	"passer les publicités", "passer la publicité", "passer les pubs", "passer la pub",
	// es
	"omitir anuncios", "omitir el anuncio", "saltar anuncios", "saltar el anuncio",
	// pt
	"pular anúncios", "pular o anúncio", "ignorar anúncios", "ignorar propaganda",
	// it
	"salta gli annunci", "salta annuncio", "salta la pubblicità",
	// nl
	"advertenties overslaan", "advertentie overslaan", "reclame overslaan",
	// ru
	"пропустить рекламу", "пропустить видеорекламу", "пропустить ролик",
	// tr
	"reklamları atla", "reklamı atla", "reklamlari atla",
	// ar
	"تخطي الإعلانات", "تخطي الإعلان",
	// fa
	"رد کردن تبلیغات", "رد کردن تبلیغ",
	// zh/ja/ko
	"広告をスキップ", "跳过广告", "跳過廣告", "광고 건너뛰기",
	// id/ms/uz — Uzbek YouTube serves the Cyrillic form; the Latin-1
	// transliteration ("o'tkazib yuborish") is not a label the page renders.
	"lewati iklan", "langkau iklan",
	"рекламани ўтказиб юбориш", "реклама ўтказиш",
}

// skipCountdownPhrases label the not-yet-available state ("Skip ads in 5").
// When only these match, the ad is detected but no click happens — the monitor
// keeps polling until the control becomes enabled. Entries must be disjoint
// from skipButtonPhrases: a phrase on both sides would make a counting-down
// button look skippable.
var skipCountdownPhrases = []string{
	"skip ads in", "skip ad in", "werbung überspringen in", "werbung in",
	"passer les publicités dans", "passer dans", "omitir anuncios en", "saltar en",
	"пропустить рекламу через", "reklamlarını atla", "広告まで", "广告还需",
}

// blockedSkipTokens keep the matcher away from controls that share the "skip"
// word but are not the ad dismissal (teaser cards, autoplay countdowns,
// "skip intro"-style buttons on other players, share/close chrome).
var blockedSkipTokens = []string{
	"share", "close", "dismiss", "not interested", "don't recommend", "dont recommend",
	"remind me later", "cancel", "back", "settings", "playback", "autoplay", "trailer",
}

// ytPlayerMarkers identify the watch page's own player: the scan only runs when
// one is present, so a plain media page can never be mistaken for YouTube.
var ytPlayerMarkers = []string{
	"#movie_player", "ytd-player", "#player", "#c4-player", "#shorts-player",
}

// adPreludeJS builds the shared skip-button matcher used by both the probe and
// the click script. It is prepended to a `() => { … }` body the same way
// consentPreludeJS is, so every ad script stays a plain function for rod.
func adPreludeJS() string {
	phrases, _ := json.Marshal(skipButtonPhrases)
	countdown, _ := json.Marshal(skipCountdownPhrases)
	blocked, _ := json.Marshal(blockedSkipTokens)
	markers, _ := json.Marshal(ytPlayerMarkers)
	return fmt.Sprintf(`
  const SKIP_RAW = %s, COUNTDOWN_RAW = %s, BLOCKED_RAW = %s, YT_MARKERS = %s;

  // norm folds a label the same way consentPreludeJS does: NFD, drop every
  // combining mark (bie → へ + U+3099 splits kana), lowercase, drop
  // punctuation/symbols, collapse spaces. The phrase tables are folded with the
  // same function at runtime, so both sides always agree.
  const norm = (s) => {
    let t = String(s == null ? "" : s);
    try { t = t.normalize("NFD").replace(/\p{M}+/gu, ""); } catch (e) {}
    try { t = t.normalize("NFC"); } catch (e) {}
    return t.toLowerCase().replace(/[\p{P}\p{S}]+/gu, " ").replace(/\s+/g, " ").trim();
  };
  const SKIP = SKIP_RAW.map(norm), COUNTDOWN = COUNTDOWN_RAW.map(norm), BLOCKED = BLOCKED_RAW.map(norm);
  const blockedToken = (t) => BLOCKED.some(function (w) { return t.indexOf(w) >= 0; });

  // hasYouTubePlayer is the scope gate: no YouTube player marker, no scan.
  const hasYouTubePlayer = () => {
    for (const s of YT_MARKERS) {
      try { if (document.querySelector(s)) return true; } catch (e) {}
    }
    return false;
  };

  const rectVisible = (el) => {
    try {
      const r = el.getBoundingClientRect();
      if (r.width < 6 || r.height < 6) return null;
      const cs = getComputedStyle(el);
      if (cs.visibility === "hidden" || cs.display === "none" || cs.opacity === "0" || cs.pointerEvents === "none") return null;
      if (r.bottom < 0 || r.right < 0 || r.top > innerHeight + 200 || r.left > innerWidth + 200) return null;
      return r;
    } catch (e) { return null; }
  };

  const deepHas = (host, el) => {
    if (!host || !el) return false;
    if (host === el) return true;
    try { if (host.contains && host.contains(el)) return true; } catch (e) {}
    try { if (host.shadowRoot && (host.shadowRoot === el || host.shadowRoot.contains(el))) return true; } catch (e) {}
    return false;
  };

  // findPlayer returns the player element the candidate must live inside. A
  // stray "Skip …" button outside the player is a different control.
  const findPlayer = () => {
    for (const s of YT_MARKERS) {
      try {
        const el = document.querySelector(s);
        if (el) return el;
      } catch (e) {}
    }
    return null;
  };

  // insidePlayer reports whether a candidate belongs to the YouTube player
  // (light DOM containment or a shadow-root path).
  const insidePlayer = (player, el) => {
    if (!player) return true; // already gated by hasYouTubePlayer
    if (deepHas(player, el)) return true;
    let n = el;
    while (n) {
      if (n === player) return true;
      const host = n.nodeType === 11 ? n.host : null;
      if (host && deepHas(player, host)) return true;
      n = n.parentNode || (n.nodeType === 11 ? n.host : null);
    }
    return false;
  };

  // scoreLabels classifies one candidate's labels: "skip" when the label names
  // the dismissal, "countdown" when it only names the pending state, "" when it
  // is neither. The countdown is checked FIRST: a pending button reads
  // "Skip ads in 5 seconds" — its label starts with a skip phrase, so a skip
  // first pass would classify a not-yet-available button as the dismissal and
  // hand the click ladder a control that cannot be clicked yet.
  const scoreLabels = (raws) => {
    let best = null;
    for (const raw of raws) {
      const t = norm(raw);
      if (!t || t.length > 160) continue;
      if (blockedToken(t)) continue;
      // Pending state first: "skip ads in 5" is a countdown, not a dismissal.
      let isCountdown = false;
      for (const p of COUNTDOWN) {
        if (t.indexOf(p) === 0 || (" " + t + " ").indexOf(" " + p + " ") >= 0) {
          isCountdown = true;
          break;
        }
      }
      if (isCountdown) {
        if (!best || best.kind === "skip") best = {kind: "countdown", phrase: "countdown", label: t};
        continue;
      }
      for (const p of SKIP) {
        if (t === p) { if (!best || best.kind !== "skip") best = {kind: "skip", phrase: p, label: t}; }
        else if (t.indexOf(p) === 0 && p.length >= 6) { if (!best || best.kind !== "skip") best = {kind: "skip", phrase: p, label: t}; }
        else if ((" " + t + " ").indexOf(" " + p + " ") >= 0) { if (!best || best.kind !== "skip") best = {kind: "skip", phrase: p, label: t}; }
      }
    }
    return best;
  };

  // describe turns a candidate element into its serialisable description.
  const describe = (el, r, best) => {
    const cx = Math.round(r.left + r.width / 2), cy = Math.round(r.top + r.height / 2);
    let hit = false;
    try {
      const top = document.elementFromPoint(Math.max(0, Math.min(innerWidth - 1, cx)), Math.max(0, Math.min(innerHeight - 1, cy)));
      hit = deepHas(top, el) || deepHas(el, top);
    } catch (e) {}
    return {kind: best.kind, phrase: best.phrase, label: best.label,
            x: cx, y: cy, hit: hit, enabled: enabledNow(el),
            aria: String((el.getAttribute && el.getAttribute("aria-label")) || "").slice(0, 120),
            tag: String(el.tagName || "").toLowerCase(),
            classes: String((el.className && el.className.baseVal !== undefined ? el.className.baseVal : el.className) || "").slice(0, 160)};
  };

  // enabledNow is the availability contract: a skip button counts as usable
  // only when the page explicitly marks it enabled. Matches, per build:
  //   - classList contains ytp-ad-skip-button-enabled (the web classic shape),
  //   - classList contains ytp-ad-skip-button AND aria-disabled is not "true",
  //   - a <button> that is not [disabled] and not aria-disabled="true".
  // A disabled attribute on a ytp-ad-skip-button is how YouTube greys the
  // control out while it counts down, so the class wins over the button
  // attribute only when the class is the -enabled variant.
  const enabledNow = (el) => {
    try {
      const cl = el.classList;
      const aria = String(el.getAttribute && el.getAttribute("aria-disabled") || "").toLowerCase();
      if (cl && cl.contains("ytp-ad-skip-button-enabled")) return true;
      if (cl && cl.contains("ytp-ad-skip-button")) {
        if (el.disabled) return false;
        return aria !== "true";
      }
      if (String(el.tagName || "").toLowerCase() === "button") {
        if (el.disabled) return false;
        return aria !== "true";
      }
      if (el.getAttribute && el.getAttribute("role") === "button") {
        return aria !== "true";
      }
    } catch (e) {}
    return false;
  };

  // findSkipButton walks the light DOM and every shadow root for the player's
  // skip control and returns the best candidate: "skip" outranks "countdown",
  // enabled outranks not-enabled. The element handle rides along as _el so the
  // caller can click it in the same eval without it going stale.
  const findSkipButton = () => {
    const player = findPlayer();
    const kinds = ["button", "[role=button]", "a[href]", "[tabindex]"];
    const seen = new Set();
    let skip = null, countdown = null, fallback = null;

    const consider = (el) => {
      if (!el || seen.has(el)) return;
      seen.add(el);
      // Skip containers: a wrapper's combined text matches every child label
      // (the Meet toolbar lesson — click the leaf, never the bar).
      try { if (el.querySelector && el.querySelector("button, [role=button]")) return; } catch (e) {}
      const r = rectVisible(el);
      if (!r) return;
      if (!insidePlayer(player, el)) return;
      const best = scoreLabels([el.getAttribute && el.getAttribute("aria-label"),
                                el.getAttribute && el.getAttribute("data-tooltip"),
                                el.getAttribute && el.getAttribute("title"),
                                el.innerText, el.textContent]);
      // A control carrying the structural skip class is a candidate even with
      // no label at all (some builds paint an unlabelled box until the
      // countdown ends); the label-only path would drop it here.
      const cl = el.classList;
      const hasSkipClass = !!(cl && (cl.contains("ytp-ad-skip-button") || cl.contains("ytp-ad-skip-button-enabled")));
      if (!best && !hasSkipClass) return;
      if (best) {
        const dsc = describe(el, r, best);
        if (best.kind === "skip" && hasSkipClass && dsc.enabled) { skip = Object.assign(dsc, {_el: el}); }
        else if (best.kind === "skip" && hasSkipClass) { if (!fallback) fallback = Object.assign(dsc, {_el: el}); }
        else if (best.kind === "countdown" && hasSkipClass) { if (!countdown) countdown = Object.assign(dsc, {_el: el}); }
      } else {
        // Unlabelled but structurally a skip control: classify by availability.
        const dsc = describe(el, r, {kind: "skip", phrase: "class:ytp-ad-skip-button", label: "class:ytp-ad-skip-button"});
        if (dsc.enabled) { if (!skip) skip = Object.assign(dsc, {_el: el}); }
        else if (!countdown) { countdown = Object.assign(dsc, {_el: el}); }
      }
    };

    const walk = (root, depth) => {
      if (!root || depth > 5) return;
      let els = [];
      try {
        const parts = [];
        for (const k of kinds) parts.push(Array.from(root.querySelectorAll(k)));
        els = parts.flat(1);
      } catch (e) { return; }
      for (const el of els) consider(el);
      // Shadows first are unnecessary; a single flat pass over each root plus a
      // descent into its shadow hosts keeps the walk O(nodes) per depth level.
      let all = [];
      try { all = Array.from(root.querySelectorAll("*")); } catch (e) {}
      if (all.length > 12000) return;
      for (const n of all) { if (n.shadowRoot) walk(n.shadowRoot, depth + 1); }
    };
    walk(document, 0);

    // Class-only fallback: some builds paint the control with no accessible
    // label until the countdown ends, and the label-only matcher above skips
    // them. Any element carrying the structural skip class inside the player
    // counts; it is only usable when enabled. The light DOM is not enough —
    // the control can live in a shadow root — so walk the same way the label
    // matcher does.
    if (!skip || (!skip.enabled && !fallback)) {
      const seenC = new Set();
      const classWalk = (root, depth) => {
        if (!root || depth > 5) return;
        let els = [];
        try {
          els = Array.from(root.querySelectorAll(".ytp-ad-skip-button-enabled, .ytp-ad-skip-button"));
        } catch (e) { return; }
        for (const el of els) {
          if (!el || seenC.has(el) || seen.has(el)) continue;
          seenC.add(el);
          const r = rectVisible(el);
          if (!r || !insidePlayer(player, el)) continue;
          const dsc = describe(el, r, {kind: "skip", phrase: "class:ytp-ad-skip-button", label: "class:ytp-ad-skip-button"});
          dsc.enabled = enabledNow(el);
          if (dsc.enabled) { if (!skip) skip = Object.assign(dsc, {_el: el}); }
          else if (!fallback && !countdown) { fallback = Object.assign(dsc, {_el: el}); }
        }
        let all = [];
        try { all = Array.from(root.querySelectorAll("*")); } catch (e) {}
        if (all.length > 12000) return;
        for (const n of all) { if (n.shadowRoot) classWalk(n.shadowRoot, depth + 1); }
      };
      classWalk(document, 0);
    }

    return skip || fallback || countdown || null;
  };
`, string(phrases), string(countdown), string(blocked), string(markers))
}

// adProbeJS reports the ad/skip state without clicking anything: is a YouTube
// player present, is a skippable ad playing, is there a skip button, and is it
// enabled yet. It is what the monitor polls every second.
func adProbeJS() string {
	return "() => {\n" + adPreludeJS() + `
  if (!hasYouTubePlayer()) {
    return JSON.stringify({youtube: false, ad: false, playable: null, clicked: false, reason: "not a YouTube player",
                           url: location.href, selector: "", label: "", phrase: "", enabled: false, x: 0, y: 0, hit: false,
                           tag: "", classes: "", playerState: -1});
  }
  const out = {youtube: true, ad: false, playable: null, clicked: false, reason: "",
               url: location.href, selector: "", label: "", phrase: "", enabled: false,
               x: 0, y: 0, hit: false, tag: "", classes: "", playerState: -1};
  try {
    const mp = document.querySelector("#movie_player");
    if (mp && mp.getPlayerState) out.playerState = mp.getPlayerState();
  } catch (e) {}
  // Ads have their own caption/label surface. An ad is "playing" when the
  // player reports a playing state or the video element shows a pinned
  // position with a duration that behaves like an ad slot. We do NOT need to
  // decide that precisely: the skip button only exists during an ad, so its
  // presence is the ad signal. ads Introduced via .ytp-ad-player-overlay.
  const adOverlay = !!(document.querySelector(".ytp-ad-player-overlay, .ytp-ad-module, [class*='ytp-ad']"));

  const cand = findSkipButton();
  out.ad = adOverlay || !!cand;
  if (!cand) {
    out.reason = adOverlay ? "ad playing: no usable skip button (non-skippable or still counting down)" : "no skip button";
    return JSON.stringify(out);
  }
  out.label = cand.label; out.phrase = cand.phrase; out.enabled = cand.enabled;
  out.x = cand.x; out.y = cand.y; out.hit = cand.hit;
  out.tag = cand.tag; out.classes = cand.classes; out.selector = cand.label;
  out.playable = cand.label;
  if (!cand.enabled) {
    out.reason = "skip button found but not yet available (counting down)";
    return JSON.stringify(out);
  }
  out.reason = "skip button available";
  return JSON.stringify(out);
}`
}

// adSkipClickJS finds the best candidate and clicks it when it is enabled,
// recording what it pressed. The element handle never leaves the eval, so the
// click cannot miss a stale element between probe and press.
func adSkipClickJS() string {
	return "() => {\n" + adPreludeJS() + `
  if (!hasYouTubePlayer()) return JSON.stringify({clicked: false, label: "", reason: "not a YouTube player"});
  const cand = findSkipButton();
  const out = {clicked: false, label: "", phrase: "", reason: "no usable skip button", x: 0, y: 0, hit: false, method: "", el: null};
  if (!cand) return JSON.stringify(out);
  if (!cand.enabled) {
    out.label = cand.label; out.phrase = cand.phrase;
    out.reason = "skip button present but not yet available";
    return JSON.stringify(out);
  }
  const el = cand._el;
  if (!el) return JSON.stringify(out);
  out.x = cand.x; out.y = cand.y; out.hit = cand.hit;
  out.label = cand.label; out.phrase = cand.phrase;

  // Synthetic first: the fastest and least intrusive input. YouTube listens on
  // click + pointer events, so dispatch the full pointer/click chain like
  // consentClickJS does. Treat rejection (an exception from el.click on an
  // "invalid" element) as a signal to escalate to a real CDP mouse click.
  let synthetic = false;
  try {
    const r = el.getBoundingClientRect();
    const opts = {bubbles: true, cancelable: true, composed: true, view: window,
                  clientX: r.left + r.width / 2, clientY: r.top + r.height / 2, button: 0};
    for (const t of ["pointerover", "pointerenter", "pointerdown", "mousedown", "pointerup", "mouseup"]) {
      const Ctor = (t.indexOf("pointer") === 0) ? PointerEvent : MouseEvent;
      el.dispatchEvent(new Ctor(t, opts));
    }
    el.click();
    synthetic = true;
  } catch (e) {
    out.reason = "synthetic click rejected: " + String(e).slice(0, 100);
  }
  out.clicked = synthetic;
  out.method = synthetic ? "js" : "rejected";
  window.__tmAdSkipLast = Date.now();
  // The click target is reported to the caller in/out (x,y) so a rejected
  // synthetic click can be re-pressed with a real CDP mouse event.
  return JSON.stringify(out);
}`
}

// adSkipState is one reading of the media tab's skippable-ad state.
type adSkipState struct {
	YouTube     bool   `json:"youtube"`
	Ad          bool   `json:"ad"`
	Playable    string `json:"playable"`
	Clicked     bool   `json:"clicked"`
	Reason      string `json:"reason"`
	URL         string `json:"url"`
	Label       string `json:"label"`
	Phrase      string `json:"phrase"`
	Enabled     bool   `json:"enabled"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	Hit         bool   `json:"hit"`
	Tag         string `json:"tag"`
	Classes     string `json:"classes"`
	PlayerState int    `json:"playerState"`
}

// describe renders the state for log lines.
func (a adSkipState) describe() string {
	return fmt.Sprintf("youtube=%t ad=%t enabled=%t label=%q phrase=%q reason=%q playerState=%d url=%s",
		a.YouTube, a.Ad, a.Enabled, truncateStr(a.Label, 60), truncateStr(a.Phrase, 40), truncateStr(a.Reason, 90), a.PlayerState, a.URL)
}

// probeAdSkip reads the skip state without clicking.
func probeAdSkip(p *rod.Page) (adSkipState, error) {
	res, err := p.Timeout(6 * time.Second).Eval(adProbeJS())
	if err != nil {
		return adSkipState{}, err
	}
	var st adSkipState
	if err := json.Unmarshal([]byte(res.Value.Str()), &st); err != nil {
		return adSkipState{}, fmt.Errorf("decode ad-skip probe: %w (raw=%.180s)", err, res.Value.Str())
	}
	return st, nil
}

// adSkipClickJS mirrors the click evaluation's contract.
type adSkipClickOutcome struct {
	Clicked bool   `json:"clicked"`
	Label   string `json:"label"`
	Phrase  string `json:"phrase"`
	Reason  string `json:"reason"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Hit     bool   `json:"hit"`
	Method  string `json:"method"`
}

// clickSkipButton presses the best available skip button and escalates through
// the input ladder when the page rejects the synthetic click: a real CDP mouse
// click at the element's centre (consumer-game mouse events hit-test the real
// point, so an overlay-swallowed button is caught by the keyboard step), then
// keyboard Enter. Returns whether the skip button was pressed at all.
func clickSkipButton(p *rod.Page, log Logger) (adSkipClickOutcome, bool) {
	if log == nil {
		log = noopLogger{}
	}
	res, err := p.Timeout(adSkipScanDeadline).Eval(adSkipClickJS())
	if err != nil {
		log.Debugf("[PIPELINE:adskip] click eval failed: %v", err)
		return adSkipClickOutcome{}, false
	}
	var out adSkipClickOutcome
	if err := json.Unmarshal([]byte(res.Value.Str()), &out); err != nil {
		log.Debugf("[PIPELINE:adskip] decode click: %v", err)
		return adSkipClickOutcome{}, false
	}
	if !out.Clicked {
		return out, false
	}
	log.Debugf("[PIPELINE:adskip] synthetic click on %q (%s)", out.Label, out.Phrase)

	// The synthetic click may have been "accepted" by the DOM but rejected by
	// the player (a click handler swallowing it), OR the ad may already be
	// gone: YouTube tears the whole module down on skip, so a re-probe that no
	// longer finds the same enabled button is proof the click worked. Only a
	// button that is STILL there AND still enabled needs escalation.
	if after, err := probeAdSkip(p); err == nil && after.YouTube && after.Ad && after.Enabled && after.Label == out.Label &&
		!(after.X == out.X && after.Y == out.Y && out.Method == "js") {
		log.Debugf("[PIPELINE:adskip] %q still present after the synthetic click; escalating to CDP mouse", out.Label)
		if err := p.Mouse.MoveTo(proto.Point{X: float64(after.X), Y: float64(after.Y)}); err == nil {
			if err := p.Mouse.Click(proto.InputMouseButtonLeft, 1); err == nil {
				out.Method = "mouse"
				log.Debugf("[PIPELINE:adskip] CDP mouse click at %d,%d", after.X, after.Y)
			} else {
				log.Debugf("[PIPELINE:adskip] CDP mouse click: %v", err)
			}
		} else {
			log.Debugf("[PIPELINE:adskip] CDP mouse move: %v", err)
		}
		if err := p.Keyboard.Press(input.Enter); err != nil {
			log.Debugf("[PIPELINE:adskip] keyboard enter: %v", err)
		}
	}
	return out, true
}

// SkipAdResult summarises one scan of the media tab for skippable ads.
type SkipAdResult struct {
	YouTube   bool   `json:"youtube"`
	AdPlaying bool   `json:"adPlaying"`
	Skipped   bool   `json:"skipped"`
	Reason    string `json:"reason"`
	Label     string `json:"label"`
	Method    string `json:"method"`
	ClickedAt int    `json:"clickedAt"`
}

// describe renders the result for log lines.
func (r SkipAdResult) describe() string {
	return fmt.Sprintf("youtube=%t ad=%t skipped=%t label=%q method=%q reason=%q",
		r.YouTube, r.AdPlaying, r.Skipped, truncateStr(r.Label, 60), r.Method, truncateStr(r.Reason, 90))
}

// scanSkipAds looks at the media tab once: on a non-YouTube tab it is a no-op;
// on a YouTube tab without a usable skip button it reports the ad state without
// touching anything; when the official skip button is visible and enabled it
// presses it. It never blocks, hides or seeks — only YouTube's own dismissal.
//
// Shared across the monitor loop, prepareMedia and recoverMedia so every path
// that looks at the media tab shares one definition of "skippable ad".
func scanSkipAds(p *rod.Page, log Logger) (SkipAdResult, error) {
	if log == nil {
		log = noopLogger{}
	}
	var rep SkipAdResult
	st, err := probeAdSkip(p)
	if err != nil {
		return rep, err
	}
	rep.YouTube = st.YouTube
	rep.AdPlaying = st.Ad
	rep.Reason = st.Reason
	rep.Label = st.Label
	if !st.YouTube || !st.Ad || !st.Enabled {
		return rep, nil
	}
	out, clicked := clickSkipButton(p, log)
	if !clicked {
		return rep, nil
	}
	rep.Skipped = true
	rep.Label = out.Label
	rep.Method = out.Method
	rep.ClickedAt = int(time.Now().Unix())
	return rep, nil
}

// MonitorSkipAds polls the media tab while the session is streaming and presses
// YouTube's own skippable-ad button the moment it becomes available — the same
// thing a human viewer does, at the same latency. It covers pre-roll (before
// prepareMedia declares the page ready) and mid-roll (during the watch loop).
//
// What it deliberately does NOT do:
//   - block or remove ads (no request interception, no element removal),
//   - hide the ad surface with CSS (the fit stylesheet stays untouched),
//   - touch playback controls (no pause/play/mute, no seeks),
//   - click anything when no official, visible, enabled skip button exists
//     (non-skippable and bumper ads play out in full).
//
// The loop exits when stop is closed, when the session's own context is done
// (unset only in tests), when the media tab is gone, or after
// adSkipMaxFailures consecutive unreadable samples followed by a final
// unreadable check — one bad eval never stops the watch, and a page that stays
// unreadable is reported with the reason instead of a silent exit.
func (s *Session) MonitorSkipAds(stop <-chan struct{}) {
	log := s.log
	if log == nil {
		log = noopLogger{}
	}
	// This goroutine runs for the whole session lifetime and polls the media
	// tab. A seek that lands at the end of the track forces a queue
	// auto-advance, which navigates that tab while this loop is polling it —
	// a rod eval against a target being torn down can panic deep inside the
	// driver, and with no recover() the whole goroutine would die and take
	// ad-skipping (and, if the panic escapes, the process) with it.
	// Recover, report, and let the next sample continue: failures are already
	// counted and bounded by adSkipMaxFailures below.
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("[PIPELINE:adskip] monitor panicked (recovered, polling stops for session %s): %v", s.ID, r)
		}
	}()
	// A test-built Session has no ctx/done; the loop must still run and stop on
	// the stop channel rather than dereference a nil context.
	ctxDone := func() <-chan struct{} {
		if s.ctx == nil {
			return nil
		}
		return s.ctx.Done()
	}
	log.Debugf("[PIPELINE:adskip] monitor started for session %s", s.ID)
	defer log.Debugf("[PIPELINE:adskip] monitor stopped for session %s", s.ID)

	ticker := time.NewTicker(adSkipPollInterval)
	defer ticker.Stop()
	failures := 0
	lastSkip := time.Time{}

	for {
		select {
		case <-stop:
			return
		default:
		}
		if d := ctxDone(); d != nil {
			select {
			case <-d:
				return
			default:
			}
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		media := s.mediaTab()
		if media == nil {
			return
		}
		rep, err := scanSkipAds(media, log)
		if err != nil {
			failures++
			if failures >= adSkipMaxFailures {
				log.Debugf("[PIPELINE:adskip] media tab unreadable for %d samples: %v", failures, err)
				failures = 0
			}
			continue
		}
		failures = 0
		if !rep.YouTube {
			continue
		}
		if rep.Skipped {
			lastSkip = time.Now()
			log.Infof("[PIPELINE:adskip] session %s: skipped a YouTube ad (%s)", s.ID, rep.describe())
			s.emit(EventAdSkipped, "skipped a YouTube ad")
			continue
		}
		if rep.AdPlaying && time.Since(lastSkip) >= adSkipCooldown {
			// Only log the pending state once per cooldown window so a long
			// non-skippable ad cannot flood the log at 1 Hz.
			log.Debugf("[PIPELINE:adskip] session %s: ad playing, no usable skip button yet (%s)", s.ID, rep.Reason)
			lastSkip = time.Now()
		}
	}
}

// skipAdsOnce is the bounded prepare/recovery path: it scans the media tab for
// up to adSkipScanDeadline and presses any available skip button, then reports.
// Failures are diagnostics, never fatal — an ad that cannot be skipped is not a
// session error. It is a no-op when the operator opted out.
func (s *Session) skipAdsOnce() (SkipAdResult, error) {
	if s.adSkipDisabled() {
		return SkipAdResult{}, nil
	}
	media := s.mediaTab()
	if media == nil {
		return SkipAdResult{}, errors.New("media page is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), adSkipScanDeadline)
	defer cancel()
	done := make(chan skipAdsOnceResult, 1)
	go func() {
		rep, err := scanSkipAds(media, s.log)
		done <- skipAdsOnceResult{rep: rep, err: err}
	}()
	select {
	case r := <-done:
		return r.rep, r.err
	case <-ctx.Done():
		return SkipAdResult{}, errors.New("skip-ad scan timed out")
	}
}

// adSkipDisabled reports the operator's opt-out. A nil config value means the
// feature is on (the default), so this only flips on an explicit false.
func (s *Session) adSkipDisabled() bool {
	if s.adSkipEnabled == nil {
		return false
	}
	return !*s.adSkipEnabled
}

// skipAdsOnceResult carries one bounded scan's outcome.
type skipAdsOnceResult struct {
	rep SkipAdResult
	err error
}
