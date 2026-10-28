package media

// Source format inspection: what a URL can actually deliver, without
// downloading it.
//
// For extractor-backed sources (YouTube and everything else yt-dlp knows)
// `yt-dlp -J` answers in one lightweight round trip: the format ladder with
// heights, codecs, frame rates, bitrates and approximate sizes. Direct media
// files are reported honestly as single-file sources — the browser reads
// them directly and there is no ladder to select from.
//
// The cache exists so re-opening the quality panel does not re-probe the
// source every time. It stores only derived metadata (heights, codecs,
// sizes), never signed URLs or media bytes, and every entry carries an
// explicit expiry.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SourceFormat is one selectable rendition of a source.
type SourceFormat struct {
	FormatID   string  `json:"format_id"`
	Height     int     `json:"height"`
	Width      int     `json:"width"`
	FPS        float64 `json:"fps"`
	VCodec     string  `json:"vcodec"`
	ACodec     string  `json:"acodec"`
	Ext        string  `json:"ext"`
	TBR        float64 `json:"tbr"`  // total bitrate, kbit/s
	ABR        float64 `json:"abr"`  // audio bitrate, kbit/s
	SizeBytes  int64   `json:"-"`    // exact size when the extractor knows it
	SizeApprox int64   `json:"-"`    // filesize_approx otherwise
	Note       string  `json:"note"` // e.g. "DASH video", " Premium"
}

// HasVideo reports whether the format carries a video stream.
func (f SourceFormat) HasVideo() bool {
	return f.VCodec != "" && f.VCodec != "none"
}

// HasAudio reports whether the format carries an audio stream.
func (f SourceFormat) HasAudio() bool {
	return f.ACodec != "" && f.ACodec != "none"
}

// SizeBest returns the best available size estimate (0 = unknown).
func (f SourceFormat) SizeBest() int64 {
	if f.SizeBytes > 0 {
		return f.SizeBytes
	}
	return f.SizeApprox
}

// SourceInfo is the inspection result for one URL.
type SourceInfo struct {
	// Title of the source, when the extractor provides one.
	Title string `json:"title,omitempty"`
	// Duration in seconds (0 = unknown/live).
	Duration float64 `json:"duration,omitempty"`
	// IsLive marks live sources: heights exist but seeking and fixed
	// rendition locks behave differently.
	IsLive bool `json:"is_live,omitempty"`
	// Formats is the extractor's ladder, video formats first by height.
	Formats []SourceFormat `json:"formats"`
	// SingleFile marks direct media URLs: there is no ladder, the browser
	// plays what the server serves.
	SingleFile bool   `json:"single_file,omitempty"`
	Via        string `json:"via"` // "yt-dlp" | "direct"
	// RequiresFFmpeg is true when playing a chosen rendition needs a merge.
	RequiresFFmpeg bool `json:"requires_ffmpeg,omitempty"`
	// InspectedAt is when the metadata was fetched (cache bookkeeping).
	InspectedAt time.Time `json:"inspected_at"`
	// Subtitles lists caption tracks the extractor reported, manual first,
	// then automatic. Automatic tracks are always labelled — a translated
	// track is offered only when the source really ships one.
	Subtitles []SubtitleTrack `json:"subtitles,omitempty"`
}

// InspectError wraps a failed probe with its cause for the UI.
var ErrInspectUnsupported = errors.New("source does not support format inspection")

// inspectCacheEntry is one cached probe result.
type inspectCacheEntry struct {
	info    *SourceInfo
	expires time.Time
}

// inspectCacheTTL bounds cached metadata. Formats ladders change rarely;
// ten minutes keeps re-opening the panel cheap without serving stale data
// for a whole session.
const inspectCacheTTL = 10 * time.Minute

// inspectCacheMax bounds the cache: 64 sources is far more than one
// operator's working set and keeps memory trivial.
const inspectCacheMax = 64

