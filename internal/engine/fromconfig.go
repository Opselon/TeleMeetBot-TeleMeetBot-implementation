package engine

import (
	"path/filepath"
	"strings"

	"github.com/Opselon/TeleMeetBot/internal/config"
)

// downloadProxyFor resolves the egress for media downloads: an explicit
// download.proxy wins, otherwise the operator's browser.proxy is reused. One
// setting is enough for a VPS where Chromium already needs a proxy.
func downloadProxyFor(c *config.Config) string {
	if p := strings.TrimSpace(c.Download.Proxy); p != "" {
		return p
	}
	return strings.TrimSpace(c.Browser.Proxy)
}

// FromConfig maps the application config to engine option structs.
func FromConfig(c *config.Config) (BrowserConfig, MediaOptions, MeetOptions) {
	b := BrowserConfig{
		Executable:   c.Browser.Executable,
		Headless:     c.Browser.Headless,
		PerSession:   c.Browser.PerSession,
		Display:      c.Browser.Display,
		WindowWidth:  c.Browser.WindowWidth,
		WindowHeight: c.Browser.WindowHeight,
		Lang:         c.Browser.Lang,
		ExtraArgs:    c.Browser.ExtraArgs,
		UserAgent:    c.Browser.UserAgent,
		Proxy:        c.Browser.Proxy,
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
