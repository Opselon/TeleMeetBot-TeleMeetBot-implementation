package engine

import (
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/config"
)

// adSkipScriptShape guards the rod contract for the ad-skip scripts: function
// form, never IIFE-wrapped (an IIFE breaks rod's Eval — the same trap
// TestMeetStageScriptShape guards for meetStageJS), and no unbalanced braces.
func TestAdSkipScriptShape(t *testing.T) {
	for name, js := range map[string]string{
		"adProbeJS":     adProbeJS(),
		"adSkipClickJS": adSkipClickJS(),
	} {
		js := js
		t.Run(name, func(t *testing.T) {
			if !strings.HasPrefix(js, "() => {") {
				t.Fatalf("%s must be function form for rod Eval, got %q", name, js[:40])
			}
			if strings.HasPrefix(js, "(() =>") {
				t.Fatalf("%s must not be IIFE-wrapped", name)
			}
			if strings.Contains(js, "})()") || strings.Contains(js, "})( )") {
				t.Fatalf("%s must not self-invoke", name)
			}
			if open, close_ := strings.Count(js, "{"), strings.Count(js, "}"); open != close_ {
				t.Fatalf("%s has unbalanced braces: %d open vs %d close", name, open, close_)
			}
			// json.Marshal escapes inner quotes; a raw string that ends in "}"
			// plus a "\n}" would be the other rod-breaking shape.
			if strings.HasSuffix(strings.TrimSpace(js), "}}") {
				t.Fatalf("%s ends in a double brace (JS builder bug)", name)
			}
		})
	}
}

// TestAdSkipPreludeIsbalanced guards the builder that prepends the prelude to
// the body: the prelude alone must be brace-balanced too, or every script it
// builds is broken the same way the JS-builder pitfall in the skill describes.
func TestAdSkipPreludeIsBalanced(t *testing.T) {
	js := adPreludeJS()
	if open, close_ := strings.Count(js, "{"), strings.Count(js, "}"); open != close_ {
		t.Fatalf("adPreludeJS has unbalanced braces: %d open vs %d close", open, close_)
	}
	// The prelude defines the matchers the body calls; a missing one makes the
	// built script throw on every page.
	for _, name := range []string{"norm", "hasYouTubePlayer", "findSkipButton", "enabledNow", "scoreLabels", "rectVisible", "insidePlayer", "findPlayer"} {
		if !strings.Contains(js, "const "+name+" ") && !strings.Contains(js, "const "+name+"(") {
			t.Fatalf("adPreludeJS lost the %q helper", name)
		}
	}
	for _, table := range []string{"SKIP_RAW", "COUNTDOWN_RAW", "BLOCKED_RAW", "YT_MARKERS"} {
		if !strings.Contains(js, table) {
			t.Fatalf("adPreludeJS lost the %q table", table)
		}
	}
}

