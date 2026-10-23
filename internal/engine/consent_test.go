package engine

import (
	"strings"
	"testing"
	"unicode"
)

// consentLocaleFixtures are the walls a real browser will show, one per YouTube
// UI language the bot is likely to meet. The integration test renders these in
// Chromium; the unit test below guards the phrase tables they depend on.
var consentLocaleFixtures = []consentFixture{
	{Name: "en", Locale: "en", Reject: "Reject all", Accept: "Accept all", Aria: true, Decoys: true},
	{Name: "de", Locale: "de", Reject: "Alle ablehnen", Accept: "Alle akzeptieren", Aria: true},
	{Name: "fr", Locale: "fr", Reject: "Tout refuser", Accept: "Tout accepter", Aria: true},
	{Name: "es", Locale: "es", Reject: "Rechazar todo", Accept: "Aceptar todo", Aria: true},
	{Name: "pt", Locale: "pt", Reject: "Rejeitar tudo", Accept: "Aceitar tudo", Aria: true},
	{Name: "it", Locale: "it", Reject: "Rifiuta tutto", Accept: "Accetta tutto", Aria: true},
	{Name: "ru", Locale: "ru", Reject: "Отклонить все", Accept: "Принять все", Aria: true},
	{Name: "tr", Locale: "tr", Reject: "Tümünü reddet", Accept: "Tümünü kabul et", Aria: true},
	{Name: "fa", Locale: "fa", Reject: "رد همه", Accept: "پذیرش همه", Aria: true},
	{Name: "zh", Locale: "zh", Reject: "全部拒绝", Accept: "全部接受", Aria: true},
	{Name: "ja", Locale: "ja", Reject: "すべて拒否", Accept: "すべて承諾", Aria: true},
	{Name: "id", Locale: "id", Reject: "Tolak semua", Accept: "Terima semua", Aria: true},
	{Name: "uz", Locale: "uz", Reject: "Rad etish", Accept: "Qabul qilish", Aria: true},
	// Real walls are not always aria-labelled: the uppercase/text-only shape must
	// still resolve to reject.
	{Name: "en-text-only", Locale: "en", Reject: "REJECT ALL", Accept: "ACCEPT ALL", NoAria: true, Decoys: true},
}

// consentTestNorm mirrors the norm() in consentPreludeJS: lowercase, drop
// diacritics and punctuation, collapse whitespace. The JS side is authoritative;
// this exists so a missing phrase is caught without launching a browser.
func consentTestNorm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if f, ok := diacriticFold[r]; ok {
			b.WriteString(f)
			continue
		}
		switch {
		case unicode.Is(unicode.M, r):
			// combining marks fold away, exactly like the JS norm()
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsSpace(r):
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// diacriticFold mirrors the JS norm() (NFD + combining-mark strip) for the
// characters that appear in consent labels.
var diacriticFold = map[rune]string{
	'á': "a", 'à': "a", 'â': "a", 'ä': "a", 'ã': "a", 'å': "a", 'ā': "a",
	'é': "e", 'è': "e", 'ê': "e", 'ë': "e", 'ē': "e",
	'í': "i", 'ì': "i", 'î': "i", 'ï': "i", 'ī': "i",
	'ó': "o", 'ò': "o", 'ô': "o", 'ö': "o", 'õ': "o", 'ø': "o", 'ō': "o",
	'ú': "u", 'ù': "u", 'û': "u", 'ü': "u", 'ū': "u",
	'ç': "c", 'ć': "c", 'č': "c", 'ñ': "n", 'ń': "n",
	'š': "s", 'ś': "s", 'ž': "z", 'ź': "z", 'ł': "l", 'ğ': "g", 'ş': "s", 'ı': "i",
	'ß': "ss", 'æ': "ae", 'œ': "oe",
}

// TestConsentPhraseTablesCoverFixtures is the fast guard: every label the bot is
// expected to meet must be in the reject table, so a new locale cannot be added
// to the fixtures without teaching the matcher about it.
func TestConsentPhraseTablesCoverFixtures(t *testing.T) {
	table := map[string]bool{}
	for _, p := range consentRejectPhrases {
		table[consentTestNorm(p)] = true
	}
	for _, f := range consentLocaleFixtures {
		if f.Reject == "" {
			continue
		}
		if !table[consentTestNorm(f.Reject)] {
			t.Errorf("locale %s: reject label %q is not in consentRejectPhrases", f.Locale, f.Reject)
		}
	}
}

// TestConsentScriptCarriesEveryPhrase catches a broken renderer: consentProbeJS
// is built with fmt.Sprintf, and a missing argument silently empties a table.
func TestConsentScriptCarriesEveryPhrase(t *testing.T) {
	js := consentProbeJS()
	for _, p := range consentRejectPhrases {
		if !strings.Contains(js, p) {
			t.Errorf("consentProbeJS does not carry reject phrase %q", p)
		}
	}
	for _, p := range consentBlockedTokens {
		if !strings.Contains(js, p) {
			t.Errorf("consentProbeJS does not carry blocked token %q", p)
		}
	}
	for _, p := range consentMarkers {
		// Markers are embedded as a JSON literal, so inner quotes arrive escaped.
		if !strings.Contains(js, p) && !strings.Contains(js, strings.ReplaceAll(p, `"`, `\"`)) {
			t.Errorf("consentProbeJS does not carry marker %q", p)
		}
	}
}

// TestConsentResultDescribeIsActionable pins the diagnostics a failed session
// shows: which surface, how many attempts, and what was tried.
func TestConsentResultDescribeIsActionable(t *testing.T) {
	stuck := ConsentResult{Present: true, Attempts: 3, Surface: "ytd-consent-bump-v2-lightbox", Tried: []string{"reject:reject all"}}
	msg := stuck.describe()
	for _, want := range []string{"still up", "3 attempts", "ytd-consent-bump-v2-lightbox", "reject all"} {
		if !strings.Contains(msg, want) {
			t.Errorf("describe() = %q, missing %q", msg, want)
		}
	}

	ok := ConsentResult{Present: true, Dismissed: true, Action: "reject", Label: "reject all", Method: "mouse", Attempts: 2}
	msg = ok.describe()
	for _, want := range []string{"dismissed", "reject", "reject all", "mouse", "2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("describe() = %q, missing %q", msg, want)
		}
	}

	if got := (ConsentResult{}).describe(); got != "no consent wall" {
		t.Errorf("empty describe() = %q", got)
	}
}

// TestPlaybackReportDiagnosticsIsActionable pins what a playback timeout says:
// the URL, the position that never moved, the consent surface, and the pixels.
func TestPlaybackReportDiagnosticsIsActionable(t *testing.T) {
	rep := PlaybackReport{
		Current: 0, Duration: 0, Paused: true, Ready: 0,
		URL: "https://www.youtube.com/watch?v=x", Title: "t", DrawErr: "",
		Samples: 4, Consent: true, Surface: "ytd-consent-bump-v2-lightbox",
		PageProblem: "",
	}
	msg := rep.describe()
	for _, want := range []string{"consent=true", "ytd-consent-bump-v2-lightbox", "https://www.youtube.com/watch?v=x"} {
		if !strings.Contains(msg, want) {
			t.Errorf("describe() = %q, missing %q", msg, want)
		}
	}
	verr := rep.failure("present")
	for _, want := range []string{"consent wall", "ytd-consent-bump-v2-lightbox", "https://www.youtube.com/watch?v=x"} {
		if !strings.Contains(verr.Error(), want) {
			t.Errorf("failure() = %q, missing %q", verr, want)
		}
	}
}
