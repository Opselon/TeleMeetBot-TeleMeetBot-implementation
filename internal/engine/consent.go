package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// YouTube/Google consent walls
//
// YouTube serves "Before you continue to YouTube" as a lightbox (a
// `ytd-consent-bump-v2-lightbox` holding a `tp-yt-paper-dialog`) and, in some
// regions, as a whole-page `consent.youtube.com` redirect. Both block the video
// element from ever painting, so the Meet share shows the dialog instead of the
// clip. The dialog's buttons are labelled per locale and their aria-label is a
// full sentence ("Reject the use of cookies and other data for the purposes
// described"), which is why the old matcher — comparing an aria-label+text
// concatenation for exact equality with "reject all" — never fired.
//
// The rules below are structural and locale-driven, never a per-locale
// selector: find the consent surface by DOM markers, then score every visible
// leaf button by its normalised labels against phrase tables in the supported
// languages. "Reject all" is preferred (it dismisses the wall without enabling
// tracking); "accept all" is the fallback when a build ships only that.

// consentRejectPhrases are normalised (lowercase, diacritics stripped, no
// punctuation) dismissal labels that reject consent, across the locales
// YouTube/Google serve this wall in.
var consentRejectPhrases = []string{
	"reject all", "reject all cookies", "reject cookies", "reject", "decline all", "decline",
	"decline cookies", "disagree",
	// de
	"alle ablehnen", "ablehnen", "alle cookies ablehnen",
	// fr
	"tout refuser", "refuser tout", "refuser",
	// es
	"rechazar todo", "rechazar todas", "rechazar",
	// pt
	"rejeitar tudo", "recusar tudo", "rejeitar", "recusar",
	// it
	"rifiuta tutto", "rifiuta",
	// nl
	"alles weigeren", "weigeren", "alles afwijzen",
	// pl
	"odrzuc wszystko", "odrzuc",
	// sv/no/da/fi
	"avvisa alla", "avvisa allt", "avvis alle", "afvis alle", "afvis", "hylkaa kaikki", "hylkaa",
	// ru/uk/bg
	"отклонить все", "отклонить", "отхвърли всички", "відхилити все",
	// tr
	"tumunu reddet", "reddet", "tumunu kabul etme",
	// ar/fa
	"رد الكل", "رفض الكل", "رفض", "رد همه", "رد کردن همه", "عدم پذیرش",
	// zh/ja/ko
	"全部拒绝", "拒绝全部", "全部拒絕", "拒絕全部", "すべて拒否", "全て拒否", "모두 거부", "모두 거부하기",
	// id/ms/uz
	"tolak semua", "tolak", "rad etish", "rad qilish",
}

// consentAcceptPhrases are the accept-side equivalents (fallback only).
var consentAcceptPhrases = []string{
	"accept all", "accept all cookies", "accept cookies", "accept", "i agree", "agree to all",
	"agreed", "allow all", "got it",
	// de/fr/es/pt/it/nl/pl
	"alle akzeptieren", "akzeptieren", "tout accepter", "accepter", "aceptar todo", "aceptar",
	"aceitar tudo", "aceitar", "accetta tutto", "accetta", "alles accepteren", "accepteren",
	"zaakceptuj wszystko", "zaakceptuj",
	// sv/no/da/fi
	"godkann alla", "godkann alle", "accepter alle", "hyvaksy kaikki",
	// ru/uk
	"принять все", "принять", "погодити все",
	// tr
	"tumunu kabul et", "kabul et",
	// ar/fa
	"قبول الكل", "موافق", "پذیرش همه", "قبول همه",
	// zh/ja/ko
	"全部接受", "接受全部", "同意", "すべて同意", "すべて受け入れる", "모두 동의", "모두 수락",
	// id/ms
	"terima semua", "terima", "qabul qilish",
}

