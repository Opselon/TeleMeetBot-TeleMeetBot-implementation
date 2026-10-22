package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// meetStageJS is the page script classifying where the bot is. It must stay in
// function form (no IIFE wrap) for rod's Eval, and it returns [stage, detail] —
// stages: meeting, lobby, blocked, prejoin, login, unknown. For "blocked" the
// detail is the page's own (lowercased) excerpt so the Go side can tell
// terminal blocks from host-gated waits.
//
// Stage classification is body-text based (see references/meet-guest-flow.md).
func meetStageJS() string {
	return `() => {
  const body = (((document.body || {}).innerText) || "").toLowerCase();
  const vis = sel => {
    try {
      const el = document.querySelector(sel);
      if (!el) return false;
      const r = el.getBoundingClientRect();
      if (r.width < 2 || r.height < 2) return false;
      const cs = getComputedStyle(el);
      return cs.visibility !== "hidden" && cs.display !== "none";
    } catch (e) { return false; }
  };
  const snippet = (re) => {
    const m = body.match(re);
    return m ? String(m[0]).slice(0, 120) : "";
  };
  const excerpt = body.replace(/\s+/g, " ").trim().slice(0, 240);
  if (vis('[aria-label="Present now"]') || vis('[data-tooltip="Present now"]') ||
      vis('[aria-label="Stop presenting"]') || vis('[data-tooltip="Stop presenting"]') ||
      /you are presenting|you're presenting|stop presenting/.test(body)) {
    return ["meeting", "meeting controls visible"];
  }
  if (/waiting for (the )?host|you.ll be able to join|someone will let you in|waiting to be admitted|you.ll join when someone lets you in/.test(body)) {
    return ["lobby", "in the lobby, host must admit"];
  }
  if (/meeting is full|you can.t join this (video )?call|this meeting has ended|the call has ended|invalid meeting|check your meeting code|no one can join|ask the host|needs permission/.test(body)) {
    return ["blocked", excerpt];
  }
  if (/join now|ask to join|ready to join/.test(body)) {
    return ["prejoin", "prejoin screen"];
  }
  if (/sign in|use a personal account|join with a google account/.test(body)) {
    return ["login", snippet(/sign in|use a personal account|join with a google account/)];
  }
  return ["unknown", ""];
}`
}

// blockedFatalPhrase reports the terminal-rejection phrase hidden in a
// "blocked" stage detail, or "" when the block is host-gated and worth waiting
// out: Google withholds host-restricted meetings ("no one can join a meeting
// unless invited or admitted by the host") until the first host joins — after
// that the same link goes to the normal prejoin/lobby flow.
func blockedFatalPhrase(detail string) string {
	d := strings.ToLower(detail)
	for _, phrase := range []string{
		"meeting is full",
		"this meeting has ended",
		"the call has ended",
		"invalid meeting",
		"check your meeting code",
	} {
		if strings.Contains(d, phrase) {
			return phrase
		}
	}
	return ""
}

// meetStage inspects the Meet page and classifies where the bot is.
// Stages: meeting, lobby, prejoin, blocked, login, unknown.
func (s *Session) meetStage() (string, string) {
	meet := s.meetTab()
	if meet == nil {
		return "unknown", "no page"
	}
	res, err := meet.Timeout(5 * time.Second).Eval(meetStageJS())
	if err != nil {
		return "unknown", err.Error()
	}
	arr := res.Value.Arr()
	if len(arr) < 2 {
		return "unknown", ""
	}
	return strings.ToLower(arr[0].Str()), strings.TrimSpace(arr[1].Str())
}

// fillGuestName types the configured guest name into the prejoin name field.
func (s *Session) fillGuestName() bool {
	meet := s.meetTab()
	if meet == nil {
		return false
	}
	name, _ := json.Marshal(s.mgr.meetCfg.GuestName)
	js := fmt.Sprintf(`() => {
  const NAME = %s;
  if (!NAME) return false;
  const inputs = Array.from(document.querySelectorAll('input'));
  for (const el of inputs) {
    const r = el.getBoundingClientRect();
    if (r.width < 5 || r.height < 5) continue;
    const ph = ((el.getAttribute('placeholder') || "") + " " + (el.getAttribute('aria-label') || "")).toLowerCase();
    if (!(ph.includes("name") || el.value === "")) continue;
    if (el.value === NAME) return true;
    el.focus();
    el.value = NAME;
    el.dispatchEvent(new Event('input', {bubbles: true}));
    el.dispatchEvent(new Event('change', {bubbles: true}));
    el.blur();
    return true;
  }
  return false;
}`, name)
	res, err := meet.Timeout(4 * time.Second).Eval(js)
	if err != nil {
		return false
	}
	return res.Value.Bool()
}

