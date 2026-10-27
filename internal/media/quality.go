package media

// Video-quality policies: how a requested quality maps onto the real
// extraction pipeline.
//
// A policy is a maximum-height target plus a codec preference, not a
// guarantee: yt-dlp picks the best available format at or below the cap and
// the ladder falls through when nothing matches. What was actually selected
// is reported back in Result.Selected and SourceInfo, so the UI can always
// show requested vs. selected honestly.

import (
	"fmt"
	"strconv"
	"strings"
)

// Quality policies.
const (
	QualityAuto     = "auto"     // resource-aware default (the historical ladder)
	QualitySaver    = "saver"    // up to 360p
	QualityBalanced = "balanced" // up to 720p, any codec
	QualityHD720    = "hd720"    // capped at 720p
	QualityFHD1080  = "fhd1080"  // capped at 1080p
	QualityBest     = "best"     // no height cap (size ceiling still applies)
)

// NormalizeQualityPolicy validates a policy string, falling back to auto.
func NormalizeQualityPolicy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case QualitySaver:
		return QualitySaver
	case QualityBalanced, QualityHD720:
		// hd720 and balanced describe the same yt-dlp ladder (both cap at
		// 720p); they are separate labels so the UI can offer both without
		// pretending they select different files.
		return QualityBalanced
	case QualityFHD1080:
		return QualityFHD1080
	case QualityBest:
		return QualityBest
	default:
		return QualityAuto
	}
}

// QualityPolicyDisplay is the human-readable policy name.
func QualityPolicyDisplay(p string) string {
	switch NormalizeQualityPolicy(p) {
	case QualitySaver:
		return "Data Saver (≤360p)"
	case QualityBalanced:
		return "Balanced (≤720p)"
	case QualityFHD1080:
		return "Full HD (≤1080p)"
	case QualityBest:
		return "Best Available"
	default:
		return "Auto"
	}
}

// MaxHeightOf returns the policy's height cap; 0 means no cap. The height is
// a maximum target: when the source has nothing at or below it, yt-dlp's
// ladder falls through to lower heights and the selected format is reported.
func MaxHeightOf(p string) int {
	switch NormalizeQualityPolicy(p) {
	case QualitySaver:
		return 360
	case QualityBalanced:
		return 720
	case QualityFHD1080:
		return 1080
	default:
		return 0 // auto and best: the ladder's own ordering decides
	}
}

// FormatSelectorFor builds the yt-dlp format ladder for a policy.
//
// The merge ladder (bv*+ba) needs ffmpeg; without it only single-file
// (progressive) formats are usable, so the ladder degrades to muxed
// candidates only and the caller reports the limitation — asking yt-dlp for
// a merge it cannot perform would just fail the download.
//
// Codec preference: H.264 in MP4 is deliberately preferred at the front of
// every ladder — software AV1/VP9 decode competes with Meet's encoder on a
// small host, and the container is the one the merge step pins anyway.
func FormatSelectorFor(policy string, ffmpeg bool) string {
	p := NormalizeQualityPolicy(policy)
	if !ffmpeg {
		// Progressive-only ladder: every candidate carries its own audio, so
		// no merge is ever attempted. The final rung ignores the cap rather
		// than failing outright.
		var ladder []string
		if h := MaxHeightOf(p); h > 0 {
			capV := fmt.Sprintf("[height<=%d]", h)
			ladder = []string{
				"b[ext=mp4][vcodec^=avc1][acodec!=none]" + capV,
				"b[ext=mp4][acodec!=none]" + capV,
				"b[acodec!=none]" + capV,
				"b[acodec!=none]",
			}
		} else {
			ladder = []string{
				"b[ext=mp4][vcodec^=avc1][acodec!=none]",
				"b[ext=mp4][acodec!=none]",
				"b[acodec!=none]",
			}
		}
		return strings.Join(ladder, "/")
	}

	// Uncapped tail: when the source has nothing at or below the cap, fall
	// through instead of failing — the selected format is reported either
	// way. It is also the whole ladder for "best".
	tail := []string{
		"bv*[ext=mp4][vcodec^=avc1]+ba[ext=m4a]",
		"bv*[ext=mp4]+ba[ext=m4a]",
		"b[ext=mp4][acodec!=none]",
		"bv*+ba/b",
		"bv*/ba/b*",
	}
	switch p {
	case QualityAuto:
		// The historical resource-aware default, preserved verbatim: 720p
		// h264-first so software decode shares 2 vCPUs with Meet's encoder.
		return strings.Join([]string{
			"bv*[ext=mp4][vcodec^=avc1][height<=720]+ba[ext=m4a]",
			"bv*[height<=720]+ba",
			"bv*[ext=mp4][vcodec^=avc1]+ba[ext=m4a]",
			"bv*[ext=mp4]+ba[ext=m4a]",
			"b[ext=mp4][acodec!=none]",
			"bv*+ba/b",
			"bv*/ba/b*",
		}, "/")
	case QualityBest:
		return strings.Join(tail, "/")
	default:
		capV := fmt.Sprintf("[height<=%d]", MaxHeightOf(p))
		merge := []string{
			"bv*[ext=mp4][vcodec^=avc1]" + capV + "+ba[ext=m4a]",
			"bv*" + capV + "+ba",
			// Progressive fallback for sources that still ship muxed files.
			"b[ext=mp4][acodec!=none]" + capV,
			"b[acodec!=none]" + capV,
		}
		return strings.Join(append(merge, tail...), "/")
	}
}

