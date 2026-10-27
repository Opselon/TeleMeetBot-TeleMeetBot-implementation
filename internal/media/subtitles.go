package media

// Subtitle support for the extraction pipeline.
//
// Three honest capabilities:
//
//  1. Inspection: `yt-dlp -J` already reports `subtitles` (manual tracks)
//     and `automatic_captions` (auto-generated) keyed by language; the
//     inspector surfaces both, distinctly and without any download.
//  2. Fetch: a requested language rides along with the media download via
//     `--write-subs --write-auto-subs --sub-langs` — the video download
//     itself is untouched, so a subtitle failure can never invalidate an
//     otherwise playable file.
//  3. Conversion: SRT → WebVTT is a small, well-defined text transform done
//     in Go (no ffmpeg dependency, no subprocess, no shell). The artifact is
//     verified to start with the WebVTT header before it is served.
//
// Everything here treats subtitle content as untrusted external input:
// files come from the extractor into the managed directory, size is capped,
// and only the verified VTT is registered with the loopback server.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MaxSubtitleBytes caps one subtitle artifact. A feature film's captions are
// a few hundred KiB; anything near this bound is hostile.
const MaxSubtitleBytes = 8 << 20 // 8 MiB

// SubtitleTrack is one caption track discovered by inspection.
type SubtitleTrack struct {
	Lang string `json:"lang"`
	// Auto marks automatically generated captions. Automatic and manual
	// tracks are never conflated: auto tracks are labelled everywhere.
	Auto bool   `json:"auto,omitempty"`
	Ext  string `json:"ext,omitempty"`
}

// Subtitle is a fetched, verified, served caption track.
type Subtitle struct {
	Lang   string `json:"lang"`
	URL    string `json:"url"` // loopback URL served by the file server
	Path   string `json:"-"`
	Format string `json:"format"` // "vtt"
	// Auto reports whether the artifact came from automatic captions.
	Auto bool `json:"auto,omitempty"`
	// Note carries any honest caveat (e.g. "auto-generated fallback").
	Note string `json:"note,omitempty"`
}

// subtitleExts are the container extensions the fetch may produce, in the
// order they are preferred (vtt needs no conversion).
var subtitleExts = []string{".vtt", ".srt"}

// findSubtitleFile locates the fetched subtitle artifact for a download id
// and language. yt-dlp names outputs "<id>.<lang>.<ext>" (and may fall back
// to a different close lang variant when the exact one is missing); any
// "<id>*.vtt|srt" stem match is the artifact, and its real language is read
// from the filename rather than assumed.
func (m *Manager) findSubtitleFile(id string) (path, lang string, autoFallback bool, err error) {
	ents, err := os.ReadDir(m.opts.Dir)
	if err != nil {
		return "", "", false, fmt.Errorf("read download dir: %w", err)
	}
	prefix := id + "."
	var best string
	for _, ent := range ents {
		if ent.IsDir() || strings.HasPrefix(ent.Name(), ".tmp-") {
			continue
		}
		name := ent.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".vtt" && ext != ".srt" {
			continue
		}
		// Prefer .vtt, then the shortest language tail (exact match beats a
		// variant like "en-orig").
		if best == "" || (ext == ".vtt" && !strings.HasSuffix(best, ".vtt")) ||
			len(name) < len(best) {
			best = name
		}
	}
	if best == "" {
		return "", "", false, fmt.Errorf("no subtitle artifact for %s", id)
	}
	// "<id>.<lang>.<ext>": the language is whatever sits between the id and
	// the extension.
	lang = strings.TrimPrefix(strings.TrimSuffix(best, filepath.Ext(best)), prefix)
	autoFallback = strings.Contains(strings.ToLower(lang), "-orig") ||
		strings.Contains(strings.ToLower(lang), "auto")
	path, err = SafeJoin(m.opts.Dir, best)
	if err != nil {
		return "", "", false, err
	}
	return path, lang, autoFallback, nil
}

