package bot

// Bot-level caption tests: the settings picker, the session 💬 panel,
// enable/disable with read-back honesty, per-item overrides and authorization.

import (
	"context"

	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/store"
)

func TestCaptionSettingsPicker(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.message(42, "/settings")
	h.callback(42, "set:captions")
	if got := h.api.lastSent().text; !strings.Contains(got, "Subtitles &amp; captions") {
		t.Fatalf("picker not shown: %q", got)
	}

	// Policy + language persist and re-render with the saved values.
	h.callback(42, "set:cap:pol:preferred")
	h.callback(42, "set:cap:lang:fa")
	if u := h.bot.userSettings(42); u.CaptionPolicy != store.CaptionPreferred || u.CaptionLang != "fa" {
		t.Fatalf("saved = %+v", u)
	}

	// Launch forwards the saved caption defaults to the engine.
	runWizardToReview(t, h, 42)
	h.callback(42, cbLaunch)
	if len(h.eng.requests) != 1 {
		t.Fatalf("launch requests = %d", len(h.eng.requests))
	}
	req := h.eng.requests[0]
	if req.CaptionPolicy != store.CaptionPreferred || req.CaptionLang != "fa" {
		t.Fatalf("launch caption = %q/%q", req.CaptionPolicy, req.CaptionLang)
	}
}

func TestCaptionSettingsLanguageCleared(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	h.callback(42, "set:cap:lang:fa")
	h.callback(42, "set:cap:lang:none")
	if u := h.bot.userSettings(42); u.CaptionLang != "" {
		t.Fatalf("lang not cleared: %q", u.CaptionLang)
	}
	// A malformed tag is rejected without touching storage.
	h.callback(42, "set:cap:lang:not a lang!!")
	if got := h.api.answers[len(h.api.answers)-1]; !strings.Contains(got, "bad language") {
		t.Fatalf("malformed tag answer = %q", got)
	}
	if u := h.bot.userSettings(42); u.CaptionLang != "" {
		t.Fatalf("malformed tag leaked: %q", u.CaptionLang)
	}
}

func TestSessionCaptionsMenuAndPolicy(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	id := string(s.ID)

	h.callback(42, "act:"+id+":captions")
	got := h.api.lastEdit().text
	if !strings.Contains(got, "Requested") {
		t.Fatalf("menu missing requested line: %q", got)
	}
	if !strings.Contains(got, "adaptive quality") && !strings.Contains(got, "captions module") {
		t.Fatalf("menu missing the path description: %q", got)
	}

	h.callback(42, "act:"+id+":cappol:preferred")
	if got := s.CaptionPolicy(); got != store.CaptionPreferred {
		t.Fatalf("policy = %q", got)
	}
	h.callback(42, "act:"+id+":caplang:en")
	if got := s.CaptionLang(); got != "en" {
		t.Fatalf("lang = %q", got)
	}
}

func TestSessionCaptionsEnableDisableHonesty(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	id := string(s.ID)

	// The fake player never reports captions: enable must be honest.
	h.callback(42, "act:"+id+":capon")
	if got := h.api.answers[len(h.api.answers)-1]; !strings.Contains(got, "issued") {
		t.Fatalf("unconfirmed enable = %q", got)
	}
}

func TestCaptionInfoViewDistinctions(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)
	_ = s.SetCaptionPolicy(store.CaptionPreferred)
	_ = s.SetCaptionLang("fa")

	h.callback(42, "act:"+string(s.ID)+":capinfo")
	got := h.api.lastEdit().text
	for _, want := range []string{
		"Requested",
		"Available",
		"Selected",
		"Enabled",
		"player itself reports",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("info view missing %q: %q", want, got)
		}
	}
}

func TestCaptionTracksProbeWithoutManager(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)

	// No media manager: the probe says the probe needs the extractor, and
	// direct-player controls stay available.
	h.callback(42, "act:"+string(s.ID)+":capdetect")
	if got := h.api.lastEdit().text; !strings.Contains(got, "Download &amp; Stream (yt-dlp)") {
		t.Fatalf("no-manager probe answer = %q", got)
	}
}

func TestCaptionTracksProbeWithManager(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	harnessWithMediaManager(t, h)
	s := h.addSessionWithQueue(42, 2)
	s.MediaURL = "https://www.youtube.com/watch?v=abcdefghijk"

	// Extend the mock's JSON with subtitle tracks.
	probe := `{"title":"Cap Video","duration":30,"formats":[{"format_id":"f18","height":360,"vcodec":"avc1","acodec":"mp4a","ext":"mp4"}],
	"subtitles": {"en": [{"ext":"vtt","url":"https://x/en.vtt"}]},
	"automatic_captions": {"de": [{"ext":"vtt","url":"https://x/de.vtt"}]}}`
	if mgr := h.bot.mediaMgr(); mgr != nil {
		mgr.SetYTDLPFnForTest(func(ctx context.Context, args []string) ([]byte, []byte, error) {
			for _, a := range args {
				if a == "-J" {
					return []byte(probe), nil, nil
				}
			}
			return []byte("f18,640,360,0,avc1,mp4a,mp4,600\n"), nil, nil
		})
	}

	h.callback(42, "act:"+string(s.ID)+":capdetect")
	got := h.api.lastEdit().text
	for _, want := range []string{"Available subtitles", "1 manual", "1 automatic", "auto-generated", "en", "de"} {
		if !strings.Contains(got, want) {
			t.Fatalf("tracks view missing %q: %q", want, got)
		}
	}

	// Selecting a detected language is a real session change.
	h.callback(42, "act:"+string(s.ID)+":caplang:de")
	if got := s.CaptionLang(); got != "de" {
		t.Fatalf("lang = %q", got)
	}
}

func TestPerItemCaptionOverride(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 3)
	id := string(s.ID)

	h.callback(42, "q:itemcap:"+id)
	if got := h.api.lastEdit().text; !strings.Contains(got, "Per-item subtitles") {
		t.Fatalf("item menu not shown: %q", got)
	}

	h.callback(42, "q:itemcapset:"+id+":item-2:en")
	if got := s.QueueItemCaption("item-2"); got != "en" {
		t.Fatalf("item caption = %q", got)
	}
	h.callback(42, "q:itemcap:"+id)
	if got := h.api.lastEdit().text; !strings.Contains(got, "💬") {
		t.Fatalf("override not marked: %q", got)
	}

	// "off" forces none for the item.
	h.callback(42, "q:itemcapset:"+id+":item-3:off")
	if got := s.QueueItemCaption("item-3"); got != "off" {
		t.Fatalf("off override = %q", got)
	}

	// Stale items are answered honestly.
	h.callback(42, "q:itemcapset:"+id+":item-999:en")
	if got := h.api.answers[len(h.api.answers)-1]; !strings.Contains(got, "not found") {
		t.Fatalf("stale item answer = %q", got)
	}
}

func TestCaptionsUnauthorized(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	s := h.addSessionWithQueue(42, 2)

	h.callback(43, "act:"+string(s.ID)+":captions")
	if got := h.api.allSentText(); !strings.Contains(got, "Access denied") {
		t.Fatalf("unauthorized captions callback not denied: %q", got)
	}
}