// TestAdSkipPhraseTablesFold wants every skip phrase to survive the norm()
// fold: NFD + strip every combining mark + lowercase + strip punctuation. A
// table entry that normalizes to itself-minus-punctuation is what the browser
// side compares against; a phrase that folds to something else can never match
// a real label — the exact kana bug the consent matcher taught us
// (べ → へ + U+3099, so a narrow mark range broke すべて拒否).
func TestAdSkipPhraseTablesFold(t *testing.T) {
	// fold mirrors the browser-side norm() for the parts that matter here:
	// letters (including non-Latin) are kept, punctuation/symbols collapse to
	// a space, whitespace collapses. Diacritics are stripped.
	fold := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				b.WriteRune(toLower(r))
			case isLetter(r):
				b.WriteRune(toLower(r))
			default:
				b.WriteRune(' ')
			}
		}
		return strings.Join(strings.Fields(b.String()), " ")
	}
	for _, p := range skipButtonPhrases {
		if strings.TrimSpace(p) == "" {
			t.Fatal("skipButtonPhrases has an empty entry")
		}
		if fold(p) == "" {
			t.Fatalf("phrase %q folds to empty under norm()", p)
		}
		// Punctuation in a table entry is a latent mismatch: the real label
		// "Skip ads" folds to "skip ads" but a table entry "Skip, ads" folds
		// to a different string and never matches.
		if strings.ContainsAny(p, ".,;:!'\"()[]{}<>") {
			t.Fatalf("phrase %q contains punctuation that norm() would drop — store it punctuation-free", p)
		}
	}
	for _, p := range skipCountdownPhrases {
		if strings.TrimSpace(p) == "" {
			t.Fatal("skipCountdownPhrases has an empty entry")
		}
		if fold(p) == "" {
			t.Fatalf("countdown phrase %q folds to empty under norm()", p)
		}
	}
	// No phrase may appear on both sides: a countdown phrase that doubles as a
	// skip phrase would make a not-yet-available button look skippable.
	skipSet := map[string]bool{}
	for _, p := range skipButtonPhrases {
		skipSet[p] = true
	}
	for _, p := range skipCountdownPhrases {
		if skipSet[p] {
			t.Fatalf("phrase %q is in both the skip and countdown tables", p)
		}
	}
	// The blocked tokens must never collide with a skip phrase: a shared prefix
	// would make the real skip button look like a decoy.
	for _, b := range blockedSkipTokens {
		for _, p := range skipButtonPhrases {
			if p == b || strings.Contains(p, b) {
				t.Fatalf("blocked token %q collides with skip phrase %q", b, p)
			}
		}
	}
	// English must always be covered: "skip ads" and "skip ad" are the two
	// labels every locale build falls back to.
	hasSkipAds, hasSkipAd := false, false
	for _, p := range skipButtonPhrases {
		if p == "skip ads" {
			hasSkipAds = true
		}
		if p == "skip ad" {
			hasSkipAd = true
		}
	}
	if !hasSkipAds || !hasSkipAd {
		t.Fatal("skipButtonPhrases must cover the English 'skip ads'/'skip ad' labels")
	}
}

func toLower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + 32
	}
	return r
}

func isLetter(r rune) bool {
	// Keep the Unicode letters norm() keeps (Latin-1 supplement, Greek and
	// Cyrillic, Arabic, Hebrew, Devanagari, CJK…). This deliberately does NOT
	// use unicode.IsLetter alone: combining marks must be dropped exactly the
	// way the browser's \p{M} strip drops them.
	switch {
	case r >= 0x00C0 && r <= 0x024F && r != 0x00D7 && r != 0x00F7:
		return true // Latin-1 supplement + Latin Extended (letters, not marks)
	case r >= 0x0370 && r <= 0x03FF:
		return true // Greek
	case r >= 0x0400 && r <= 0x04FF:
		return true // Cyrillic
	case r >= 0x0530 && r <= 0x058F, r >= 0x05D0 && r <= 0x05EA:
		return false // Armenian/Hebrew probes are not needed; keep the table tight
	case r >= 0x0600 && r <= 0x06FF:
		return true // Arabic
	case r >= 0x0900 && r <= 0x097F:
		return true // Devanagari
	case r >= 0x3040 && r <= 0x30FF:
		return true // Hiragana/Katakana
	case r >= 0x4E00 && r <= 0x9FFF:
		return true // CJK
	case r >= 0xAC00 && r <= 0xD7AF:
		return true // Hangul
	}
	return false
}

// TestAdSkipOptOutConfig covers the media.skip_youtube_ads switch end to end:
// unset (the default) and true both enable the monitor, an explicit false
// disables it. The switch rides through FromConfig into the session.
func TestAdSkipOptOutConfig(t *testing.T) {
	off := false
	on := true
	cases := []struct {
		name string
		cfg  *bool
		want bool
	}{
		{"default-on", nil, false},
		{"explicit-on", &on, false},
		{"explicit-off", &off, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{adSkipEnabled: tc.cfg}
			if got := s.adSkipDisabled(); got != tc.want {
				t.Fatalf("adSkipDisabled() = %t, want %t", got, tc.want)
			}
		})
	}

	// The switch must survive the config → engine mapping.
	c := config.Default()
	c.Telegram.Token = "1:abc"
	c.Media.SkipYouTubeAds = &off
	_, m, _ := FromConfig(c)
	if m.SkipYouTubeAds == nil || *m.SkipYouTubeAds != false {
		t.Fatalf("FromConfig dropped the opt-out: %+v", m.SkipYouTubeAds)
	}
	c.Media.SkipYouTubeAds = nil
	_, m2, _ := FromConfig(c)
	if m2.SkipYouTubeAds != nil {
		t.Fatalf("FromConfig must keep an unset switch nil, got %+v", m2.SkipYouTubeAds)
	}
}

