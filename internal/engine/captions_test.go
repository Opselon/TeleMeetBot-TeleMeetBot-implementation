package engine

// Caption tests: policy lifecycle, per-item overrides across navigation,
// toggle commands reaching the fake player, and honest status projection.

import (
	"strings"
	"testing"

	"context"
)

func TestCaptionPolicyRoundTrip(t *testing.T) {
	s := newQueueSession(t, 2)
	if got := s.CaptionPolicy(); got != CaptionOff {
		t.Fatalf("initial caption policy = %q, want off", got)
	}
	if err := s.SetCaptionPolicy(CaptionPreferred); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := s.CaptionPolicy(); got != CaptionPreferred {
		t.Fatalf("policy = %q", got)
	}
	if err := s.SetCaptionLang("FA"); err != nil {
		t.Fatalf("set lang: %v", err)
	}
	if got := s.CaptionLang(); got != "fa" {
		t.Fatalf("lang = %q, want normalized lowercase", got)
	}
	// Unknown policies and bad language tags degrade instead of poisoning.
	_ = s.SetCaptionPolicy("karaoke")
	if got := s.CaptionPolicy(); got != CaptionOff {
		t.Fatalf("bogus policy = %q", got)
	}
	if err := s.SetCaptionLang("not a lang!!"); err != nil {
		t.Fatalf("set bad lang: %v", err)
	}
	if got := s.CaptionLang(); got != "" {
		t.Fatalf("bad lang not cleared: %q", got)
	}
	st := s.Snapshot()
	if st.CaptionPolicy != CaptionOff || st.CaptionLang != "" {
		t.Fatalf("status = %+v", st.Caption)
	}
}

func TestQueueItemCaptionOverrideAcrossNavigation(t *testing.T) {
	s := newQueueSessionWithPage(t, 4)
	s.SetCaptionPolicy(CaptionPreferred)
	s.SetCaptionLang("fa")
	s.SetSwitchRunForTesting(func(target MediaTarget, stale func() bool) error { return nil })

	// item-2 forces English captions; item-3 forces none.
	if err := s.SetQueueItemCaption("item-2", "en"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.SetQueueItemCaption("item-3", "off"); err != nil {
		t.Fatalf("set: %v", err)
	}

	if err := s.Next(); err != nil { // → item-2
		t.Fatalf("next: %v", err)
	}
	if got := s.CaptionLang(); got != "en" {
		t.Fatalf("override item lang = %q, want en", got)
	}
	if got := s.CaptionPolicy(); got != CaptionPreferred {
		t.Fatalf("override item policy = %q", got)
	}

	if err := s.Next(); err != nil { // → item-3 (off)
		t.Fatalf("next: %v", err)
	}
	if got := s.CaptionPolicy(); got != CaptionOff || s.CaptionLang() != "" {
		t.Fatalf("off item = %q/%q, want off/empty", s.CaptionPolicy(), s.CaptionLang())
	}

	if err := s.Next(); err != nil { // → item-4 (no override)
		t.Fatalf("next: %v", err)
	}
	if got := s.CaptionPolicy(); got != CaptionPreferred || s.CaptionLang() != "fa" {
		t.Fatalf("after override = %q/%q, want session defaults", s.CaptionPolicy(), s.CaptionLang())
	}

	// Unknown items are reported.
	if err := s.SetQueueItemCaption("item-999", "en"); err != ErrQueueItemNotFound {
		t.Fatalf("unknown item = %v", err)
	}
}

func TestToggleCaptionsReachesPlayerAndReadsBack(t *testing.T) {
	s := newQueueSessionWithPage(t, 2)
	var actions []string
	lastCC := ""
	s.SetMediaRunForTesting(func(action string, arg float64) (MediaStatus, error) {
		actions = append(actions, action)
		if strings.HasPrefix(action, "ccOn") || action == "ccOff" {
			lastCC = action
		}
		st := MediaStatus{OK: true, Ready: 4}
		// Simulate a player whose state reflects the last caption command.
		if action == "seekStatus" && lastCC != "" {
			st.CaptionSource = "native"
			st.CaptionEnabled = strings.HasPrefix(lastCC, "ccOn")
			st.CaptionAvailable = true
		}
		return st, nil
	})

	confirmed, note := s.ToggleCaptions(true, "fa")
	if !strings.Contains(note, "player reports") {
		t.Fatalf("enable note = %q", note)
	}
	if !confirmed {
		t.Fatalf("enable not confirmed: %q", note)
	}
	if len(actions) < 2 || !strings.HasPrefix(actions[0], "ccOn") {
		t.Fatalf("actions = %v", actions)
	}

	confirmed, note = s.ToggleCaptions(false, "")
	if !confirmed || !strings.Contains(note, "confirmed") {
		t.Fatalf("disable = %v %q", confirmed, note)
	}
	if actions[len(actions)-2] != "ccOff" {
		t.Fatalf("disable action missing: %v", actions)
	}
}

func TestToggleCaptionsUnverifiedIsHonest(t *testing.T) {
	s := newQueueSessionWithPage(t, 2)
	s.SetMediaRunForTesting(func(action string, arg float64) (MediaStatus, error) {
		// The player never reports captions: any "on" request stays issued.
		return MediaStatus{OK: true, Ready: 4}, nil
	})
	confirmed, note := s.ToggleCaptions(true, "en")
	if confirmed {
		t.Fatalf("unconfirmed enable reported as confirmed: %q", note)
	}
	if !strings.Contains(note, "issued") {
		t.Fatalf("note = %q", note)
	}
}

func TestCaptionDescribeStates(t *testing.T) {
	if got := (captionState{}).Describe(); got != "" {
		t.Fatalf("empty described as %q", got)
	}
	on := captionState{Source: "native", Enabled: true, Lang: "fa"}
	if got := on.Describe(); !strings.Contains(got, "native <track>") || !strings.Contains(got, "ON") || !strings.Contains(got, "fa") {
		t.Fatalf("on describe = %q", got)
	}
	issued := captionState{Source: "youtube", Note: "issued"}
	if got := issued.Describe(); !strings.Contains(got, "issued") || strings.Contains(got, "ON") {
		t.Fatalf("issued describe = %q", got)
	}
	ctl := captionState{Source: "source-controlled"}
	if got := ctl.Describe(); !strings.Contains(got, "source's own player") {
		t.Fatalf("source-controlled describe = %q", got)
	}
}

func TestStartRequestCaptionDefaultsSeeded(t *testing.T) {
	mgr := NewManager(BrowserConfig{}, MediaOptions{}, MeetOptions{}, dummyLogger{})
	s, err := mgr.Start(context.Background(), StartRequest{
		MeetLink:      "https://meet.google.com/abc-defg-hij",
		MediaLink:     "https://example.com/a.mp4",
		CaptionPolicy: "preferred",
		CaptionLang:   "FA",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(s.Stop)
	if got := s.CaptionPolicy(); got != CaptionPreferred {
		t.Fatalf("policy = %q", got)
	}
	if got := s.CaptionLang(); got != "fa" {
		t.Fatalf("lang = %q", got)
	}
}
