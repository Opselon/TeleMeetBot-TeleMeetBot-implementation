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