// consentBlockedTokens keep the matcher away from controls that live in the same
// dialog but do not dismiss it (settings, language picker, "more options").
var consentBlockedTokens = []string{
	"customi", "more option", "more action", "manage", "setting", "language", "learn more",
	"sign in", "privacy policy", "terms", "feedback", "help", "share", "back", "close", "dismiss",
	"send feedback", "why this ad", "personaliz", "personalis",
}

// consentMarkers are DOM-level surface markers. They are structural (stable
// element names), so they work in every locale; text is only a secondary hint.
var consentMarkers = []string{
	"ytd-consent-bump-v2-lightbox",
	"ytd-consent-bump-v2-renderer",
	"ytd-consent-bump-lightbox",
	"#consent-bump",
	"[consent-bump-v2]",
	"form[action*=\"consent.google\"]",
	"form[action*=\"consent.youtube\"]",
	"c-wiz form[action*=\"consent\"]",
}

// consentIDFallbacks are locale-independent IDs on Google's own consent page.
var consentIDFallbacks = map[string][]string{
	"reject": {"#W0wltc"},
	"accept": {"#L2AGLb"},
}

// consentTextHints is the multilingual "this page is asking for consent" signal
// used only when a dialog is on screen but no marker matched.
const consentTextHintsJS = `/(cookie|consent|before you continue|datenschutz|privatsph|confidentialit|privacidad|riservatezza|gizlilik|maxfiylik|قبل المتابعة|ملفات تعريف الارتباط|کوکی|隐私|隱私|プライバシー|개인정보|конфиденциальн|приватност|çerez|kolačić|cookie-)/i`