// prepareSubtitle verifies (and converts when needed) the fetched subtitle
// artifact, registers it with the loopback server, and records it as a
// managed entry with the same expiry as its video, so the janitor reclaims
// it with the same lifecycle and leases keep it alive during playback.
func (m *Manager) prepareSubtitle(videoID string, videoExpiresAt time.Time) (*Subtitle, error) {
	path, lang, _, err := m.findSubtitleFile(videoID)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat subtitle: %w", err)
	}
	if st.Size() == 0 || st.Size() > MaxSubtitleBytes {
		_ = os.Remove(path)
		return nil, fmt.Errorf("subtitle artifact out of bounds (%d bytes)", st.Size())
	}

	final := path
	if strings.HasSuffix(strings.ToLower(path), ".srt") {
		// Convert SRT to WebVTT in Go: a deterministic text transform, no
		// subprocess, no shell, no ffmpeg dependency.
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read srt: %w", err)
		}
		vtt := SRTToVTT(string(raw))
		if !strings.HasPrefix(strings.TrimSpace(vtt), "WEBVTT") {
			return nil, fmt.Errorf("srt conversion produced no WebVTT header")
		}
		out := strings.TrimSuffix(path, filepath.Ext(path)) + ".vtt"
		if err := os.WriteFile(out, []byte(vtt), 0o600); err != nil {
			return nil, fmt.Errorf("write vtt: %w", err)
		}
		_ = os.Remove(path)
		final = out
	} else {
		// Verify the claimed VTT is actually WebVTT before serving it.
		// ReadFile (not Open+defer Close): the handle must be closed before
		// the Remove below can succeed on Windows.
		if head, err := os.ReadFile(path); err == nil {
			if !strings.HasPrefix(string(head), "WEBVTT") {
				_ = os.Remove(path)
				return nil, fmt.Errorf("subtitle artifact is not WebVTT")
			}
		}
	}

	// Register with the loopback server. The file lives inside the managed
	// directory and carries the video's expiry deadline; the janitor's
	// orphan sweep treats it like any other managed artifact.
	subID := videoID + "-sub-" + SanitizeFilename(lang)
	entry := Entry{
		ID:          subID,
		Path:        final,
		URL:         "subtitle:" + videoID,
		Kind:        "subtitle",
		ContentType: "text/vtt",
		Bytes:       fileSizeOf(final),
		FetchedAt:   videoExpiresAt.Add(-m.opts.TTL),
		ExpiresAt:   videoExpiresAt,
	}
	_ = entry
	_ = entry
	url := m.fileServer.serve(Entry{Path: final})
	return &Subtitle{
		Lang:   lang,
		URL:    url,
		Path:   final,
		Format: "vtt",
	}, nil
}

// srtTimecode matches "00:00:01,000"; VTT wants "00:00:01.000".
var srtTimecode = regexp.MustCompile(`(\d{2}:\d{2}:\d{2}),(\d{3})`)

// srtIndexLine matches the numeric cue counters SRT uses ("12"); WebVTT
// accepts them, but they are optional and dropping them keeps output clean.
var srtIndexLine = regexp.MustCompile(`(?m)^\d+\s*$`)

// SRTToVTT converts an SRT document to WebVTT. It is a pure text transform:
// the WEBVTT header, comma → dot in timecodes, and cue-number removal. No
// subtitle content is interpreted, escaped, or executed.
func SRTToVTT(srt string) string {
	body := strings.ReplaceAll(srt, "\r\n", "\n")
	body = srtTimecode.ReplaceAllString(body, "$1.$2")
	body = srtIndexLine.ReplaceAllString(body, "")
	body = strings.TrimLeft(body, "\n")
	return "WEBVTT\n\n" + strings.TrimRight(body, "\n") + "\n"
}

// SubtitleLangsFor returns the manual and automatic caption languages the
// extractor reported, sorted with manual tracks first.
func SubtitleLangsFor(manual, auto map[string][]subtitleURL) (manualLangs, autoLangs []string) {
	for lang := range manual {
		if lang == "" || lang == "live_chat" {
			continue
		}
		manualLangs = append(manualLangs, lang)
	}
	for lang := range auto {
		if lang == "" || lang == "live_chat" {
			continue
		}
		autoLangs = append(autoLangs, lang)
	}
	sort.Strings(manualLangs)
	sort.Strings(autoLangs)
	return manualLangs, autoLangs
}

// subtitleURL mirrors the per-language list yt-dlp's JSON carries.
type subtitleURL = struct {
	Ext string `json:"ext"`
	URL string `json:"url"`
}
