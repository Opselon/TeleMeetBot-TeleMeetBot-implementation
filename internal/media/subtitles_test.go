package media

// Subtitle tests: SRT→VTT conversion, inspection parsing, artifact
// preparation (verify + convert + serve) and the language list ordering.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSRTToVTT(t *testing.T) {
	srt := "1\r\n00:00:01,000 --> 00:00:03,500\r\nHello\r\n\r\n2\r\n00:00:04,200 --> 00:00:06,000\r\nWorld\r\n"
	vtt := SRTToVTT(srt)
	if !strings.HasPrefix(vtt, "WEBVTT\n\n") {
		t.Fatalf("missing WEBVTT header: %q", vtt)
	}
	if strings.Contains(vtt, ",") {
		t.Fatalf("timecode commas not converted: %q", vtt)
	}
	if !strings.Contains(vtt, "00:00:01.000 --> 00:00:03.500") {
		t.Fatalf("timecode dots wrong: %q", vtt)
	}
	if !strings.Contains(vtt, "Hello") || !strings.Contains(vtt, "World") {
		t.Fatalf("content lost: %q", vtt)
	}
}

func TestSRTToVTTHandlesMalformedInput(t *testing.T) {
	// Non-SRT garbage still produces a syntactically valid (empty) VTT: the
	// verification step catches real format problems before serving.
	vtt := SRTToVTT("not an srt file")
	if !strings.HasPrefix(vtt, "WEBVTT") {
		t.Fatalf("no header: %q", vtt)
	}
}

func TestInspectParsesSubtitleTracks(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Enabled = true
	opts.YTDLPPath = "yt-dlp-probe"
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	m.ytdlpFn = func(ctx context.Context, args []string) ([]byte, []byte, error) {
		return []byte(`{"title":"Cap Video","duration":30,"formats":[{"format_id":"f18","height":360,"vcodec":"avc1","acodec":"mp4a","ext":"mp4"}],
		"subtitles": {"en": [{"ext":"vtt","url":"https://x/en.vtt"}], "fa": [{"ext":"srt","url":"https://x/fa.srt"}]},
		"automatic_captions": {"de": [{"ext":"vtt","url":"https://x/de.vtt"}]}}`), nil, nil
	}
	info, err := m.Inspect(context.Background(), "https://www.youtube.com/watch?v=abcdefghijk")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(info.Subtitles) != 3 {
		t.Fatalf("subtitles = %+v", info.Subtitles)
	}
	// Manual tracks first, sorted; automatic last and labelled.
	if info.Subtitles[0].Lang != "en" || info.Subtitles[0].Auto {
		t.Fatalf("first = %+v", info.Subtitles[0])
	}
	if info.Subtitles[1].Lang != "fa" {
		t.Fatalf("second = %+v", info.Subtitles[1])
	}
	if info.Subtitles[2].Lang != "de" || !info.Subtitles[2].Auto {
		t.Fatalf("auto track = %+v", info.Subtitles[2])
	}
}

func TestPrepareSubtitleVerifiesAndServes(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Enabled = true
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	expires := time.Now().Add(time.Hour)

	// A genuine VTT artifact is served over loopback with the parsed lang.
	vtt := filepath.Join(dir, "vid.en.vtt")
	if err := os.WriteFile(vtt, []byte("WEBVTT\n\n1\n00:00:01.000 --> 00:00:02.000\nhi\n"), 0o600); err != nil {
		t.Fatalf("write vtt: %v", err)
	}
	sub, err := m.prepareSubtitle("vid", expires)
	if err != nil {
		t.Fatalf("prepareSubtitle: %v", err)
	}
	if sub.Lang != "en" || sub.Format != "vtt" || sub.URL == "" {
		t.Fatalf("subtitle = %+v", sub)
	}
	if !strings.HasPrefix(sub.Path, dir) {
		t.Fatalf("artifact escaped the managed dir: %q", sub.Path)
	}

	// An SRT artifact is converted and the original removed.
	srt := filepath.Join(dir, "vid2.fa.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,000\nسلام\n"), 0o600); err != nil {
		t.Fatalf("write srt: %v", err)
	}
	sub2, err := m.prepareSubtitle("vid2", expires)
	if err != nil {
		t.Fatalf("prepareSubtitle srt: %v", err)
	}
	if sub2.Lang != "fa" || !strings.HasSuffix(sub2.Path, ".vtt") {
		t.Fatalf("srt subtitle = %+v", sub2)
	}
	if _, err := os.Stat(srt); !os.IsNotExist(err) {
		t.Fatal("srt original not removed after conversion")
	}
	raw, _ := os.ReadFile(sub2.Path)
	if !strings.Contains(string(raw), "سلام") || !strings.Contains(string(raw), "00:00:01.000") {
		t.Fatalf("converted content wrong: %q", string(raw))
	}

	// A non-WebVTT file masquerading as one is rejected, not served.
	bad := filepath.Join(dir, "vid3.xx.vtt")
	if err := os.WriteFile(bad, []byte("<html>not captions</html>"), 0o600); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	if _, err := m.prepareSubtitle("vid3", expires); err == nil {
		t.Fatal("fake vtt accepted")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("rejected artifact not cleaned up")
	}

	// A missing artifact is a clean error.
	if _, err := m.prepareSubtitle("vid-none", expires); err == nil {
		t.Fatal("missing artifact accepted")
	}
}

func TestSubtitleLangsForOrdering(t *testing.T) {
	manual := map[string][]subtitleURL{
		"en": {{Ext: "vtt", URL: "u"}}, "fa": {{Ext: "srt", URL: "u"}}, "live_chat": {{Ext: "json", URL: "u"}},
	}
	auto := map[string][]subtitleURL{"de": {{Ext: "vtt", URL: "u"}}, "": {{Ext: "vtt", URL: "u"}}}
	ml, al := SubtitleLangsFor(manual, auto)
	if len(ml) != 2 || ml[0] != "en" || ml[1] != "fa" {
		t.Fatalf("manual langs = %v", ml)
	}
	if len(al) != 1 || al[0] != "de" {
		t.Fatalf("auto langs = %v", al)
	}
}