// consentPreludeJS is the shared matcher: label normalisation, phrase scoring,
// candidate collection (light DOM + shadow roots, leaf buttons only) and surface
// detection. Every consent script is built as `() => { <prelude> <body> }` so
// rod can call it as a function (an IIFE breaks rod's Eval).
func consentPreludeJS() string {
	reject, _ := json.Marshal(consentRejectPhrases)
	accept, _ := json.Marshal(consentAcceptPhrases)
	blocked, _ := json.Marshal(consentBlockedTokens)
	markers, _ := json.Marshal(consentMarkers)
	ids, _ := json.Marshal(consentIDFallbacks)
	return fmt.Sprintf(`
  const REJECT_RAW = %s, ACCEPT_RAW = %s, BLOCKED_RAW = %s, MARKERS = %s, ID_FALLBACKS = %s, TEXT_HINT = %s;

  // norm folds a label into the form both sides of the comparison use: NFD,
  // drop EVERY combining mark, lowercase, drop punctuation/symbols, collapse
  // spaces. Dropping all marks (not just U+0300-036F) matters for kana: NFD
  // splits べ into へ + U+3099, and a narrower range left "すべて拒否" that
  // could never equal the table's "すべて拒否". The tables are folded with the
  // same function below, so both sides always agree.
  const norm = (s) => {
    let t = String(s == null ? "" : s);
    try { t = t.normalize("NFD").replace(/\p{M}+/gu, ""); } catch (e) {}
    try { t = t.normalize("NFC"); } catch (e) {}
    return t.toLowerCase().replace(/[\p{P}\p{S}]+/gu, " ").replace(/\s+/g, " ").trim();
  };
  const REJECT = REJECT_RAW.map(norm), ACCEPT = ACCEPT_RAW.map(norm), BLOCKED = BLOCKED_RAW.map(norm);
  const blockedToken = (t) => BLOCKED.some(function (w) { return t.indexOf(w) >= 0; });

  // scoreLabels returns the best dismissal candidate for an element's labels.
  // reject outranks accept: both dismiss the wall, only one stops tracking.
  const scoreLabels = (raws) => {
    let best = null;
    const consider = (action, score, phrase, label) => {
      if (score <= 0) return;
      if (!best || score > best.score) best = {action: action, score: score, phrase: phrase, label: label};
    };
    for (const raw of raws) {
      const t = norm(raw);
      if (!t || t.length > 160 || blockedToken(t)) continue;
      for (const p of REJECT) {
        if (t === p) consider("reject", 100, p, t);
        else if (t.indexOf(p) === 0) consider("reject", 92, p, t);
        else if ((" " + t + " ").indexOf(" " + p + " ") >= 0) consider("reject", 84, p, t);
      }
      for (const p of ACCEPT) {
        if (t === p) consider("accept", 60, p, t);
        else if (t.indexOf(p) === 0) consider("accept", 52, p, t);
        else if ((" " + t + " ").indexOf(" " + p + " ") >= 0) consider("accept", 46, p, t);
      }
      if (/^(reject|decline|refuse|deny|disagree|ablehnen|rechazar|rifiuta|weiger|avvisa|refuser|afvis|отклонить)\b/u.test(t)) consider("reject", 70, "", t);
      if (/^(accept|agree|akzeptieren|accepter|aceptar|accetta|accepteren|godkann|принять|kabul)\b/u.test(t)) consider("accept", 40, "", t);
    }
    return best;
  };

  const rectOf = (el) => {
    try {
      const r = el.getBoundingClientRect();
      if (r.width < 4 || r.height < 4) return null;
      const cs = getComputedStyle(el);
      if (cs.visibility === "hidden" || cs.display === "none" || cs.opacity === "0" || cs.pointerEvents === "none") return null;
      if (r.bottom < 0 || r.right < 0 || r.top > innerHeight + 400 || r.left > innerWidth + 400) return null;
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

  const describe = (el, r, best) => {
    const cx = Math.round(r.left + r.width / 2), cy = Math.round(r.top + r.height / 2);
    let hit = false;
    try {
      const top = document.elementFromPoint(Math.max(0, Math.min(innerWidth - 1, cx)), Math.max(0, Math.min(innerHeight - 1, cy)));
      hit = deepHas(top, el) || deepHas(el, top);
    } catch (e) {}
    return {x: cx, y: cy, hit: hit, action: best.action, score: best.score,
            label: best.label, phrase: best.phrase,
            aria: String((el.getAttribute && el.getAttribute("aria-label")) || "").slice(0, 120),
            tag: String(el.tagName || "").toLowerCase()};
  };

  // collectCandidates walks the light DOM plus shadow roots and returns visible
  // leaf buttons that look like a consent decision.
  const collectCandidates = () => {
    const found = [], seen = new Set();
    const push = (el) => {
      if (!el || seen.has(el)) return;
      seen.add(el);
      // Skip containers: Meet's toolbar taught us a wrapper's combined text
      // matches every child label at once.
      try { if (el.querySelector && el.querySelector("button, [role=button]")) return; } catch (e) {}
      const r = rectOf(el);
      if (!r) return;
      const best = scoreLabels([el.getAttribute && el.getAttribute("aria-label"), el.getAttribute && el.getAttribute("data-tooltip"),
                                el.innerText, el.textContent, el.value, el.title]);
      if (!best) return;
      found.push(describe(el, r, best));
      found[found.length - 1]._el = el;
    };
    const walk = (root, depth) => {
      if (!root || depth > 4) return;
      let els = [];
      try { els = Array.from(root.querySelectorAll("button, [role=button], tp-yt-paper-button, input[type=submit], a[role=button]")); } catch (e) { return; }
      for (const el of els) push(el);
      let all = [];
      try { all = Array.from(root.querySelectorAll("*")); } catch (e) {}
      if (all.length > 12000) return;
      for (const n of all) { if (n.shadowRoot) walk(n.shadowRoot, depth + 1); }
    };
    walk(document, 0);
    // Locale-independent IDs on Google's consent page (structural fallback).
    for (const action of ["reject", "accept"]) {
      for (const sel of (ID_FALLBACKS[action] || [])) {
        let el = null;
        try { el = document.querySelector(sel); } catch (e) {}
        if (!el || seen.has(el)) continue;
        const r = rectOf(el);
        if (!r) continue;
        seen.add(el);
        found.push(Object.assign(describe(el, r, {action: action, score: 65, label: "id " + sel, phrase: sel}), {_el: el}));
      }
    }
    found.sort((a, b) => (b.score - a.score) || (a.action === "reject" ? -1 : 1));
    return found;
  };

  // consentSurfaces reports markers that are actually ON SCREEN. A hit on a
  // display:none leftover (YouTube keeps the lightbox node mounted after the wall
  // is resolved) is not a wall, and treating it as one would hold the session
  // hostage to a dialog nobody can see.
  const consentSurfaces = () => {
    const out = [];
    for (const s of MARKERS) {
      let el = null;
      try { el = document.querySelector(s); } catch (e) { continue; }
      if (!el) continue;
      if (!rectOf(el)) {
        // The wrapper may be a zero-height host whose dialog child is visible.
        let child = null;
        try { child = el.querySelector("tp-yt-paper-dialog, [role=dialog], form"); } catch (e) {}
        if (!child || !rectOf(child)) continue;
      }
      out.push(s);
    }
    try {
      const dlg = document.querySelector("tp-yt-paper-dialog");
      if (dlg && rectOf(dlg) && document.querySelector("[aria-label*='cookie' i], [aria-label*='consent' i]")) out.push("paper-dialog");
    } catch (e) {}
    return out;
  };

  // visibleDialogs finds dialog-ish nodes in the light DOM and inside shadow
  // roots: some YouTube builds mount the wall in a polymer shadow tree, where
  // document.querySelector cannot see it.
  const visibleDialogs = () => {
    const out = [];
    const visit = (root, depth) => {
      if (!root || depth > 4) return;
      let els = [];
      try { els = Array.from(root.querySelectorAll("tp-yt-paper-dialog, [role=dialog], [role=alertdialog], ytd-consent-bump-v2-lightbox")); } catch (e) { els = []; }
      for (const el of els) { if (rectOf(el)) out.push(el); }
      let all = [];
      try { all = Array.from(root.querySelectorAll("*")); } catch (e) {}
      if (all.length > 12000) return;
      for (const n of all) { if (n.shadowRoot) visit(n.shadowRoot, depth + 1); }
    };
    visit(document, 0);
    return out;
  };

  const bodyText = () => {
    try { return ((document.body && document.body.innerText) || "").replace(/\s+/g, " ").trim().slice(0, 500); }
    catch (e) { return ""; }
  };

  const onConsentHost = () => {
    const h = String(location.hostname || "").toLowerCase();
    return h === "consent.youtube.com" || h === "consent.google.com" || h.indexOf("consent.") === 0;
  };

  const strip = (c) => { const o = Object.assign({}, c); delete o._el; return o; };
`, string(reject), string(accept), string(blocked), string(markers), string(ids), consentTextHintsJS)
}