// Inspect probes a URL's available formats. Direct media files report
// SingleFile without spawning yt-dlp; everything else goes through
// `yt-dlp -J` (simulate only — nothing is downloaded).
func (m *Manager) Inspect(ctx context.Context, rawURL string) (*SourceInfo, error) {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return nil, err
	}
	link := u.String()

	// Direct media files need no subprocess: the browser plays what the
	// server serves and the "ladder" is that one file.
	if KindForURL(link) == "direct" {
		return &SourceInfo{
			SingleFile:  true,
			Via:         "direct",
			InspectedAt: time.Now(),
		}, nil
	}

	if cached := m.cachedInspect(link); cached != nil {
		return cached, nil
	}

	bin, ok := m.YTDLP()
	if !ok {
		return nil, &ErrMissingTool{
			Name: "yt-dlp",
			Path: firstNonEmpty(m.opts.YTDLPPath, "yt-dlp"),
			Hint: "install yt-dlp to inspect available formats",
		}
	}
	args := []string{
		"-J",            // dump single JSON, simulate: nothing is downloaded
		"--no-playlist", // one item, never a playlist avalanche
		"--no-warnings",
		"--no-check-certificate", // the SSRF gate already validated the host
		"--socket-timeout", "30",
	}
	if p := strings.TrimSpace(m.opts.Proxy); p != "" {
		args = append(args, "--proxy", p)
	}
	args = append(args, link)

	stdout, stderr, err := m.runYTDLP(ctx, bin, args, nil)
	if err != nil {
		return nil, fmt.Errorf("inspect source: %w%s", err, stderrSuffix(stderr))
	}
	info, err := parseSourceInfo(stdout)
	if err != nil {
		return nil, err
	}
	info.Via = "yt-dlp"
	info.InspectedAt = time.Now()
	m.cacheInspect(link, info)
	return info, nil
}

// ytDLPJSON is the subset of `yt-dlp -J` output the panel needs.
type ytDLPJSON struct {
	Title             string                   `json:"title"`
	Duration          float64                  `json:"duration"`
	IsLive            bool                     `json:"is_live"`
	Subtitles         map[string][]subtitleURL `json:"subtitles"`
	AutomaticCaptions map[string][]subtitleURL `json:"automatic_captions"`
	Formats           []struct {
		FormatID       string  `json:"format_id"`
		Height         int     `json:"height"`
		Width          int     `json:"width"`
		FPS            float64 `json:"fps"`
		VCodec         string  `json:"vcodec"`
		ACodec         string  `json:"acodec"`
		Ext            string  `json:"ext"`
		TBR            float64 `json:"tbr"`
		ABR            float64 `json:"abr"`
		Filesize       int64   `json:"filesize"`
		FilesizeApprox int64   `json:"filesize_approx"`
		FormatNote     string  `json:"format_note"`
	} `json:"formats"`
}

// KindForURL classifies a URL for inspection: "youtube" for the known
// YouTube hosts, "direct" for URLs ending in a media extension, "other"
// otherwise (yt-dlp supports many extractors, so "other" still gets a probe;
// a page that is not media fails with ErrInspectUnsupported). The engine's
// classifier remains the authority for playback decisions; this one only
// routes the inspection round trip.
func KindForURL(rawURL string) string {
	host := strings.ToLower(strings.TrimSpace(rawURL))
	if strings.Contains(host, "youtube.com/") || strings.Contains(host, "youtu.be/") {
		return "youtube"
	}
	i := strings.LastIndexByte(host, '.')
	j := strings.LastIndexByte(host, '/')
	if i > j {
		switch strings.ToLower(host[i:]) {
		case ".mp4", ".m4v", ".webm", ".mkv", ".mov", ".ogv", ".mp3", ".m4a", ".aac", ".flac", ".wav", ".ogg", ".opus":
			return "direct"
		}
	}
	return "other"
}