// clickJoin clicks "Join now" / "Ask to join" on the prejoin screen.
func (s *Session) clickJoin() (bool, string) {
	meet := s.meetTab()
	if meet == nil {
		return false, ""
	}
	res, err := meet.Timeout(4 * time.Second).Eval(`() => {
  const norm = x => (x || "").replace(/\s+/g, " ").trim().toLowerCase();
  const els = Array.from(document.querySelectorAll('button, [role=button], div[role=button]'));
  let best = null, bestScore = 0;
  for (const el of els) {
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
  try { best.scrollIntoView({block: 'center'}); } catch (e) {}
  best.click();
  return norm((best.innerText || best.textContent || "")).slice(0, 40);
}`)
	if err != nil {
		return false, ""
	}
	label := strings.TrimSpace(res.Value.Str())
	return label != "", label
}

// clickPresentButton opens Meet's "Present now" menu.
func (s *Session) clickPresentButton() (bool, string) {
	meet := s.meetTab()
	if meet == nil {
		return false, ""
	}
	res, err := meet.Timeout(4 * time.Second).Eval(`() => {
  const norm = x => (x || "").replace(/\s+/g, " ").trim().toLowerCase();
  const els = Array.from(document.querySelectorAll('[role=button], button, [aria-label]'));
  let best = null;
  for (const el of els) {
    const t = norm((el.getAttribute('aria-label') || "") + " " + (el.innerText || el.textContent || ""));
    if (!t.includes("present")) continue;
    if (t.includes("stop presenting")) continue;
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    best = el;
    break;
  }
  if (!best) return "";
  const label = norm((best.getAttribute('aria-label') || "") + " " + (best.innerText || ""));
  try { best.scrollIntoView({block: 'center'}); } catch (e) {}
  best.click();
  return label.slice(0, 40);
}`)
	if err != nil {
		return false, ""
	}
	label := strings.TrimSpace(res.Value.Str())
	return label != "", label
}

// clickPresentSource picks "A tab" (default) or "Entire screen" in the menu.
func (s *Session) clickPresentSource() (bool, string) {
	meet := s.meetTab()
	if meet == nil {
		return false, ""
	}
	pats := []string{"a tab", "a chrome tab", "chrome tab"}
	if s.mgr.meetCfg.PresentMode == "screen" {
		pats = []string{"entire screen", "your entire screen", "a screen"}
	}
	jsPats, _ := json.Marshal(pats)
	js := fmt.Sprintf(`() => {
  const pats = %s;
  const norm = x => (x || "").replace(/\s+/g, " ").trim().toLowerCase();
  const els = Array.from(document.querySelectorAll('[role=menuitem], [role=button], button, li, div[aria-label]'));
  for (const el of els) {
    const t = norm((el.getAttribute('aria-label') || "") + " " + (el.innerText || el.textContent || ""));
    if (!pats.some(p => t === p || t.startsWith(p))) continue;
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    try { el.scrollIntoView({block: 'center'}); } catch (e) {}
    el.click();
    return t.slice(0, 40);
  }
  return "";
}`, jsPats)
	res, err := meet.Timeout(4 * time.Second).Eval(js)
	if err != nil {
		return false, ""
	}
	label := strings.TrimSpace(res.Value.Str())
	return label != "", label
}

// presenting reports whether Meet shows an active presentation.
func (s *Session) presenting() (bool, error) {
	meet := s.meetTab()
	if meet == nil {
		return false, nil
	}
	res, err := meet.Timeout(4 * time.Second).Eval(`() => {
  try {
    if (document.querySelector('[aria-label="Stop presenting"], [data-tooltip="Stop presenting"]')) return true;
    const t = ((document.body || {}).innerText || "").toLowerCase();
    return /you are presenting|you're presenting|stop presenting/.test(t);
  } catch (e) { return false; }
}`)
	if err != nil {
		return false, err
	}
	return res.Value.Bool(), nil
}

// clickShareConfirm confirms the "Share" button some Meet versions show after a
// capture source has been picked. It is a no-op when the share starts directly.
func (s *Session) clickShareConfirm() (bool, string) {
	meet := s.meetTab()
	if meet == nil {
		return false, ""
	}
	res, err := meet.Timeout(4 * time.Second).Eval(`() => {
  const norm = x => (x || "").replace(/\s+/g, " ").trim().toLowerCase();
  const visible = el => { const r = el.getBoundingClientRect(); return r.width > 2 && r.height > 2; };
  const pick = sel => Array.from(document.querySelectorAll(sel));
  const candidates = [
    ...pick('[role=dialog] button, [role=dialog] [role=button]'),
    ...pick('dialog button'),
    ...pick('button[aria-label="Share"], button[aria-label="share"]'),
  ];
  for (const el of candidates) {
    if (!visible(el)) continue;
    const t = norm((el.getAttribute("aria-label") || "").trim() || (el.innerText || el.textContent || ""));
    if (!(t === "share" || t === "share now" || t === "start sharing")) continue;
    try { el.scrollIntoView({block: 'center'}); } catch (e) {}
    el.click();
    return t.slice(0, 40);
  }
  return "";
}`)
	if err != nil {
		return false, ""
	}
	label := strings.TrimSpace(res.Value.Str())
	return label != "", label
}