// consentProbeJS reports whether a consent wall is on screen, which structural
// marker identified it, and every dismissal candidate with its score.
func consentProbeJS() string {
	return "() => {\n" + consentPreludeJS() + `
  const cands = collectCandidates();
  const marks = consentSurfaces();
  const text = bodyText();
  const hasHint = TEXT_HINT.test(text);
  const dialogs = visibleDialogs();
  const dialogish = dialogs.length > 0;
  const dialogSurface = dialogish ? ("dialog:" + String(dialogs[0].tagName || "dialog").toLowerCase()) : "";
  const present = onConsentHost() || marks.length > 0 || (cands.length > 0 && (dialogish || hasHint));
  return JSON.stringify({
    present: present,
    surface: marks.length ? marks[0] : (onConsentHost() ? "consent-host" : dialogSurface),
    markers: marks,
    url: location.href,
    title: String(document.title || "").slice(0, 120),
    consentHost: onConsentHost(),
    textHint: hasHint,
    buttons: cands.slice(0, 8).map(strip),
    best: cands.length ? strip(cands[0]) : null,
    text: text
  });
}`
}

// consentClickJS clicks the best candidate (optionally forcing an action) and
// reports what it clicked. Kept in the same eval as the scan so the element
// handle can never go stale between probe and click.
func consentClickJS(forceAction string) string {
	forced, _ := json.Marshal(forceAction)
	return "() => {\n" + consentPreludeJS() + fmt.Sprintf(`
  const force = %s;
  const cands = collectCandidates().filter((c) => !force || c.action === force);
  const out = {clicked: false, label: "", action: "", score: 0, x: 0, y: 0, hit: false,
               buttons: cands.slice(0, 8).map(strip), surface: (consentSurfaces()[0] || (onConsentHost() ? "consent-host" : "")),
               elapsed: 0};
  // A marked "tried" button (failed ladder) is skipped on later attempts.
  const skip = Array.isArray(window.__tmConsentTried) ? window.__tmConsentTried : [];
  let target = null;
  for (const c of cands) {
    const key = c.action + ":" + c.label;
    if (skip.indexOf(key) >= 0) continue;
    target = c; break;
  }
  if (!target) return JSON.stringify(out);
  const el = target._el;
  try { el.scrollIntoView({block: "center", inline: "center"}); } catch (e) {}
  try { el.focus({preventScroll: true}); } catch (e) {}
  try {
    const r = el.getBoundingClientRect();
    const opts = {bubbles: true, cancelable: true, composed: true, view: window,
                  clientX: r.left + r.width / 2, clientY: r.top + r.height / 2, button: 0};
    for (const t of ["pointerover", "pointerenter", "pointerdown", "mousedown", "pointerup", "mouseup"]) {
      const Ctor = (t.indexOf("pointer") === 0) ? PointerEvent : MouseEvent;
      el.dispatchEvent(new Ctor(t, opts));
    }
  } catch (e) {}
  try { el.click(); } catch (e) {}
  out.clicked = true;
  out.label = target.label; out.action = target.action; out.score = target.score;
  out.x = target.x; out.y = target.y; out.hit = target.hit;
  return JSON.stringify(out);
}`, string(forced))
}

