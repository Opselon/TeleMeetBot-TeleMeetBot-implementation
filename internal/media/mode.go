// Package media implements the Download & Stream side of TeleMeetBot: fetching
// remote media to local disk (plain HTTP or yt-dlp), guarding the download
// surface (SSRF, size, time, concurrency), and enforcing a mandatory expiry
// policy so a downloaded file can never outlive its configured lifetime.
//
// The package is self-contained: the engine wires a Manager in and asks it to
// resolve a URL into a playable local file; everything else (validation,
// subprocess invocation, janitor work, manifest persistence) happens here.
package media

import "strings"

// Mode selects how a session should put media on the share.
type Mode string

const (
	// ModeBrowser is the original behaviour: the media tab loads the remote
	// URL directly and the network keeps feeding Chromium.
	ModeBrowser Mode = "browser"
	// ModeDownload downloads supported media to disk first, then the media
	// tab plays the local file through the loopback player.
	ModeDownload Mode = "download"
	// ModeAuto prefers browser playback and falls back to downloading when
	// the browser pipeline fails at the media stage.
	ModeAuto Mode = "auto"
)

// DefaultMode is used when no preference is configured.
const DefaultMode = ModeBrowser

// NormalizeMode parses and validates a mode, falling back to DefaultMode.
// Unknown or empty input is never an error: the caller gets a usable mode.
func NormalizeMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "browser", "direct", "play", "off", "none", "no":
		return ModeBrowser
	case "download", "download-stream", "download_stream", "downloadandstream", "local":
		return ModeDownload
	case "auto", "fallback", "smart":
		return ModeAuto
	default:
		return DefaultMode
	}
}

// String is the canonical wire representation.
func (m Mode) String() string { return string(m) }

// Display is the human label shown on Telegram cards and in logs.
func (m Mode) Display() string {
	switch NormalizeMode(string(m)) {
	case ModeDownload:
		return "Download & Stream"
	case ModeAuto:
		return "Auto"
	default:
		return "Browser"
	}
}

// Emoji is the dashboard icon for the mode.
func (m Mode) Emoji() string {
	switch NormalizeMode(string(m)) {
	case ModeDownload:
		return "⬇️"
	case ModeAuto:
		return "🤖"
	default:
		return "🌐"
	}
}

// Modes lists every selectable mode in dashboard order.
func Modes() []Mode { return []Mode{ModeBrowser, ModeDownload, ModeAuto} }

// IsDownloadable reports whether a URL classification is a candidate for
// downloading: direct media files and YouTube links are, arbitrary web pages
// are not (there is nothing to fetch and replay as media).
// kind follows the engine MediaKind string ("direct" | "youtube" | "page").
func IsDownloadable(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "direct", "youtube":
		return true
	default:
		return false
	}
}