// parseSourceInfo decodes and normalizes `yt-dlp -J` output.
func parseSourceInfo(stdout []byte) (*SourceInfo, error) {
	var raw ytDLPJSON
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInspectUnsupported, err)
	}
	info := &SourceInfo{
		Title:    raw.Title,
		Duration: raw.Duration,
		IsLive:   raw.IsLive,
		Formats:  make([]SourceFormat, 0, len(raw.Formats)),
	}
	for _, f := range raw.Formats {
		sf := SourceFormat{
			FormatID:   f.FormatID,
			Height:     f.Height,
			Width:      f.Width,
			FPS:        f.FPS,
			VCodec:     f.VCodec,
			ACodec:     f.ACodec,
			Ext:        f.Ext,
			TBR:        f.TBR,
			ABR:        f.ABR,
			SizeBytes:  f.Filesize,
			SizeApprox: f.FilesizeApprox,
			Note:       f.FormatNote,
		}
		if !sf.HasVideo() && !sf.HasAudio() {
			continue
		}
		if !sf.HasVideo() && sf.Height == 0 {
			// Audio-only renditions are kept (they matter for the merge
			// question) but they do not represent a "resolution choice".
			sf.Note = strings.TrimSpace(sf.Note + " audio-only")
		}
		info.Formats = append(info.Formats, sf)
		if sf.HasVideo() && !sf.HasAudio() {
			info.RequiresFFmpeg = true
		}
	}
	manualLangs, autoLangs := SubtitleLangsFor(raw.Subtitles, raw.AutomaticCaptions)
	for _, lang := range manualLangs {
		ext := ""
		if urls := raw.Subtitles[lang]; len(urls) > 0 {
			ext = urls[0].Ext
		}
		info.Subtitles = append(info.Subtitles, SubtitleTrack{Lang: lang, Ext: ext})
	}
	for _, lang := range autoLangs {
		ext := ""
		if urls := raw.AutomaticCaptions[lang]; len(urls) > 0 {
			ext = urls[0].Ext
		}
		info.Subtitles = append(info.Subtitles, SubtitleTrack{Lang: lang, Auto: true, Ext: ext})
	}
	if len(info.Formats) == 0 && len(info.Subtitles) == 0 {
		return nil, fmt.Errorf("%w: no formats reported", ErrInspectUnsupported)
	}
	return info, nil
}

// ---- cache -----------------------------------------------------------------

func (m *Manager) cachedInspect(link string) *SourceInfo {
	m.inspectMu.Lock()
	defer m.inspectMu.Unlock()
	if e, ok := m.inspectCache[link]; ok && time.Now().Before(e.expires) {
		return e.info
	}
	return nil
}

func (m *Manager) cacheInspect(link string, info *SourceInfo) {
	m.inspectMu.Lock()
	defer m.inspectMu.Unlock()
	if m.inspectCache == nil {
		m.inspectCache = make(map[string]inspectCacheEntry, 8)
	}
	// Simple bound: when full, drop the soonest-expiring entry.
	if len(m.inspectCache) >= inspectCacheMax {
		var oldestKey string
		var oldest time.Time
		for k, e := range m.inspectCache {
			if oldestKey == "" || e.expires.Before(oldest) {
				oldestKey, oldest = k, e.expires
			}
		}
		delete(m.inspectCache, oldestKey)
	}
	m.inspectCache[link] = inspectCacheEntry{info: info, expires: time.Now().Add(inspectCacheTTL)}
}

// ---- ladder views ----------------------------------------------------------

// DistinctHeights lists the distinct video heights in the ladder, tallest
// first. Audio-only renditions are skipped: they are not resolution choices.
func DistinctHeights(formats []SourceFormat) []int {
	seen := make(map[int]bool, 8)
	var out []int
	for _, f := range formats {
		if !f.HasVideo() || f.Height <= 0 {
			continue
		}
		if !seen[f.Height] {
			seen[f.Height] = true
			out = append(out, f.Height)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] > out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// BestAtOrBelow returns the best video format whose height is exactly h —
// the rung the ladder view describes — preferring higher bitrate, then the
// first seen. It returns the zero format when none matches.
func BestAtOrBelow(formats []SourceFormat, h int) SourceFormat {
	var best SourceFormat
	for _, f := range formats {
		if !f.HasVideo() || f.Height != h {
			continue
		}
		if best.FormatID == "" || f.TBR > best.TBR {
			best = f
		}
	}
	return best
}

// CodecShort shortens a codec string for a dashboard line.
func CodecShort(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	switch {
	case strings.HasPrefix(c, "avc"), strings.HasPrefix(c, "h264"), strings.HasPrefix(c, "h.264"):
		return "h264"
	case strings.HasPrefix(c, "vp9"):
		return "vp9"
	case strings.HasPrefix(c, "av01"), strings.HasPrefix(c, "av1"):
		return "av1"
	case strings.HasPrefix(c, "vp8"):
		return "vp8"
	case strings.HasPrefix(c, "hevc"), strings.HasPrefix(c, "h265"):
		return "hevc"
	case strings.HasPrefix(c, "mp4a"), strings.HasPrefix(c, "aac"):
		return "aac"
	case strings.HasPrefix(c, "opus"):
		return "opus"
	default:
		if i := strings.IndexByte(c, '.'); i > 0 {
			return c[:i]
		}
		return c
	}
}
