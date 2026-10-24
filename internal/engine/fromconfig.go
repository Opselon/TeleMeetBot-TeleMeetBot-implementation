package engine

import "github.com/Opselon/TeleMeetBot/internal/config"

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
		DownloadTimeoutSec: c.Download.TimeoutSeconds,
		DownloadConcurrent: c.Download.MaxConcurrent,
		DownloadYTDLP:      c.Download.YTDLPPath,
		DownloadFFmpeg:     c.Download.FFmpegPath,
		DownloadAllowXcode: c.Download.AllowTranscode,

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
