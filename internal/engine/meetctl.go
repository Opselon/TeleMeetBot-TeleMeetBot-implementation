package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/proto"
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
      const els = document.querySelectorAll(sel);
      for (const el of els) {
        const r = el.getBoundingClientRect();
        if (r.width >= 2 && r.height >= 2) {
          const cs = getComputedStyle(el);
          if (cs.visibility !== "hidden" && cs.display !== "none") return true;
        }
      }
      return false;
    } catch (e) { return false; }
  };
  const snippet = (re) => {
    const m = body.match(re);
    return m ? String(m[0]).slice(0, 120) : "";
  };
  const excerpt = body.replace(/\s+/g, " ").trim().slice(0, 240);

  // In-meeting controls that only exist once the call is active:
  const inMeeting = vis('[aria-label*="Leave call" i]') ||
                    vis('[data-tooltip*="Leave call" i]') ||
                    vis('[aria-label*="Present" i]') ||
                    vis('[data-tooltip*="Present" i]') ||
                    vis('[aria-label*="Share screen" i]') ||
                    vis('[aria-label*="Share your screen" i]') ||
                    vis('[aria-label*="Stop presenting" i]') ||
                    vis('[data-tooltip*="Stop presenting" i]') ||
                    vis('[aria-label*="Stop sharing" i]') ||
                    vis('[data-tooltip*="Stop sharing" i]') ||
                    vis('[aria-label*="Raise hand" i]') ||
                    vis('[aria-label*="People" i]') ||
                    vis('[aria-label*="Chat with everyone" i]') ||
                    vis('[aria-label*="Meeting details" i]') ||
                    /you are presenting|you're presenting|stop presenting|you are sharing|you're sharing|stop sharing/.test(body);

  if (inMeeting) {
    return ["meeting", "meeting controls visible"];
  }
  if (/waiting for (the )?host|you.ll be able to join|someone will let you in|waiting to be admitted|you.ll join when someone lets you in|asking to join/.test(body)) {
    return ["lobby", "in the lobby, host must admit"];
  }
  if (/meeting is full|you can.t join this (video )?call|this meeting has ended|the call has ended|invalid meeting|check your meeting code|no one can join|ask the host|needs permission/.test(body)) {
    return ["blocked", excerpt];
  }
  if (/join now|ask to join|ready to join/.test(body) || vis('input[placeholder*="name" i]') || vis('input[aria-label*="name" i]')) {
    return ["prejoin", "prejoin screen"];
  }
  if (/sign in|use a personal account|join with a google account/.test(body)) {
    return ["login", snippet(/sign in|use a personal account|join with a google account/)];
  }
  return ["unknown", excerpt.slice(0, 80)];
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
    const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
    if (setter) {
      setter.call(el, NAME);
    } else {
      el.value = NAME;
    }
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
    if (el.disabled || el.getAttribute('aria-disabled') === 'true') continue;
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

// mutePrejoinDevices turns off camera and microphone on the prejoin screen,
// ensuring the bot does not join Google Meet with an active green fake-camera.
func (s *Session) mutePrejoinDevices() {
	meet := s.meetTab()
	if meet == nil {
		return
	}
	res, err := meet.Timeout(4 * time.Second).Eval(`() => {
  const norm = x => (x || "").toLowerCase().trim();
  const btns = Array.from(document.querySelectorAll('button, [role=button], div[role=button]'));
  let camOff = false;
  let micOff = false;
  for (const b of btns) {
    const label = norm((b.getAttribute('aria-label') || "") + " " + (b.getAttribute('data-tooltip') || ""));
    if (!camOff && (label.includes("turn off camera") || label.includes("turn off video"))) {
      try { b.click(); camOff = true; } catch (e) {}
    }
    if (!micOff && (label.includes("turn off microphone") || label.includes("turn off mic"))) {
      try { b.click(); micOff = true; } catch (e) {}
    }
  }
  // Keyboard shortcut fallback: Ctrl+E (camera), Ctrl+D (mic)
  if (!camOff) {
    window.dispatchEvent(new KeyboardEvent('keydown', {key: 'e', code: 'KeyE', keyCode: 69, which: 69, ctrlKey: true, bubbles: true}));
    window.dispatchEvent(new KeyboardEvent('keyup', {key: 'e', code: 'KeyE', keyCode: 69, which: 69, ctrlKey: true, bubbles: true}));
  }
  return JSON.stringify({cam: camOff, mic: micOff});
}`)
	if err != nil {
		s.log.Debugf("session %s: mute prejoin devices: %v", s.ID, err)
	} else {
		s.log.Infof("session %s: muted prejoin devices: %s", s.ID, res.Value.Str())
	}
}