// TestAdSkipDisabledSessionMakesScanNoOp: with the feature opted out, the
// prepare/recovery scan must not touch the tab at all.
func TestAdSkipDisabledSessionMakesScanNoOp(t *testing.T) {
	off := false
	s := &Session{adSkipEnabled: &off, log: noopLogger{}}
	rep, err := s.skipAdsOnce()
	if err != nil {
		t.Fatalf("opted-out scan must not fail: %v", err)
	}
	if rep.YouTube || rep.AdPlaying || rep.Skipped {
		t.Fatalf("opted-out scan touched the tab: %s", rep.describe())
	}
}

// TestSkipAdResultDescribe covers the log rendering so a diagnostic line can
// never panic on an empty result.
func TestSkipAdResultDescribe(t *testing.T) {
	if got := (SkipAdResult{}).describe(); !strings.Contains(got, "youtube=false") {
		t.Fatalf("empty describe = %q", got)
	}
	if got := (SkipAdResult{YouTube: true, AdPlaying: true, Skipped: true, Label: "Skip ads", Method: "js"}).describe(); !strings.Contains(got, "skipped=true") {
		t.Fatalf("skipped describe = %q", got)
	}
	if got := (adSkipState{YouTube: true, Ad: true, Enabled: false, Reason: "counting down"}).describe(); !strings.Contains(got, "enabled=false") {
		t.Fatalf("state describe = %q", got)
	}
}

// TestAdSkipMonitorGateChecks documents the invariants the monitor relies on;
// they are enforced structurally in adskip.go and repeated here so a refactor
// that drops one fails loudly.
func TestAdSkipMonitorGateChecks(t *testing.T) {
	if adSkipPollInterval <= 0 {
		t.Fatal("adSkipPollInterval must be positive")
	}
	if adSkipScanDeadline <= 0 {
		t.Fatal("adSkipScanDeadline must be positive")
	}
	if adSkipCooldown < adSkipPollInterval {
		t.Fatal("adSkipCooldown must be at least one poll interval")
	}
	if adSkipMaxFailures < 2 {
		t.Fatal("adSkipMaxFailures must tolerate more than one eval failure")
	}
	// The two input paths the ladder escalates through must both be wired.
	if !strings.Contains(adSkipClickJS(), "window.__tmAdSkipLast") {
		t.Fatal("adSkipClickJS must record its click for the cooldown bookkeeping")
	}
	if !strings.Contains(adSkipClickJS(), "pointerdown") {
		t.Fatal("adSkipClickJS must dispatch the full pointer event chain, not only el.click()")
	}
}

// TestSkipPhrasesCoverYouTubeLocales: every locale the bot meets (the consent
// wall's locale list) has at least one skip label. A locale without a phrase is
// a locale where the ad plays out to the audience.
func TestSkipPhrasesCoverYouTubeLocales(t *testing.T) {
	// One representative phrase per locale YouTube serves ads in. The browser
	// side folds labels with the same norm(), so these are prefix/substring
	// matches against the real label sentences.
	required := map[string]string{
		"en": "skip ads", "de": "werbung", "fr": "publicités", "es": "anuncios",
		"pt": "anúncios", "it": "annunci", "nl": "advertenties", "ru": "рекламу",
		"tr": "reklam", "ar": "الإعلانات", "fa": "تبلیغ", "ja": "広告", "ko": "광고",
		"zh": "广告", "id": "iklan",
	}
	for locale, want := range required {
		found := false
		for _, p := range skipButtonPhrases {
			if strings.Contains(p, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("locale %q has no skip phrase covering %q", locale, want)
		}
	}
}
