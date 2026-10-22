package engine

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClassifyMedia(t *testing.T) {
	cases := []struct {
		name string
		in   string
		kind MediaKind
		want string // expected canonical URL (optional check via contains)
	}{
		{"youtube watch", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", MediaYouTube, "v=dQw4w9WgXcQ"},
		{"youtube short link", "https://youtu.be/dQw4w9WgXcQ", MediaYouTube, "watch?v=dQw4w9WgXcQ"},
		{"youtube shorts", "https://youtube.com/shorts/dQw4w9WgXcQ", MediaYouTube, "watch?v=dQw4w9WgXcQ"},
		{"youtube live", "https://www.youtube.com/live/dQw4w9WgXcQ", MediaYouTube, "watch?v=dQw4w9WgXcQ"},
		{"youtube playlist stays page", "https://www.youtube.com/playlist?list=PL123", MediaPage, "playlist"},
		{"mp4 direct", "https://cdn.example.com/clip.mp4", MediaDirect, "clip.mp4"},
		{"mp4 with query", "https://cdn.example.com/clip.MP4?token=abc", MediaDirect, "token=abc"},
		{"mp3 direct", "https://cdn.example.com/song.mp3", MediaDirect, "song.mp3"},
		{"googlevideo", "https://rr1---sn-abc.googlevideo.com/videoplayback?expire=1", MediaDirect, "videoplayback"},
		{"web page", "https://example.com/slides", MediaPage, "example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyMedia(tc.in)
			if err != nil {
				t.Fatalf("ClassifyMedia(%q): %v", tc.in, err)
			}
			if got.Kind != tc.kind {
				t.Fatalf("kind = %s, want %s", got.Kind, tc.kind)
			}
			if !strings.Contains(got.URL, tc.want) {
				t.Fatalf("url = %q, want it to contain %q", got.URL, tc.want)
			}
		})
	}
}

func TestClassifyMediaRejectsBadInput(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"ftp://example.com/x.mp4",
		"javascript:alert(1)",
		"not a url at all",
		"https://",
	} {
		if _, err := ClassifyMedia(in); err == nil {
			t.Fatalf("expected %q to be rejected", in)
		}
	}
}

func TestClassifyMediaYouTubeKeepsAuthuser(t *testing.T) {
	got, err := ClassifyMedia("https://www.youtube.com/watch?v=abc12345678&authuser=2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.URL, "authuser=2") {
		t.Fatalf("authuser lost: %s", got.URL)
	}
}

func TestNormalizeMeetURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://meet.google.com/abc-defg-hij", "https://meet.google.com/abc-defg-hij?hl=en-US", false},
		{"meet.google.com/abc-defg-hij", "https://meet.google.com/abc-defg-hij?hl=en-US", false},
		{"abc-defg-hij", "https://meet.google.com/abc-defg-hij?hl=en-US", false},
		{"https://meet.google.com/lookup/abcd1234", "https://meet.google.com/lookup/abcd1234?hl=en-US", false},
		{"https://meet.google.com/abc-defg-hij?hl=fa", "https://meet.google.com/abc-defg-hij?hl=fa", false},
		{"https://meet.google.com/abc-defg-hij?authuser=1", "https://meet.google.com/abc-defg-hij?hl=en-US&authuser=1", false},
		{"", "", true},
		{"https://zoom.us/j/123", "", true},
		{"https://meet.google.com/", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeMeetURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("NormalizeMeetURL(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeMeetURL(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeMeetURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShortcode(t *testing.T) {
	if got := Shortcode("https://meet.google.com/ABC-defg-hij?hl=en"); got != "abc-defg-hij" {
		t.Fatalf("Shortcode() = %q", got)
	}
	if got := Shortcode("https://meet.google.com/"); got != "" {
		t.Fatalf("Shortcode() = %q, want empty", got)
	}
}

func TestPlayerDataURL(t *testing.T) {
	tgt, err := ClassifyMedia("https://cdn.example.com/clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := PlayerDataURL(tgt, PlayerOptions{TabTitle: "TeleMeet Media", Volume: 55, Speed: 1.5})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "data:text/html;base64,") {
		t.Fatalf("not a base64 data URL: %.40s", raw)
	}
	doc := decodeDataURL(t, raw)
	if strings.Contains(doc, "__TM_BOOTSTRAP__") {
		t.Fatal("bootstrap placeholder was not replaced")
	}
	if !strings.Contains(doc, `"src":"https://cdn.example.com/clip.mp4"`) {
		t.Fatal("src missing from bootstrap")
	}
	if !strings.Contains(doc, `"tabTitle":"TeleMeet Media"`) {
		t.Fatal("tab title missing from bootstrap")
	}
	if !strings.Contains(doc, `"volume":55`) || !strings.Contains(doc, `"speed":1.5`) {
		t.Fatal("player options missing")
	}
}

func TestMediaControlJSIsFunctionForm(t *testing.T) {
	// rod evaluates function-form scripts; an IIFE breaks the js transformer.
	for name, js := range map[string]string{
		"media control":  mediaControlJS("seek", -10),
		"title lock":     titleLockJS("TeleMeet Media"),
		"player problem": playerProblemJS(),
	} {
		if !strings.HasPrefix(strings.TrimSpace(js), "() =>") {
			t.Fatalf("%s script must start with a function expression, got: %.40s", name, js)
		}
		if strings.HasSuffix(strings.TrimSpace(js), "})()") || strings.HasPrefix(strings.TrimSpace(js), "(() => {") {
			t.Fatalf("%s script is still wrapped in an IIFE", name)
		}
	}
	if !strings.Contains(mediaControlJS("seek", -10), "const arg = -10;") {
		t.Fatal("seek argument not inlined")
	}
}

func TestTitleLockMarkers(t *testing.T) {
	js := titleLockJS(`Media "Tab"`)
	if !strings.Contains(js, `const T = "Media \"Tab\"";`) {
		t.Fatalf("title not JSON-escaped: %s", js)
	}
	if !strings.Contains(js, "__tmOrigTitle") {
		t.Fatal("original title tracking missing")
	}
}

func decodeDataURL(t *testing.T, raw string) string {
	t.Helper()
	idx := strings.Index(raw, "base64,")
	if idx < 0 {
		t.Fatalf("no base64 payload in data URL")
	}
	dec, err := base64.StdEncoding.DecodeString(raw[idx+len("base64,"):])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return string(dec)
}

func TestPlayerServerServesDocuments(t *testing.T) {
	html, err := PlayerHTML(MediaTarget{URL: "https://example.com/a.mp4", Kind: MediaDirect}, PlayerOptions{TabTitle: "TeleMeet Media", Volume: 50})
	if err != nil {
		t.Fatal(err)
	}
	url, err := players.playerURL(html)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("player url = %q", url)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "https://example.com/a.mp4") {
		t.Fatalf("player doc = %d %q", resp.StatusCode, string(body[:80]))
	}
	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q", resp.Header.Get("Content-Type"))
	}
	// Unknown ids must 404, never serve another session's document.
	bad, err := http.Get(strings.TrimSuffix(url, url[len(url)-24:]) + "doesnotexist")
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != 404 {
		t.Fatalf("unknown doc = %d", bad.StatusCode)
	}
}

func TestPlayerDocsAreBounded(t *testing.T) {
	for i := 0; i < maxPlayerDocs+5; i++ {
		if _, err := players.playerURL("<html>x</html>"); err != nil {
			t.Fatal(err)
		}
	}
	players.mu.RLock()
	n := len(players.docs)
	players.mu.RUnlock()
	if n > maxPlayerDocs {
		t.Fatalf("docs = %d, want <= %d", n, maxPlayerDocs)
	}
}