// turnOffMeetingCamera turns off the camera inside the meeting call.
func (s *Session) turnOffMeetingCamera() {
	meet := s.meetTab()
	if meet == nil {
		return
	}
	_, _ = meet.Timeout(3 * time.Second).Eval(`() => {
  const norm = x => (x || "").toLowerCase().trim();
  const btns = Array.from(document.querySelectorAll('button, [role=button]'));
  for (const b of btns) {
    const label = norm((b.getAttribute('aria-label') || "") + " " + (b.getAttribute('data-tooltip') || ""));
    if (label.includes("turn off camera") || label.includes("turn off video")) {
      try { b.click(); return true; } catch (e) {}
    }
  }
  return false;
}`)
}

// clickPresentButton opens Meet's "Present now" menu.
func (s *Session) clickPresentButton() (bool, string) {
	meet := s.meetTab()
	if meet == nil {
		return false, ""
	}
	res, err := meet.Timeout(4 * time.Second).Eval(`() => {
  const norm = x => (x || "").replace(/\s+/g, " ").trim().toLowerCase();
  // Target leaf button elements only; never match large toolbar containers.
  const els = Array.from(document.querySelectorAll('button, [role=button]'));
  let best = null;
  let bestScore = 0;
  for (const el of els) {
    if (el.querySelector('button, [role=button]')) continue;

    const aria = norm(el.getAttribute('aria-label') || "");
    const tooltip = norm(el.getAttribute('data-tooltip') || "");
    const text = norm(el.innerText || el.textContent || "");

    // Exclude unrelated controls (audio settings arrow, mic, camera, leave call)
    if (aria.includes("audio") || aria.includes("microphone") || aria.includes("video") ||
        aria.includes("camera") || aria.includes("leave") || aria.includes("settings") ||
        tooltip.includes("audio") || tooltip.includes("microphone") || tooltip.includes("camera")) {
      continue;
    }
    if (aria.includes("stop presenting") || aria.includes("stop sharing") || tooltip.includes("stop") ||
        aria.includes("is starting") || text.includes("is starting") || tooltip.includes("is starting")) {
      continue;
    }

    let score = 0;
    if (aria.includes("present now") || tooltip.includes("present now")) score = 100;
    else if (aria.includes("share screen") || tooltip.includes("share screen")) score = 90;
    else if (aria.includes("share your screen") || tooltip.includes("share your screen")) score = 90;
    else if (aria.includes("present") || tooltip.includes("present")) score = 80;
    else if (text === "present now" || text === "share screen") score = 75;
    else if (text.includes("present now") || text.includes("share screen")) score = 70;
    else if (text.includes("present") && text.length < 30) score = 60;

    if (score <= 0) continue;

    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;

    if (score > bestScore) {
      bestScore = score;
      best = el;
    }
  }
  if (!best) return "";
  const label = norm((best.getAttribute('aria-label') || "") + " " + (best.innerText || ""));
  try { best.scrollIntoView({block: 'center'}); } catch (e) {}
  best.click();
  return (label || "present").slice(0, 40);
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
	pats := []string{"a tab", "a chrome tab", "chrome tab", "tab"}
	if s.mgr.meetCfg.PresentMode == "screen" {
		pats = []string{"entire screen", "your entire screen", "a screen"}
	}
	jsPats, _ := json.Marshal(pats)
	js := fmt.Sprintf(`() => {
  const pats = %s;
  const norm = x => (x || "").replace(/\s+/g, " ").trim().toLowerCase();
  const els = Array.from(document.querySelectorAll('[role=menuitem], [role=button], button, li, div[aria-label]'));
  let best = null;
  let bestText = "";
  for (const el of els) {
    if (el.querySelector('[role=menuitem]')) continue;
    const t = norm((el.getAttribute('aria-label') || "") + " " + (el.innerText || el.textContent || ""));
    if (!pats.some(p => t === p || t.startsWith(p) || t.includes(p))) continue;
    // "presentation is starting..." status text must not be clicked.
    if (t.includes("is starting") || t.includes("starting")) continue;
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    // Prefer real menu items over generic divs.
    const role = (el.getAttribute('role') || '').toLowerCase();
    const isItem = role === 'menuitem' || el.tagName === 'BUTTON' || el.tagName === 'LI';
    if (!best || (isItem && !(best.getAttribute('role') || '').toLowerCase() === 'menuitem')) {
      best = el;
      bestText = t;
    }
  }
  if (!best) return "";
  try { best.scrollIntoView({block: 'center'}); } catch (e) {}
  best.click();
  return bestText.slice(0, 40);
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
    if (document.querySelector('[aria-label*="Stop presenting" i], [data-tooltip*="Stop presenting" i], [aria-label*="Stop sharing" i], [data-tooltip*="Stop sharing" i]')) return true;
    const t = ((document.body || {}).innerText || "").toLowerCase();
    return /you are presenting|you're presenting|stop presenting|you are sharing|you're sharing|stop sharing/.test(t);
  } catch (e) { return false; }
}`)
	if err != nil {
		return false, err
	}
	return res.Value.Bool(), nil
}

// captureTabTitles lists the page titles Chromium currently knows about. This is
// the browser-side (WebContents) title, which is what
// --auto-select-tab-capture-source-by-title matches against.
func (s *Session) captureTabTitles() []string {
	if s.browser == nil {
		return nil
	}
	infos, err := proto.TargetGetTargets{}.Call(s.browser)
	if err != nil {
		return nil
	}
	var titles []string
	for _, ti := range infos.TargetInfos {
		if ti.Type != proto.TargetTargetInfoTypePage {
			continue
		}
		titles = append(titles, ti.Title)
	}
	return titles
}

// ensureCaptureTitle makes sure Chromium sees a tab named `title` — the
// precondition for the auto-select capture matcher to answer getDisplayMedia
// without a picker. It re-applies the title lock on the media tab until the
// browser-side title shows up (or the attempt budget runs out).
func (s *Session) ensureCaptureTitle(title string) error {
	media := s.mediaTab()
	if media == nil {
		return errors.New("media tab is gone")
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		titles := s.captureTabTitles()
		for _, t := range titles {
			if t == title {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("chromium has no tab titled %q (open tabs: %v)", title, titles)
		}
		if _, err := media.Eval(titleLockJS(title)); err != nil {
			s.log.Debugf("session %s: re-lock capture title: %v", s.ID, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// presentControlState returns the visible present/share control's label. Some
// Meet builds open a source menu ("A tab" / "A window" / "Entire screen"),
// others call getDisplayMedia straight from the toolbar button and show
// "Presentation is starting..." until the capture stream is live — the label
// tells those two apart.
func (s *Session) presentControlState() string {
	meet := s.meetTab()
	if meet == nil {
		return ""
	}
	res, err := meet.Timeout(4 * time.Second).Eval(`() => {
  const norm = x => (x || "").replace(/\s+/g, " ").trim().toLowerCase();
  for (const el of document.querySelectorAll("button, [role=button]")) {
    if (el.querySelector("button, [role=button]")) continue;
    const aria = norm(el.getAttribute("aria-label") || "");
    const tip = norm(el.getAttribute("data-tooltip") || "");
    const label = aria + " " + tip;
    if (!label.includes("present") && !label.includes("share")) continue;
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    const cs = getComputedStyle(el);
    if (cs.visibility === "hidden" || cs.display === "none") continue;
    return norm(label).slice(0, 80);
  }
  return "";
}`)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Value.Str())
}

// dumpPresentMenu logs the visible present-menu DOM so the exact labels of
// Meet's picker (which vary between Meet versions) can be diagnosed from logs.
func (s *Session) dumpPresentMenu() {
	meet := s.meetTab()
	if meet == nil {
		return
	}
	res, err := meet.Timeout(4 * time.Second).Eval(`() => {
  const norm = x => (x || "").replace(/\s+/g, " ").trim();
  const out = [];
  const sel = '[role=menuitem], [role=menuitemradio], [role=button], button, li, [aria-label]';
  for (const el of document.querySelectorAll(sel)) {
    const r = el.getBoundingClientRect();
    if (r.width < 2 || r.height < 2) continue;
    const aria = norm(el.getAttribute('aria-label') || "");
    if (!aria && !el.innerText) continue;
    const t = norm(el.innerText || el.textContent || "");
    if (t.length > 80) continue;
    out.push((el.tagName + "[" + (el.getAttribute('role') || "") + "]") +
      " aria=" + JSON.stringify(aria) + " text=" + JSON.stringify(t));
  }
  return out.slice(0, 60).join("\n");
}`)
	if err != nil {
		s.log.Warnf("[PIPELINE:present] session %s: menu dump failed: %v", s.ID, err)
		return
	}
	s.log.Warnf("[PIPELINE:present] session %s: visible present-menu elements:\n%s", s.ID, res.Value.Str())
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
