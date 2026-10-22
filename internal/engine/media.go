package engine

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
)

//go:embed assets/player.html
var assetFS embed.FS

// MediaKind classifies how the engine should open a link.
type MediaKind string

const (
	// MediaDirect is a playable media file (mp4, webm, mp3…): rendered in the
	// embedded player document.
	MediaDirect MediaKind = "direct"
	// MediaYouTube is a YouTube watch/short link: opened directly, controlled
	// through the page's own player API when possible.
	MediaYouTube MediaKind = "youtube"
	// MediaPage is any other web page the user wants visible on the share.
	MediaPage MediaKind = "page"
)

// MediaTarget is a validated media link with its classification.
type MediaTarget struct {
	URL  string
	Kind MediaKind
}

// directExts are path extensions playable by Chromium's <video>/<audio>.
var directExts = map[string]bool{
	"mp4": true, "m4v": true, "webm": true, "mkv": true, "mov": true,
	"ogv": true, "mp3": true, "m4a": true, "aac": true, "flac": true,
	"wav": true, "opus": true, "oga": true, "ogg": true, "avi": true,
}

// ClassifyMedia parses and classifies a user-provided link.
func ClassifyMedia(raw string) (MediaTarget, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return MediaTarget{}, fmt.Errorf("media link is empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return MediaTarget{}, fmt.Errorf("cannot parse media link: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return MediaTarget{}, fmt.Errorf("media link must be an http(s) URL")
	}
	host := strings.ToLower(u.Hostname())

	// YouTube: normalise to the desktop watch URL for stable control.
	if host == "youtu.be" || strings.HasSuffix(host, "youtube.com") {
		if t, ok := normalizeYouTube(host, u); ok {
			return MediaTarget{URL: t, Kind: MediaYouTube}, nil
		}
	}

	file := strings.ToLower(path.Ext(u.Path))
	if file != "" && directExts[strings.TrimPrefix(file, ".")] {
		return MediaTarget{URL: u.String(), Kind: MediaDirect}, nil
	}
	if strings.Contains(host, "googlevideo.com") || strings.Contains(u.Path, "videoplayback") {
		return MediaTarget{URL: u.String(), Kind: MediaDirect}, nil
	}
	return MediaTarget{URL: u.String(), Kind: MediaPage}, nil
}

// normalizeYouTube converts short/shorts/live links into the canonical watch
// URL (the only layout whose <video> element stays same-origin and controllable).
func normalizeYouTube(host string, u *url.URL) (string, bool) {
	parts := []string{}
	for _, p := range strings.Split(strings.Trim(u.Path, "/"), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	videoID := u.Query().Get("v")
	switch {
	case host == "youtu.be":
		if len(parts) == 1 {
			videoID = parts[0]
		}
	case strings.HasSuffix(host, "youtube.com") && len(parts) >= 2 && (parts[0] == "shorts" || parts[0] == "live"):
		videoID = parts[1]
	case strings.HasSuffix(host, "youtube.com") && (len(parts) == 0 || parts[0] != "watch"):
		return "", false // channel/playlist pages: treat as raw page
	}
	if !isYouTubeID(videoID) {
		return "", false
	}
	q := url.Values{}
	for k, v := range u.Query() {
		if k != "v" {
			q.Add(k, v[0])
		}
	}
	if q.Get("autoplay") == "" {
		q.Set("autoplay", "1")
	}
	w := "https://www.youtube.com/watch?v=" + url.PathEscape(videoID)
	if s := q.Encode(); s != "" {
		w += "&" + s
	}
	return w, true
}

// isYouTubeID accepts the 11-char YouTube video id (strict but lenient on
// envelopes around embed URLs).
func isYouTubeID(id string) bool {
	if id == "" || len(id) > 16 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// playerRow echoes "direct" without external references — internal only.

// PlayerOptions parameterise the embedded player document.
type PlayerOptions struct {
	Src      string
	TabTitle string
	Autoplay bool
	Mute     bool
	Loop     bool
	Volume   int
	Speed    float64
}

// PlayerDataURL renders the embedded player document as a data: URL. The page
// locks document.title to TabTitle because Chromium's screen-share auto-select
// matches the source by tab title.
func PlayerDataURL(t MediaTarget, o PlayerOptions) (string, error) {
	if o.TabTitle == "" {
		o.TabTitle = "TeleMeet Media"
	}
	if o.Volume == 0 {
		o.Volume = 80
	}
	if o.Speed <= 0 {
		o.Speed = 1
	}
	html, err := assetFS.ReadFile("assets/player.html")
	if err != nil {
		return "", fmt.Errorf("read player asset: %w", err)
	}
	boot, err := json.Marshal(map[string]any{
		"src":      t.URL,
		"tabTitle": o.TabTitle,
		"autoplay": o.Autoplay,
		"mute":     o.Mute,
		"loop":     o.Loop,
		"volume":   o.Volume,
		"speed":    o.Speed,
	})
	if err != nil {
		return "", err
	}
	doc := strings.Replace(string(html), "__TM_BOOTSTRAP__", string(boot), 1)
	return "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(doc)), nil
}
