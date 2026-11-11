package engine

import (
	"path/filepath"
	"strings"

	"github.com/Opselon/TeleMeetBot/internal/config"
)

// downloadProxyFor resolves the egress for media downloads.
//
// DIRECT-BY-DEFAULT: an explicit download.proxy wins; when it is unset the
// download runs DIRECT rather than inheriting browser.proxy. Verified live:
// unproxied egress extracts and downloads YouTube cleanly on an ordinary
// server, while inheriting a proxy YouTube has throttled (rotating free pools
// are the textbook case) is exactly what produces "Sign in to confirm you're
// not a bot" and "TLS/SSL connection has been closed (EOF)". Coupling the
// two also meant an operator who only needed a proxy for the WATCH PAGE
// silently forced yt-dlp through the same route.
//
// An operator who wants the old coupling writes download.proxy explicitly.
// The egress ladder (direct → download.proxy → download.fallback_proxies)
// still honours every configured route.
func downloadProxyFor(c *config.Config) string {
	return strings.TrimSpace(c.Download.Proxy)
}

// mediaEgresses lists, in preference order, every egress the media pipeline
// may use: the browser proxy first (Chromium's own route, which the download
// also defaults to), then each configured download fallback. The engine uses
// it to pick a media-tab route the source has not already rejected — YouTube
// bot-blocks a datacenter IP for BOTH the innertube API and the watch page,
// so a proxy that lost once must not be handed to the media browser again.
func mediaEgresses(c *config.Config) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" && !isDirectSentinel(p) && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(c.Browser.Proxy)
	add(c.Download.Proxy)
	for _, p := range c.Download.FallbackProxies {
		add(p)
	}
	return out
}

// isDirectSentinel reports whether p is the "no proxy, ever" marker accepted
// by detectProxy (browser.proxy / download.proxy). A sentinel must never
// enter the egress ladder: it is not a route the media browser can fail over
// to, only an instruction to stay unproxied.
func isDirectSentinel(p string) bool {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "direct", "direct://":
		return true
	}
	return false
}

// downloadFallbacksFor resolves the fallback egresses for media downloads.
// An explicit download.fallback_proxies list wins; otherwise the historical
// single-proxy behaviour is kept (no fallbacks).
func downloadFallbacksFor(c *config.Config) []string {
	var out []string
	for _, p := range c.Download.FallbackProxies {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FromConfig maps the application config to engine option structs.
func FromConfig(c *config.Config) (BrowserConfig, MediaOptions, MeetOptions) {
	b := BrowserConfig{
		Executable:   c.Browser.Executable,
		Headless:     c.Browser.Headless,
		PerSession:   c.Browser.PerSession,
		ProfileDir:   c.Browser.ProfileDir,
		Display:      c.Browser.Display,
		WindowWidth:  c.Browser.WindowWidth,
		WindowHeight: c.Browser.WindowHeight,
		Lang:         c.Browser.Lang,
		ExtraArgs:    c.Browser.ExtraArgs,
		UserAgent:    c.Browser.UserAgent,
		Proxy:        c.Browser.Proxy,
		MeetViaProxy: c.Browser.MeetViaProxy,
		GPU:          c.Browser.GPU,
		WarmUp:       c.Browser.WarmUp,
	}
	// Persistent cache root: <data_dir>/browser-cache. Explicit
	// disk_cache:false disables it entirely.
	if c.Browser.DiskCache == nil || *c.Browser.DiskCache {
		b.CacheDir = filepath.Join(c.Paths.DataDir, "browser-cache")
	}
	m := MediaOptions{
		DefaultVolume:      c.Media.DefaultVolume,
		DefaultSpeed:       c.Media.DefaultSpeed,
		Autoplay:           c.Media.Autoplay,
		MuteUntilConnected: c.Media.MuteUntilConnected,
		SeekStepSeconds:    c.Media.SeekStepSeconds,
		DefaultAudioLang:   c.Media.DefaultAudioLang,
		SkipYouTubeAds:     c.Media.SkipYouTubeAds,

		DownloadEnabled:    c.Download.Enabled,
		DownloadDir:        c.Download.Dir,
		DownloadTTLMinutes: c.Download.TTLMinutes,
		DownloadMaxSizeMB:  c.Download.MaxSizeMB,
		DownloadMinFreeMB:  c.Download.MinFreeMB,
		DownloadTimeoutSec: c.Download.TimeoutSeconds,
		DownloadConcurrent: c.Download.MaxConcurrent,
		DownloadYTDLP:      c.Download.YTDLPPath,
		DownloadFFmpeg:     c.Download.FFmpegPath,
		DownloadAllowXcode: c.Download.AllowTranscode,
		DownloadProxy:      downloadProxyFor(c),
		DownloadFallbacks:  downloadFallbacksFor(c),
		// The authenticated cookie jar for YouTube's account gate. See
		// DownloadConfig.CookiesFile: it is the only ladder-independent
		// answer to "Sign in to confirm you're not a bot".
		DownloadCookies: c.Download.CookiesFile,
		MediaEgresses:   mediaEgresses(c),

		// The anchor for a relative download dir, so an operator writing
		// "downloads" gets <data_dir>/downloads rather than a CWD-relative
		// path (a service may start from anywhere).
		DownloadBaseDir: c.Paths.DataDir,
	}
	meet := MeetOptions{
		GuestName:             c.Meet.GuestName,
		PresentMode:           c.Meet.PresentMode,
		CaptureSource:         c.Meet.CaptureSource,
		JoinTimeoutSeconds:    c.Meet.JoinTimeoutSeconds,
		PresentTimeoutSeconds: c.Meet.PresentTimeoutSeconds,
		AdmitWaitSeconds:      c.Meet.AdmitWaitSeconds,
	}
	return b, m, meet
}
