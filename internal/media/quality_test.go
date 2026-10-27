package media

// Quality-policy tests: policy normalization, ladder construction, the
// selected-format parser, and the format-inspection probe with its cache.

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestNormalizeQualityPolicy(t *testing.T) {
	for raw, want := range map[string]string{
		"": QualityAuto, "AUTO": QualityAuto, "bogus": QualityAuto,
		"saver": QualitySaver, "balanced": QualityBalanced,
		// hd720 is a display alias: it selects the same ladder as balanced.
		"hd720": QualityBalanced, "fhd1080": QualityFHD1080, "best": QualityBest,
	} {
		if got := NormalizeQualityPolicy(raw); got != want {
			t.Fatalf("NormalizeQualityPolicy(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestMaxHeightOf(t *testing.T) {
	cases := map[string]int{
		QualitySaver: 360, QualityBalanced: 720, QualityHD720: 720,
		QualityFHD1080: 1080, QualityAuto: 0, QualityBest: 0,
	}
	for p, want := range cases {
		if got := MaxHeightOf(p); got != want {
			t.Fatalf("MaxHeightOf(%q) = %d, want %d", p, got, want)
		}
	}
}

func TestFormatSelectorForLadders(t *testing.T) {
	// Auto keeps the historical resource-aware ladder: 720p cap on the
	// h264-first rungs, merge capability, ultimate fallback.
	auto := FormatSelectorFor(QualityAuto, true)
	if !strings.Contains(auto, "vcodec^=avc1][height<=720]") {
		t.Fatalf("auto ladder lost the h264 720p rung: %q", auto)
	}
	if !strings.HasSuffix(auto, "bv*/ba/b*") {
		t.Fatalf("auto ladder lost the ultimate fallback: %q", auto)
	}

	// Capped policies carry their cap on the preferred rungs but keep the
	// uncapped tail so an absent rung degrades instead of failing.
	fhd := FormatSelectorFor(QualityFHD1080, true)
	if !strings.Contains(fhd, "bv*[ext=mp4][vcodec^=avc1][height<=1080]+ba[ext=m4a]") {
		t.Fatalf("fhd ladder missing the capped rung: %q", fhd)
	}
	if !strings.HasSuffix(fhd, "bv*/ba/b*") {
		t.Fatalf("fhd ladder lost the uncapped fallback tail: %q", fhd)
	}

	saver := FormatSelectorFor(QualitySaver, true)
	if !strings.Contains(saver, "bv*[ext=mp4][vcodec^=avc1][height<=360]+ba[ext=m4a]") {
		t.Fatalf("saver ladder missing the 360p rung: %q", saver)
	}

	// Best has no cap at all.
	best := FormatSelectorFor(QualityBest, true)
	if strings.Contains(best, "height<=") {
		t.Fatalf("best ladder must not cap: %q", best)
	}

	// Without ffmpeg the ladder degrades to progressive-only formats: no
	// merge rungs, so the download never requests something yt-dlp cannot
	// deliver.
	noFF := FormatSelectorFor(QualityBalanced, false)
	if strings.Contains(noFF, "+ba") {
		t.Fatalf("ffmpeg-less ladder requested a merge: %q", noFF)
	}
	if !strings.Contains(noFF, "b[acodec!=none]") {
		t.Fatalf("ffmpeg-less ladder lost progressive fallback: %q", noFF)
	}
}

func TestParseSelectedFromPrint(t *testing.T) {
	// A merged download prints one line per output file; the last line is
	// the final artifact.
	stdout := []byte(`[download] Destination: vid.f135.mp4
f135,1920,1080,30,avc1.640028,none,mp4,4523
f140,0,0,0,none,mp4a.40.2,m4a,129
f135,1920,1080,30,avc1.640028,mp4a.40.2,mp4,4652
`)
	sel := parseSelectedFromPrint(stdout)
	if sel.FormatID != "f135" || sel.Height != 1080 || sel.Width != 1920 {
		t.Fatalf("selected = %+v", sel)
	}
	if sel.VCodec != "avc1.640028" || sel.ACodec != "mp4a.40.2" {
		t.Fatalf("codecs = %q/%q", sel.VCodec, sel.ACodec)
	}
	if sel.FPS != 30 || sel.TBR != 4652 {
		t.Fatalf("fps/tbr = %.2f/%.2f", sel.FPS, sel.TBR)
	}
	if got := sel.Describe(); !strings.Contains(got, "1920x1080") || !strings.Contains(got, "h264") {
		t.Fatalf("Describe = %q", got)
	}

	// Malformed input yields the zero value: the UI reports "not reported"
	// instead of inventing a resolution.
	if sel := parseSelectedFromPrint([]byte("nothing here")); sel.FormatID != "" {
		t.Fatalf("garbage parsed as %+v", sel)
	}
	if sel := parseSelectedFromPrint(nil); sel.Height != 0 {
		t.Fatalf("empty parsed as %+v", sel)
	}
}

func TestInspectViaYTDLPMock(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Enabled = true
	var calls int
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.opts.YTDLPPath = "yt-dlp-probe" // force the fake where no yt-dlp is installed
	m.lookupFn = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	m.ytdlpFn = func(ctx context.Context, args []string) ([]byte, []byte, error) {
		calls++
		for _, a := range args {
			if a == "-J" {
				return []byte(`{"title":"Test Video","duration":61.5,"formats":[
					{"format_id":"f135","height":1080,"width":1920,"fps":30,"vcodec":"avc1.640028","acodec":"none","ext":"mp4","tbr":4523,"filesize_approx":34500000},
					{"format_id":"f136","height":2160,"width":3840,"vcodec":"vp9.2","acodec":"none","ext":"webm","tbr":15000},
					{"format_id":"f140","height":0,"vcodec":"none","acodec":"mp4a.40.2","ext":"m4a","abr":129}
				]}`), nil, nil
			}
		}
		return nil, nil, errors.New("unexpected yt-dlp invocation")
	}

	info, err := m.Inspect(context.Background(), "https://www.youtube.com/watch?v=abcdefghijk")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.Title != "Test Video" || info.Duration != 61.5 {
		t.Fatalf("info = %+v", info)
	}
	if len(info.Formats) != 3 {
		t.Fatalf("formats = %d, want 3", len(info.Formats))
	}
	heights := DistinctHeights(info.Formats)
	if len(heights) != 2 || heights[0] != 2160 || heights[1] != 1080 {
		t.Fatalf("heights = %v", heights)
	}
	best := BestAtOrBelow(info.Formats, 1080)
	if best.FormatID != "f135" || best.SizeBest() != 34500000 {
		t.Fatalf("best at 1080 = %+v", best)
	}
	if !info.RequiresFFmpeg {
		t.Fatal("video-only renditions must flag the ffmpeg requirement")
	}

	// The second probe is served from cache: yt-dlp is not invoked again.
	if _, err := m.Inspect(context.Background(), "https://www.youtube.com/watch?v=abcdefghijk"); err != nil {
		t.Fatalf("cached inspect: %v", err)
	}
	if calls != 1 {
		t.Fatalf("yt-dlp invoked %d times, want 1 (cache miss)", calls)
	}
}

func TestInspectDirectFileIsSingleFileSource(t *testing.T) {
	opts := DefaultOptions(t.TempDir())
	opts.Enabled = true
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.ytdlpFn = func(ctx context.Context, args []string) ([]byte, []byte, error) {
		t.Fatal("direct files must not spawn yt-dlp")
		return nil, nil, nil
	}
	info, err := m.Inspect(context.Background(), "https://cdn.example.com/movie.mp4")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !info.SingleFile || info.Via != "direct" {
		t.Fatalf("info = %+v", info)
	}
}

func TestInspectRejectsInvalidURLs(t *testing.T) {
	opts := DefaultOptions(t.TempDir())
	opts.Enabled = true
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// A private destination must be rejected by the SSRF gate, not probed.
	if _, err := m.Inspect(context.Background(), "http://127.0.0.1:8080/x.mp4"); err == nil {
		t.Fatal("private URL accepted by Inspect")
	}
}

func TestDownloadAppliesQualityPolicyAndReportsSelection(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Enabled = true
	opts.Timeout = 5_000_000_000_000 // generous; the fake returns instantly
	var gotSelector string
	m, err := NewManager(opts, &testLog{t: t})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Force the fake to run even where no yt-dlp/ffmpeg is installed, and
	// make the merge ladder consider ffmpeg available.
	m.opts.YTDLPPath = "yt-dlp-probe"
	m.opts.FFmpegPath = "ffmpeg-probe"
	m.lookupFn = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	m.retryDelay = func(int) time.Duration { return 0 }
	m.ytdlpFn = func(ctx context.Context, args []string) ([]byte, []byte, error) {
		for i, a := range args {
			if a == "-f" && i+1 < len(args) {
				gotSelector = args[i+1]
			}
		}
		// The stdout mimics what a real run prints with --print after_move.
		return []byte("f135,1920,1080,30,avc1.640028,none,mp4,4523\n"), nil, nil
	}

	// The fake never writes a file, so the download fails at file discovery;
	// the selector it was given is what this test pins.
	_, err = m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "qtest", Quality: "fhd1080",
	})
	if err == nil {
		t.Fatal("expected the missing-file failure from the fake runner")
	}
	if !strings.Contains(gotSelector, "[height<=1080]") {
		t.Fatalf("selector = %q, want the 1080 cap", gotSelector)
	}
}
