package engine

// Out-of-meeting detection.
//
// Production evidence (session s-9e5ee80e): after the bot was removed from
// the meeting, Meet redirected its tab to the marketing landing page. The
// watch loop then burned ~2 minutes re-presenting into a page with no
// meeting UI (three 45 s present-flow timeouts, full toolbar dumps every
// 10 s) before failing with the misleading "screen share was dropped".
//
// The fix: when the share is missing, check whether the Meet tab is still in
// a meeting at all. A landing/ended page fails the session immediately with
// an honest reason; only an actual meeting page earns re-present attempts.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// leftMeetingJS reports whether the Meet tab shows an actual meeting.
// Signals of a real meeting: the URL still carries a meeting code. Signals
// of a landing/ended page: marketing markers ("Open the Sign Up page",
// "Sign in to Google") or a bare meet.google.com URL without a code.
const leftMeetingJS = `() => {
  const url = location.href || "";
  const code = url.match(/meet\.google\.com\/([a-z]{3}-[a-z]{4}-[a-z]{3})/i);
  const marketing = /Open the (Sign Up|Continue) page|Sign in to Google/i.test(document.body ? document.body.innerText.slice(0, 4000) : "");
  const landing = /meet\.google\.com\/?(#|\?|$)/.test(url);
  return JSON.stringify({inMeeting: !!code && !landing, marketing: marketing, landing: landing && !code, url: url.slice(0, 120)});
}`

// leftMeetingState is the parsed probe result.
type leftMeetingState struct {
	InMeeting bool   `json:"inMeeting"`
	Marketing bool   `json:"marketing"`
	Landing   bool   `json:"landing"`
	URL       string `json:"url"`
}

// Reason renders the human explanation for the session card and the log.
func (l *leftMeetingState) Reason() string {
	switch {
	case l.Marketing:
		return "the Meet tab is showing a marketing/landing page — the bot is no longer in the meeting (removed, or the meeting ended)"
	case l.Landing:
		return "the Meet tab left the meeting URL — the meeting ended or the bot was removed"
	default:
		return "the Meet tab no longer shows a meeting"
	}
}

// probeLeftMeeting evaluates the detector on the Meet tab. A probe error is
// reported as "unknown" (nil, false) so a transient CDP hiccup cannot kill a
// live session through this path.
func (s *Session) probeLeftMeeting() (*leftMeetingState, bool) {
	meet := s.meetTab()
	if meet == nil {
		return nil, false
	}
	res, err := meet.Timeout(5 * time.Second).Eval(leftMeetingJS)
	if err != nil {
		return nil, false
	}
	body := strings.TrimSpace(res.Value.Str())
	if !strings.HasPrefix(body, "{") {
		return nil, false
	}
	st := &leftMeetingState{}
	if err := json.Unmarshal([]byte(body), st); err != nil {
		return nil, false
	}
	return st, true
}

// decideMeetingExit maps a probe result to the recovery decision: exit when
// the tab is demonstrably out of the meeting, continue when in a meeting or
// when the probe cannot tell. Pure function so the table test pins it.
func decideMeetingExit(st *leftMeetingState, probeOK bool) error {
	if !probeOK || st == nil {
		return nil
	}
	if st.InMeeting {
		return nil
	}
	return fmt.Errorf("%s (tab: %s)", st.Reason(), st.URL)
}

// checkMeetingPresence runs the out-of-meeting detector when the share is
// missing. It returns a non-nil error when the session must fail fast (the
// tab is demonstrably out of the meeting) — this replaces up to three blind
// re-present rounds and their 45 s timeouts. A nil error means "keep the
// normal recovery path": either the meeting is still there or the probe
// could not tell.
func (s *Session) checkMeetingPresence() error {
	st, ok := s.probeLeftMeeting()
	if err := decideMeetingExit(st, ok); err != nil {
		s.setDetail("left the meeting")
		return err
	}
	return nil
}
