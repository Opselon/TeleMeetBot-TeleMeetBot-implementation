package media

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Netscape cookie jar parsing.
//
// download.cookies_file is the one supported answer to YouTube's account
// gate. These tests pin the format the operator hands us so the jar cannot
// silently fail to parse (which would look on the card like "the bot check
// won" instead of "the config is wrong").

func writeJar(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadCookiesFileParsesNetscapeFormat(t *testing.T) {
	p := writeJar(t, `# Netscape HTTP Cookie File
# https://curl.se/docs/http-cookies.html
.youtube.com	TRUE	/	TRUE	0	SID	abc123
.youtube.com	TRUE	/	TRUE	0	HSID	def456
.google.com	TRUE	/	FALSE	0	PREF	xyz=1
`)
	cs, err := LoadCookiesFile(p)
	if err != nil {
		t.Fatalf("LoadCookiesFile: %v", err)
	}
	if len(cs) != 3 {
		t.Fatalf("parsed %d cookies, want 3", len(cs))
	}
	want := map[string]string{"SID": "abc123", "HSID": "def456", "PREF": "xyz=1"}
	for _, c := range cs {
		v, ok := want[c.Name]
		if !ok {
			t.Fatalf("unexpected cookie %q", c.Name)
		}
		if c.Value != v {
			t.Fatalf("cookie %q = %q, want %q", c.Name, c.Value, v)
		}
		if c.Domain != "youtube.com" && c.Domain != "google.com" {
			t.Fatalf("domain %q", c.Domain)
		}
	}
}

func TestLoadCookiesFileRejectsMissingFile(t *testing.T) {
	if _, err := LoadCookiesFile(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Fatal("a missing jar must be an error, never a silent empty jar")
	}
}

func TestLoadCookiesFileRejectsEmptyJar(t *testing.T) {
	p := writeJar(t, "# only a comment\n\n# another\n")
	if _, err := LoadCookiesFile(p); err == nil {
		t.Fatal("a jar with no cookies must be an error: the operator pointed at the wrong file")
	}
}

func TestLoadCookiesFileEmptyPathIsNil(t *testing.T) {
	cs, err := LoadCookiesFile("")
	if err != nil {
		t.Fatalf("empty path: %v", err)
	}
	if cs != nil {
		t.Fatalf("empty path must yield nil, got %d cookies", len(cs))
	}
}

func TestLoadCookiesFileSkipsMalformedLines(t *testing.T) {
	p := writeJar(t, "not\treal\n.youtube.com\tTRUE\t/\tTRUE\t0\tSID\tok\ngarbage\n")
	cs, err := LoadCookiesFile(p)
	if err != nil {
		t.Fatalf("LoadCookiesFile: %v", err)
	}
	if len(cs) != 1 || cs[0].Name != "SID" {
		t.Fatalf("expected the one well-formed cookie, got %v", cs)
	}
}

// ---- the account gate classifier -------------------------------------

func TestAccountGateClassifier(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"ascii bot check", "ERROR: [youtube] X: Sign in to confirm you're not a bot.", true},
		{"curly-quote bot check", "Sign in to confirm you’re not a bot", true},
		{"unrelated extractor verdict", "ERROR: Unsupported URL", false},
		{"network shaped", "Unable to download API page: TLS/SSL connection has been closed (EOF)", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountGate([]byte(tc.in)); got != tc.want {
				t.Fatalf("accountGate(%q) = %v, want %v", tc.in, got, tc.want)
			}
			if tc.want && !proxyBlocked([]byte(tc.in)) {
				t.Fatalf("proxyBlocked must stay true for an account gate so a configured jar still gets other routes")
			}
		})
	}
}

// ---- the argv carries the jar ----------------------------------------

func TestYTDLPArgCarriesCookiesFile(t *testing.T) {
	m, err := NewManager(Options{Enabled: true, Dir: t.TempDir(), CookiesFile: "/etc/yt/cookies.txt", Timeout: 30 * time.Second, TTL: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := m.ytdlpArgs("https://www.youtube.com/watch?v=x", "sid", "auto", "", "")
	found := false
	for i, a := range args {
		if a == "--cookies" && i+1 < len(args) && args[i+1] == "/etc/yt/cookies.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("--cookies missing from argv: %v", args)
	}
	// The jar must come BEFORE the positional URL, or yt-dlp treats the
	// value as a second video. The URL is the only non-flag argument.
	urlAt := -1
	cookiesAt := -1
	for i, a := range args {
		if a == "https://www.youtube.com/watch?v=x" {
			urlAt = i
		}
		if a == "--cookies" {
			cookiesAt = i
		}
	}
	if urlAt < 0 || cookiesAt < 0 || cookiesAt > urlAt {
		t.Fatalf("--cookies must precede the positional URL: cookies@%d url@%d", cookiesAt, urlAt)
	}
}

func TestYTDLOmitsCookiesWhenUnconfigured(t *testing.T) {
	m, err := NewManager(Options{Enabled: true, Dir: t.TempDir(), Timeout: 30 * time.Second, TTL: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range m.ytdlpArgs("https://example.com/v.mp4", "sid", "auto", "", "") {
		if a == "--cookies" {
			t.Fatal("an unconfigured jar must not reach the argv")
		}
	}
}