// SelectedFormat is what yt-dlp actually chose, parsed from the download's
// own --print output. Requested and selected are different things and both
// are reported.
type SelectedFormat struct {
	FormatID string  `json:"format_id"`
	Height   int     `json:"height"`
	Width    int     `json:"width"`
	FPS      float64 `json:"fps"`
	VCodec   string  `json:"vcodec"`
	ACodec   string  `json:"acodec"`
	Ext      string  `json:"ext"`
	// TBR is the total bitrate (kbit/s) when the extractor reports it.
	TBR float64 `json:"tbr"`
}

// Describe renders the selected format for a dashboard line. It never
// claims a resolution that was not reported by the extractor.
func (s SelectedFormat) Describe() string {
	if s.FormatID == "" {
		return ""
	}
	var sb strings.Builder
	if s.Height > 0 {
		if s.Width > 0 {
			sb.WriteString(fmt.Sprintf("%dx%d", s.Width, s.Height))
		} else {
			sb.WriteString(fmt.Sprintf("%dp", s.Height))
		}
	}
	if s.VCodec != "" && s.VCodec != "none" {
		if sb.Len() > 0 {
			sb.WriteString(" · ")
		}
		sb.WriteString(codecLabel(s.VCodec))
	}
	if s.ACodec != "" && s.ACodec != "none" {
		if sb.Len() > 0 {
			sb.WriteString(" + ")
		}
		sb.WriteString(codecLabel(s.ACodec))
	}
	if s.FPS > 0 && s.FPS != float64(int(s.FPS)) {
		sb.WriteString(fmt.Sprintf(" %.4gfps", s.FPS))
	}
	if s.Ext != "" {
		sb.WriteString(" (" + s.Ext + ")")
	}
	if sb.Len() == 0 {
		return s.FormatID
	}
	return sb.String()
}

// codecLabel shortens common codec names for the card.
func codecLabel(c string) string {
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
		return truncateRunes(c, 18)
	}
}

// truncateRunes shortens s to at most n runes.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// selectedPrintFields is the field count the fetch's --print template emits:
// format_id,width,height,fps,vcodec,acodec,ext,tbr (comma-separated so the
// argv stays free of shell metacharacters).
const selectedPrintFields = 8

// parseSelectedFromPrint extracts the last after_move line yt-dlp printed
// during a real download. A merged download prints one line per output file;
// the last line is the final (merged) artifact, which is what the dashboard
// should show. Malformed or missing lines yield the zero value — the UI then
// reports "not reported" instead of inventing a resolution.
func parseSelectedFromPrint(stdout []byte) SelectedFormat {
	var sel SelectedFormat
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, ",") {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < selectedPrintFields {
			continue
		}
		if parts[0] == "" {
			continue // a probe line, not a format report
		}
		sel = SelectedFormat{
			FormatID: parts[0],
			VCodec:   parts[4],
			ACodec:   parts[5],
			Ext:      parts[6],
		}
		sel.Width = atoiOrZero(parts[1])
		sel.Height = atoiOrZero(parts[2])
		if fps, err := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64); err == nil {
			sel.FPS = fps
		}
		if tbr, err := strconv.ParseFloat(strings.TrimSpace(parts[7]), 64); err == nil {
			sel.TBR = tbr
		}
	}
	return sel
}

// atoiOrZero is a forgiving int parse for extractor-reported numbers.
func atoiOrZero(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