// ConsentButton is one dismissal control found on the consent surface.
type ConsentButton struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	Phrase string `json:"phrase"`
	ARIA   string `json:"aria"`
	Tag    string `json:"tag"`
	Score  int    `json:"score"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Hit    bool   `json:"hit"`
}

// ConsentState is the page's consent status at one instant.
type ConsentState struct {
	Present     bool            `json:"present"`
	Surface     string          `json:"surface"`
	Markers     []string        `json:"markers"`
	URL         string          `json:"url"`
	Title       string          `json:"title"`
	ConsentHost bool            `json:"consentHost"`
	TextHint    bool            `json:"textHint"`
	Buttons     []ConsentButton `json:"buttons"`
	Best        *ConsentButton  `json:"best"`
	Text        string          `json:"text"`
}

// describe renders a consent state for logs and errors: which surface, which
// buttons, and which page — everything needed to debug a wall from a log line.
func (c ConsentState) describe() string {
	var b []string
	for _, x := range c.Buttons {
		b = append(b, fmt.Sprintf("%s:%q(score=%d,hit=%t)", x.Action, x.Label, x.Score, x.Hit))
	}
	return fmt.Sprintf("surface=%q url=%s consent_host=%t markers=%v buttons=[%s] text=%q",
		c.Surface, c.URL, c.ConsentHost, c.Markers, strings.Join(b, " "), truncateStr(c.Text, 140))
}

// ConsentResult summarises a dismissal run.
type ConsentResult struct {
	Present   bool     `json:"present"`
	Dismissed bool     `json:"dismissed"`
	Action    string   `json:"action"`
	Label     string   `json:"label"`
	Method    string   `json:"method"`
	Attempts  int      `json:"attempts"`
	Surface   string   `json:"surface"`
	Tried     []string `json:"tried"`
}

// describe renders the outcome for logs.
func (r ConsentResult) describe() string {
	if !r.Present {
		return "no consent wall"
	}
	if r.Dismissed {
		return fmt.Sprintf("dismissed via %s (%q, method=%s, attempts=%d)", r.Action, r.Label, r.Method, r.Attempts)
	}
	return fmt.Sprintf("consent wall still up after %d attempts (surface=%q, tried=%v)", r.Attempts, r.Surface, r.Tried)
}

// probeConsent reads the consent state from a page.
func probeConsent(p *rod.Page) (ConsentState, error) {
	res, err := p.Timeout(6 * time.Second).Eval(consentProbeJS())
	if err != nil {
		return ConsentState{}, err
	}
	var st ConsentState
	if err := json.Unmarshal([]byte(res.Value.Str()), &st); err != nil {
		return ConsentState{}, fmt.Errorf("decode consent probe: %w", err)
	}
	return st, nil
}

// consentClickOutcome mirrors consentClickJS.
type consentClickOutcome struct {
	Clicked bool            `json:"clicked"`
	Label   string          `json:"label"`
	Action  string          `json:"action"`
	Score   int             `json:"score"`
	X       int             `json:"x"`
	Y       int             `json:"y"`
	Hit     bool            `json:"hit"`
	Surface string          `json:"surface"`
	Buttons []ConsentButton `json:"buttons"`
}

// dismissConsent clears a Google/YouTube consent wall. It prefers "reject all",
// falls back to "accept all", and escalates each candidate through three click
// methods (JS click, physical CDP mouse click, keyboard Enter) so a button that
// ignores synthetic clicks or sits under an overlay still gets pressed.
//
// It never navigates: the caller owns the tab, and the media tab's identity is
// what Chromium captures for the Meet share.
func dismissConsent(p *rod.Page, log Logger, budget time.Duration) (ConsentResult, error) {
	if log == nil {
		log = noopLogger{}
	}
	if budget <= 0 {
		budget = 25 * time.Second
	}
	deadline := time.Now().Add(budget)
	var res ConsentResult
	var last ConsentState
	var lastErr error
	consecutiveErrs := 0

	for time.Now().Before(deadline) {
		st, err := probeConsent(p)
		if err != nil {
			lastErr = err
			consecutiveErrs++
			if consecutiveErrs >= 5 {
				return res, fmt.Errorf("cannot read the media page while checking for a consent wall: %w", err)
			}
			time.Sleep(300 * time.Millisecond)
			continue
		}
		consecutiveErrs = 0
		last = st
		if !st.Present {
			res.Dismissed = true
			if res.Present {
				log.Infof("[PIPELINE:consent] %s", res.describe())
			} else {
				log.Debugf("[PIPELINE:consent] no consent wall")
			}
			return res, nil
		}
		if !res.Present {
			res.Present = true
			res.Surface = st.Surface
			log.Infof("[PIPELINE:consent] consent wall detected: %s", st.describe())
		}
		if st.Best == nil {
			// Surface found, controls not painted yet (consent.youtube.com
			// loads its buttons after the redirect settles).
			log.Debugf("[PIPELINE:consent] wall has no dismissal control yet (%s)", st.Surface)
			time.Sleep(400 * time.Millisecond)
			continue
		}
		key := st.Best.Action + ":" + st.Best.Label

		// Ladder: JS click → physical mouse click → keyboard Enter.
		steps := []struct {
			method string
			run    func(ConsentButton) bool
		}{
			{"js", func(ConsentButton) bool { return clickConsentJS(p, "", log) }},
			{"mouse", func(b ConsentButton) bool { return clickConsentMouse(p, b, log) }},
			{"keyboard", func(ConsentButton) bool { return clickConsentKey(p, log) }},
		}
		for _, step := range steps {
			res.Attempts++
			if !step.run(*st.Best) {
				continue
			}
			if waitConsentGone(p, 1200*time.Millisecond) {
				res.Dismissed = true
				res.Action = st.Best.Action
				res.Label = st.Best.Label
				res.Method = step.method
				log.Infof("[PIPELINE:consent] %s", res.describe())
				return res, nil
			}
		}
		// Every method failed for this control: remember it, try the other
		// action family (a build may only ship an accept button), then wait for
		// the DOM to change instead of clicking the same dead button forever.
		res.Tried = append(res.Tried, key)
		_, _ = p.Eval(fmt.Sprintf(`() => { window.__tmConsentTried = (window.__tmConsentTried || []).concat([%q]); return 1; }`, key))
		alt := "accept"
		if st.Best.Action == "accept" {
			alt = "reject"
		}
		if clickConsentJS(p, alt, log) {
			res.Attempts++
			if waitConsentGone(p, 1200*time.Millisecond) {
				res.Dismissed = true
				res.Action = alt
				res.Method = "js-alt"
				res.Label = lastLabel(p)
				log.Infof("[PIPELINE:consent] %s", res.describe())
				return res, nil
			}
		}
		log.Warnf("[PIPELINE:consent] could not dismiss %q with js/mouse/keyboard; retrying (%s)", st.Best.Label, st.describe())
		time.Sleep(700 * time.Millisecond)
	}
	if lastErr != nil {
		return res, fmt.Errorf("consent wall still up after %s (%v): %s", budget, lastErr, last.describe())
	}
	return res, fmt.Errorf("YouTube/Google consent dialog did not clear after %s: %s; tried=%v — the wall is blocking the video, so the share would show it instead of the clip", budget, last.describe(), res.Tried)
}

// waitConsentGone reports whether the wall disappeared within d.
func waitConsentGone(p *rod.Page, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		st, err := probeConsent(p)
		if err == nil && !st.Present {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// lastLabel re-reads what is clickable now (diagnostics for the alt path).
func lastLabel(p *rod.Page) string {
	st, err := probeConsent(p)
	if err != nil || st.Best == nil {
		return ""
	}
	return st.Best.Label
}

// clickConsentJS presses the best candidate (or one of the forced action) with
// a synthetic click; returns whether a control was pressed.
func clickConsentJS(p *rod.Page, force string, log Logger) bool {
	res, err := p.Timeout(6 * time.Second).Eval(consentClickJS(force))
	if err != nil {
		log.Debugf("[PIPELINE:consent] js click failed: %v", err)
		return false
	}
	var out consentClickOutcome
	if err := json.Unmarshal([]byte(res.Value.Str()), &out); err != nil {
		log.Debugf("[PIPELINE:consent] decode js click: %v", err)
		return false
	}
	if !out.Clicked {
		return false
	}
	log.Debugf("[PIPELINE:consent] js click %s %q (score=%d)", out.Action, out.Label, out.Score)
	return true
}

// clickConsentMouse presses the control with a real CDP mouse click when the
// button is the topmost element at its centre (an overlay-swallowed button is
// left to the keyboard path).
func clickConsentMouse(p *rod.Page, b ConsentButton, log Logger) bool {
	if !b.Hit || b.X <= 0 || b.Y <= 0 {
		return false
	}
	if err := p.Mouse.MoveTo(proto.Point{X: float64(b.X), Y: float64(b.Y)}); err != nil {
		log.Debugf("[PIPELINE:consent] mouse move: %v", err)
		return false
	}
	if err := p.Mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
		log.Debugf("[PIPELINE:consent] mouse click: %v", err)
		return false
	}
	log.Debugf("[PIPELINE:consent] mouse click at %d,%d", b.X, b.Y)
	return true
}

// clickConsentKey presses Enter on the focused control (covers builds whose
// buttons only react to keyboard activation).
func clickConsentKey(p *rod.Page, log Logger) bool {
	if err := p.Keyboard.Press(input.Enter); err != nil {
		log.Debugf("[PIPELINE:consent] keyboard enter: %v", err)
		return false
	}
	log.Debugf("[PIPELINE:consent] keyboard enter")
	return true
}
