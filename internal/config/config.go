// Package config loads, validates and persists the TeleMeetBot configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/Opselon/TeleMeetBot/internal/localbotapi"

	"gopkg.in/yaml.v3"
)

// Version is the application version, injected at build time (see Makefile).
var Version = "dev"

// Config is the full on-disk configuration (YAML).
type Config struct {
	Telegram TelegramConfig `yaml:"telegram"`
	Server   ServerConfig   `yaml:"server"`
	Browser  BrowserConfig  `yaml:"browser"`
	Media    MediaConfig    `yaml:"media"`
	Meet     MeetConfig     `yaml:"meet"`
	Limits   LimitsConfig   `yaml:"limits"`
	Logging  LoggingConfig  `yaml:"logging"`
	Paths    PathsConfig    `yaml:"paths"`
	Download DownloadConfig `yaml:"download"`
	Google   GoogleConfig   `yaml:"google"`

	// path is where the config was loaded from / should be saved to.
	path string
	// mu guards the few fields the running bot mutates (the admin
	// allow-list). It is unexported and therefore invisible to YAML.
	mu sync.RWMutex
}

// DownloadConfig controls the Download & Stream feature: fetching media to
// local disk with yt-dlp or plain HTTP, and the mandatory expiry policy.
type DownloadConfig struct {
	// Enabled turns the feature on. Browser mode always works, with or
	// without this.
	Enabled bool `yaml:"enabled"`
	// Dir is the managed download directory. Only files inside it are ever
	// created or deleted. Empty defaults to <data_dir>/downloads.
	Dir string `yaml:"dir"`
	// TTLMinutes is the orphan-cleanup lifetime of a downloaded file,
	// counted from the moment the download completes. Default 60 (one
	// hour). It does not cap playback: a file an active session is
	// streaming is held for the lifetime of that session (any duration,
	// any pause) and is only freed when the session stops, switches media
	// or fails. The TTL reclaims files whose owning session is gone.
	TTLMinutes int `yaml:"ttl_minutes"`
	// MaxSizeMB is the per-file ceiling. Default 2048 (2 GiB).
	MaxSizeMB int `yaml:"max_size_mb"`
	// MinFreeMB is the minimum free disk space required on Dir before
	// allowing a download. Default 256 (256 MiB).
	MinFreeMB int `yaml:"min_free_mb"`
	// TimeoutSeconds bounds one whole download, transcode included.
	// Default 600 (10 min).
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// MaxConcurrent bounds parallel downloads. Default 1.
	MaxConcurrent int `yaml:"max_concurrent"`
	// YTDLPPath overrides the yt-dlp binary location; empty auto-detects.
	YTDLPPath string `yaml:"ytdlp_path"`
	// FFmpegPath overrides the ffmpeg binary location; empty auto-detects.
	FFmpegPath string `yaml:"ffmpeg_path"`
	// AllowTranscode permits ffmpeg to remux containers Chromium cannot
	// decode (mkv/avi/ogv) into mp4.
	AllowTranscode bool `yaml:"allow_transcode"`
	// Proxy is handed to yt-dlp (and the plain-HTTP fetcher). YouTube
	// bot-blocks datacenter egress, so a VPS download without a proxy fails
	// with "Sign in to confirm you're not a bot". Empty means a direct
	// connection. Defaults to browser.proxy when unset, since that is the
	// egress the operator already had to configure for Chromium.
	Proxy string `yaml:"proxy"`
}

// TelegramConfig holds bot token and access control.
type TelegramConfig struct {
	Token string `yaml:"token"`
	// APIEndpoint allows using a local Telegram Bot API server (e.g. "http://localhost:8081/bot%s/%s")
	// for handling files up to 2000 MiB. Empty uses the official cloud endpoint.
	APIEndpoint string `yaml:"api_endpoint"`
	// MaxFileSizeMB overrides the maximum allowed file size for Telegram attachments (in MiB).
	// Default is 20 MiB for official cloud Telegram Bot API, up to 2000 MiB for local Bot API servers.
	MaxFileSizeMB int `yaml:"max_file_size_mb"`
	// Admins is the allow-list of Telegram user IDs. Access is fail-closed:
	// an empty list means nobody can control the bot (except when
	// FirstStartClaimsAdmin is enabled and no admin has claimed the bot yet).
	Admins []int64 `yaml:"admins"`
	// FirstStartClaimsAdmin lets the very first /start claim the bot and become
	// the sole admin. Useful for a fresh install: start the bot, press Start,
	// done. Turn this off in hardened deployments.
	FirstStartClaimsAdmin bool `yaml:"first_start_claims_admin"`
	// NotifyChatID receives lifecycle notifications (started, failed, ended).
	// Zero means "notify the admin who created the session".
	NotifyChatID int64 `yaml:"notify_chat_id"`
	// PollTimeoutSeconds is the long-poll timeout used for getUpdates.
	PollTimeoutSeconds int `yaml:"poll_timeout_seconds"`
	// LocalBotAPI configures the embedded or supervised local telegram-bot-api server.
	LocalBotAPI localbotapi.Config `yaml:"local_bot_api"`
}

// ServerConfig is the local HTTP surface (health checks only, no remote login).
type ServerConfig struct {
	HealthAddr string `yaml:"health_addr"`
}

// BrowserConfig controls the embedded Chromium instance.
type BrowserConfig struct {
	// Executable is an explicit chrome/chromium path. Empty means auto-detect,
	// falling back to rod's managed download.
	Executable string `yaml:"executable"`
	Headless   bool   `yaml:"headless"`
	// PerSession launches one browser per meeting (isolation, more RAM).
	// When false a single browser is shared, one tab per role.
	PerSession   bool     `yaml:"per_session"`
	Display      string   `yaml:"display"` // e.g. ":99" when headless=false
	WindowWidth  int      `yaml:"window_width"`
	WindowHeight int      `yaml:"window_height"`
	Lang         string   `yaml:"lang"`
	ExtraArgs    []string `yaml:"extra_args"`
	// UserAgent overrides the headless UA; empty uses a stock Chrome UA.
	UserAgent string `yaml:"user_agent"`
	// Proxy is passed to Chromium, e.g. "socks5://127.0.0.1:9050".
	Proxy string `yaml:"proxy"`
	// GPU selects the compositing backend: "off" (software compositing, the
	// default in headless — measured ~6x less host CPU than SwiftShader for the
	// same shared frames on a 2-core VPS), "swiftshader" (ANGLE/SwiftShader
	// software GL, the pre-1.2 default) or "auto" (let Chromium decide; use on
	// hosts with a real GPU). Empty keeps the default.
	GPU string `yaml:"gpu"`
	// DiskCache enables a persistent Chromium HTTP cache under
	// <data_dir>/browser-cache, so repeated sessions reuse static Meet/YouTube
	// resources across restarts. Only HTTP cache data is stored — never
	// cookies, sessions or meeting state. Default true.
	DiskCache *bool `yaml:"disk_cache"`
	// WarmUp pre-launches one idle Chromium at startup so the first session
	// skips the cold browser start (saves ~1-3s). Default false.
	WarmUp bool `yaml:"warm_up"`
}

// MediaConfig controls playback inside the shared tab.
type MediaConfig struct {
	DefaultVolume      int     `yaml:"default_volume"`
	DefaultSpeed       float64 `yaml:"default_speed"`
	Autoplay           bool    `yaml:"autoplay"`
	MuteUntilConnected bool    `yaml:"mute_until_connected"`
	SeekStepSeconds    int     `yaml:"seek_step_seconds"`
	Loop               bool    `yaml:"loop"`
	DefaultAudioLang   string  `yaml:"default_audio_lang"`
	// SkipYouTubeAds presses YouTube's own "Skip ads" button the moment it
	// becomes available. It never blocks or hides ads: an ad without a visible,
	// enabled skip button plays out in full, and non-YouTube media is not
	// touched. Default true.
	SkipYouTubeAds *bool `yaml:"skip_youtube_ads"`
}

// MeetConfig controls Google Meet join and screen-share behaviour.
type MeetConfig struct {
	// GuestName is the display name used when joining as a guest.
	GuestName string `yaml:"guest_name"`
	// PresentMode is "tab" (share the media tab, works headless) or "screen"
	// (share the whole virtual display, needs Xvfb).
	PresentMode string `yaml:"present_mode"`
	// CaptureSource must match the auto-select desktop-capture-source value:
	// for "tab" it is the media tab title, for "screen" it is e.g.
	// "Entire screen (monitor 1)".
	CaptureSource         string `yaml:"capture_source"`
	JoinTimeoutSeconds    int    `yaml:"join_timeout_seconds"`
	PresentTimeoutSeconds int    `yaml:"present_timeout_seconds"`
	// AdmitWaitSeconds is how long to wait in the "ask to join" lobby before
	// reporting an error (0 waits forever).
	AdmitWaitSeconds int `yaml:"admit_wait_seconds"`
}

// LimitsConfig caps resource usage.
type LimitsConfig struct {
	MaxSessions int `yaml:"max_sessions"`
}

// LoggingConfig controls the logger.
type LoggingConfig struct {
	Level string `yaml:"level"` // debug, info, warn, error
	File  string `yaml:"file"`
}

// GoogleConfig controls the Google OAuth 2.0 / OpenID Connect integration.
type GoogleConfig struct {
	// Enabled toggles the Google OAuth integration. When ClientID is provided,
	// Enabled is automatically considered true.
	Enabled bool `yaml:"enabled"`
	// ClientID is the OAuth 2.0 Client ID from Google Cloud Console.
	ClientID string `yaml:"client_id"`
	// ClientSecret is the OAuth 2.0 Client Secret from Google Cloud Console.
	ClientSecret string `yaml:"client_secret"`
	// RedirectURL is the authorized redirect URI registered in Google Cloud Console
	// (e.g. "https://example.com/auth/google/callback" or "http://127.0.0.1:18080/auth/google/callback").
	RedirectURL string `yaml:"redirect_url"`
	// TokenEncryptionKey is a 32-byte hex-encoded key for AES-256-GCM token encryption at rest.
	// If empty, an encryption key is securely generated and saved in <data_dir>/secrets/oauth_cipher.key.
	TokenEncryptionKey string `yaml:"token_encryption_key"`
	// Scopes specifies the OAuth scopes. Defaults to ["openid", "email", "profile"].
	Scopes []string `yaml:"scopes"`
}

// PathsConfig holds writable locations.
type PathsConfig struct {
	DataDir string `yaml:"data_dir"`
}

// Default returns a fully populated, sane configuration.
func Default() *Config {
	return &Config{
		Telegram: TelegramConfig{
			FirstStartClaimsAdmin: true,
			PollTimeoutSeconds:    30,
		},
		Server: ServerConfig{HealthAddr: "127.0.0.1:18080"},
		Browser: BrowserConfig{
			Headless:     true,
			PerSession:   true,
			Display:      ":99",
			WindowWidth:  1280,
			WindowHeight: 800,
			Lang:         "en-US",
		},
		Media: MediaConfig{
			DefaultVolume:      80,
			DefaultSpeed:       1.0,
			Autoplay:           true,
			MuteUntilConnected: true,
			SeekStepSeconds:    10,
			DefaultAudioLang:   "fa",
		},
		Meet: MeetConfig{
			GuestName:             "TeleMeet Bot",
			PresentMode:           "tab",
			CaptureSource:         "TeleMeet Media",
			JoinTimeoutSeconds:    120,
			PresentTimeoutSeconds: 45,
			AdmitWaitSeconds:      300,
		},
		Limits: LimitsConfig{MaxSessions: 2},
		Download: DownloadConfig{
			Enabled:        true,
			TTLMinutes:     60,
			MaxSizeMB:      2048,
			MinFreeMB:      256,
			TimeoutSeconds: 600,
			MaxConcurrent:  1,
			AllowTranscode: true,
		},
		Logging: LoggingConfig{Level: "info"},
		Paths:   PathsConfig{DataDir: "data"},
		Google: GoogleConfig{
			Scopes: []string{
				"openid",
				"https://www.googleapis.com/auth/userinfo.email",
				"https://www.googleapis.com/auth/userinfo.profile",
			},
		},
	}
}

// ResolvePath picks the config file: explicit flag, env var, ./telemeetbot.yml,
// the platform's system config, in that order. The first existing candidate
// wins; otherwise the first candidate is returned so the wizard can create it.
func ResolvePath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := os.Getenv("TELEMEETBOT_CONFIG"); p != "" {
		return p
	}
	candidates := []string{"telemeetbot.yml", filepath.Join(SystemConfigDir(), "config.yml")}
	if runtime.GOOS == "windows" {
		// A per-user config wins over the machine-wide one when both exist:
		// the wizard writes LOCALAPPDATA for a non-admin Windows install.
		candidates = []string{"telemeetbot.yml"}
		if local := os.Getenv("LOCALAPPDATA"); strings.TrimSpace(local) != "" {
			candidates = append(candidates, filepath.Join(local, "TeleMeetBot", "config.yml"))
		}
		candidates = append(candidates, filepath.Join(SystemConfigDir(), "config.yml"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs
			}
			return c
		}
	}
	abs, err := filepath.Abs(candidates[0])
	if err != nil {
		return candidates[0]
	}
	return abs
}

// Load reads the YAML file at path, starting from Default() so omitted keys keep
// their defaults.
func Load(path string) (*Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.path = path
	cfg.normalize()
	return cfg, nil
}

// Exists reports whether a config file is present at path.
func Exists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// Backup copies the config at path to "<path>.bak" (overwriting any previous
// backup) so a wizard re-run or an upgrade can be rolled back by hand. It is
// best-effort: a missing file or an unwritable directory returns an error the
// caller may warn about, never one that aborts an install.
func Backup(path string) (string, error) {
	if path == "" {
		return "", errors.New("no config path given")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read config for backup: %w", err)
	}
	dst := path + ".bak"
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return "", fmt.Errorf("write backup %s: %w", dst, err)
	}
	return dst, nil
}

// RestoreBackup moves the .bak file created by Backup back over the config.
// It is the rollback half of the pair; a missing backup is reported as an
// error rather than silently clearing the live config.
func RestoreBackup(path string) error {
	if path == "" {
		return errors.New("no config path given")
	}
	bak := path + ".bak"
	raw, err := os.ReadFile(bak)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", bak, err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("restore %s: %w", path, err)
	}
	return nil
}

// Path returns the file the config was loaded from.
func (c *Config) Path() string { return c.path }

// normalize fills derived/legacy fields.
func (c *Config) normalize() {
	if strings.TrimSpace(c.Browser.Lang) == "" {
		c.Browser.Lang = "en-US"
	}
	if c.Browser.WindowWidth <= 0 {
		c.Browser.WindowWidth = 1280
	}
	if c.Browser.WindowHeight <= 0 {
		c.Browser.WindowHeight = 800
	}
	if c.Media.SeekStepSeconds <= 0 {
		c.Media.SeekStepSeconds = 10
	}
	if c.Media.DefaultSpeed <= 0 {
		c.Media.DefaultSpeed = 1.0
	}
	if strings.TrimSpace(c.Media.DefaultAudioLang) == "" {
		c.Media.DefaultAudioLang = "fa"
	}
	if c.Telegram.PollTimeoutSeconds <= 0 {
		c.Telegram.PollTimeoutSeconds = 30
	}
	c.Telegram.LocalBotAPI.EnvOverrides()
	if c.Telegram.LocalBotAPI.Enabled {
		if c.Telegram.LocalBotAPI.HTTPPort <= 0 {
			c.Telegram.LocalBotAPI.HTTPPort = 8081
		}
		if c.Telegram.LocalBotAPI.APIID <= 0 {
			c.Telegram.LocalBotAPI.APIID = 17349
		}
		if c.Telegram.LocalBotAPI.APIHash == "" {
			c.Telegram.LocalBotAPI.APIHash = "344583e45741c457fe1862106095a5eb"
		}
		if strings.TrimSpace(c.Telegram.LocalBotAPI.Dir) == "" {
			dataDir := c.Paths.DataDir
			if dataDir == "" {
				dataDir = os.TempDir()
			}
			c.Telegram.LocalBotAPI.Dir = filepath.Join(dataDir, "telegram-bot-api")
		}
		if strings.TrimSpace(c.Telegram.LocalBotAPI.TempDir) == "" {
			c.Telegram.LocalBotAPI.TempDir = filepath.Join(os.TempDir(), "telegram-bot-api")
		}
		if strings.TrimSpace(c.Telegram.APIEndpoint) == "" {
			c.Telegram.APIEndpoint = fmt.Sprintf("http://127.0.0.1:%d/bot%%s/%%s", c.Telegram.LocalBotAPI.HTTPPort)
		}
		if c.Telegram.MaxFileSizeMB <= 0 || c.Telegram.MaxFileSizeMB == 20 {
			c.Telegram.MaxFileSizeMB = 2000
		}
	}
	if strings.TrimSpace(c.Meet.PresentMode) == "" {
		c.Meet.PresentMode = "tab"
	}
	if strings.TrimSpace(c.Meet.CaptureSource) == "" {
		if c.Meet.PresentMode == "screen" {
			c.Meet.CaptureSource = "Entire screen (monitor 1)"
		} else {
			c.Meet.CaptureSource = "TeleMeet Media"
		}
	}
	if c.Limits.MaxSessions <= 0 {
		c.Limits.MaxSessions = 1
	}
	if c.Download.TTLMinutes < 0 {
		c.Download.TTLMinutes = 0
	}
	if c.Download.TimeoutSeconds < 0 {
		c.Download.TimeoutSeconds = 0
	}
	if c.Download.MaxConcurrent < 0 {
		c.Download.MaxConcurrent = 0
	}
	if c.Download.MaxSizeMB < 0 {
		c.Download.MaxSizeMB = 0
	}
	if tok := os.Getenv("TELEGRAM_TOKEN"); tok != "" && c.Telegram.Token == "" {
		c.Telegram.Token = tok
	} else if tok := os.Getenv("TELEMEETBOT_TOKEN"); tok != "" && c.Telegram.Token == "" {
		c.Telegram.Token = tok
	}
	if admins := os.Getenv("TELEGRAM_ADMINS"); admins != "" && len(c.Telegram.Admins) == 0 {
		for _, part := range strings.Split(admins, ",") {
			part = strings.TrimSpace(part)
			if id, err := strconv.ParseInt(part, 10, 64); err == nil && id > 0 {
				c.Telegram.Admins = append(c.Telegram.Admins, id)
			}
		}
	}
	if nchat := os.Getenv("TELEGRAM_NOTIFY_CHAT_ID"); nchat != "" && c.Telegram.NotifyChatID == 0 {
		if id, err := strconv.ParseInt(strings.TrimSpace(nchat), 10, 64); err == nil {
			c.Telegram.NotifyChatID = id
		}
	}
	if pxy := os.Getenv("TELEMEETBOT_PROXY"); pxy != "" {
		c.Browser.Proxy = pxy
	} else if pxy := os.Getenv("BROWSER_PROXY"); pxy != "" && c.Browser.Proxy == "" {
		c.Browser.Proxy = pxy
	}
	if cid := os.Getenv("TELEMEETBOT_GOOGLE_CLIENT_ID"); cid != "" {
		c.Google.ClientID = cid
	}
	if csec := os.Getenv("TELEMEETBOT_GOOGLE_CLIENT_SECRET"); csec != "" {
		c.Google.ClientSecret = csec
	}
	if rurl := os.Getenv("TELEMEETBOT_GOOGLE_REDIRECT_URL"); rurl != "" {
		c.Google.RedirectURL = rurl
	}
	if tkey := os.Getenv("TELEMEETBOT_GOOGLE_TOKEN_ENCRYPTION_KEY"); tkey != "" {
		c.Google.TokenEncryptionKey = tkey
	}
	if strings.TrimSpace(c.Google.ClientID) != "" && !c.Google.Enabled {
		c.Google.Enabled = true
	}
	if len(c.Google.Scopes) == 0 {
		c.Google.Scopes = []string{
			"openid",
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
		}
	}
}

// Save writes the config back to disk (0600, atomic replace).
func (c *Config) Save(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked(path)
}

// saveLocked is Save without the mutex.
func (c *Config) saveLocked(path string) error {
	if path == "" {
		path = c.path
	}
	if path == "" {
		return errors.New("no config path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	c.path = path
	return os.Chmod(path, 0o600)
}

// Validate checks invariants that must hold before the bot starts.
func (c *Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.Telegram.Token) == "" {
		problems = append(problems, "telegram.token is empty")
	} else if !strings.Contains(c.Telegram.Token, ":") {
		problems = append(problems, "telegram.token does not look like <bot_id>:<secret>")
	}
	if len(c.Telegram.Admins) == 0 && !c.Telegram.FirstStartClaimsAdmin {
		problems = append(problems, "telegram.admins is empty and first_start_claims_admin is disabled: nobody could control the bot")
	}
	for _, id := range c.Telegram.Admins {
		if id <= 0 {
			problems = append(problems, "telegram.admins contains a non-positive id")
			break
		}
	}
	switch c.Meet.PresentMode {
	case "tab", "screen":
	default:
		problems = append(problems, `meet.present_mode must be "tab" or "screen"`)
	}
	switch strings.ToLower(strings.TrimSpace(c.Browser.GPU)) {
	case "", "off", "disable", "disabled", "none", "swiftshader", "angle", "swangle", "compatible", "meet", "meet-safe", "webgl", "auto", "default", "on", "hardware":
	default:
		problems = append(problems, `browser.gpu must be "off", "compatible", "swiftshader" or "auto"`)
	}
	if strings.TrimSpace(c.Meet.CaptureSource) == "" {
		problems = append(problems, "meet.capture_source is empty")
	}
	if c.Meet.PresentMode == "tab" && strings.Contains(strings.ToLower(c.Meet.CaptureSource), "entire screen") {
		problems = append(problems, `meet.capture_source looks like a screen source but present_mode is "tab"`)
	}
	if c.Media.DefaultVolume < 0 || c.Media.DefaultVolume > 100 {
		problems = append(problems, "media.default_volume must be within 0..100")
	}
	if c.Media.DefaultSpeed < 0.25 || c.Media.DefaultSpeed > 4 {
		problems = append(problems, "media.default_speed must be within 0.25..4")
	}
	if c.Limits.MaxSessions < 1 || c.Limits.MaxSessions > 8 {
		problems = append(problems, "limits.max_sessions must be within 1..8")
	}
	if strings.TrimSpace(c.Paths.DataDir) == "" {
		problems = append(problems, "paths.data_dir is empty")
	}
	if c.Download.Enabled {
		switch {
		case c.Download.TTLMinutes == 0:
			// 0 keeps the built-in default of 60; only negative is an error.
		case c.Download.TTLMinutes < 1:
			problems = append(problems, "download.ttl_minutes must be positive")
		case c.Download.TTLMinutes > 1440:
			problems = append(problems, "download.ttl_minutes must be at most 1440 (24h); the default policy is 60")
		}
		if c.Download.TimeoutSeconds == 0 || c.Download.TimeoutSeconds > 3600 {
			problems = append(problems, "download.timeout_seconds must be within 1..3600")
		}
		if c.Download.MaxSizeMB != 0 && (c.Download.MaxSizeMB < 1 || c.Download.MaxSizeMB > 8192) {
			problems = append(problems, "download.max_size_mb must be within 1..8192 (0 keeps the 2048 default)")
		}
		if c.Download.MaxConcurrent < 0 || c.Download.MaxConcurrent > 8 {
			problems = append(problems, "download.max_concurrent must be within 0..8")
		}
		if p := strings.TrimSpace(c.Download.Proxy); p != "" {
			u, err := url.Parse(p)
			switch {
			case err != nil || u.Host == "":
				problems = append(problems, "download.proxy is not a valid proxy URL (want scheme://host:port)")
			case u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h":
				problems = append(problems, "download.proxy scheme must be http, https, socks5 or socks5h")
			}
		}
		switch strings.ToLower(strings.TrimSpace(c.Download.Dir)) {
		case "/", "/etc", "/usr", "/bin", "/sbin", "/var", "/root", "/home":
			problems = append(problems, "download.dir must not be a system directory")
		}
	}
	if c.Google.Enabled {
		if strings.TrimSpace(c.Google.ClientID) == "" {
			problems = append(problems, "google.client_id is required when google auth is enabled")
		}
		if strings.TrimSpace(c.Google.ClientSecret) == "" {
			problems = append(problems, "google.client_secret is required when google auth is enabled")
		}
		if strings.TrimSpace(c.Google.RedirectURL) == "" {
			problems = append(problems, "google.redirect_url is required when google auth is enabled")
		} else {
			u, err := url.Parse(c.Google.RedirectURL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				problems = append(problems, "google.redirect_url must be a valid http or https URL")
			}
		}
	}
	if c.Telegram.LocalBotAPI.Enabled {
		if c.Telegram.LocalBotAPI.APIID <= 0 {
			problems = append(problems, "telegram.local_bot_api.api_id must be positive")
		}
		if strings.TrimSpace(c.Telegram.LocalBotAPI.APIHash) == "" {
			problems = append(problems, "telegram.local_bot_api.api_hash must not be empty")
		}
		if c.Telegram.LocalBotAPI.HTTPPort < 1024 || c.Telegram.LocalBotAPI.HTTPPort > 65535 {
			problems = append(problems, "telegram.local_bot_api.http_port must be within 1024..65535")
		}
	}
	if c.Telegram.MaxFileSizeMB < 0 || c.Telegram.MaxFileSizeMB > 4000 {
		problems = append(problems, "telegram.max_file_size_mb must be within 0..4000")
	}
	if len(problems) > 0 {
		return errors.New("invalid config: " + strings.Join(problems, "; "))
	}
	return nil
}

// IsAdmin reports whether a Telegram user may control the bot.
func (c *Config) IsAdmin(userID int64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, id := range c.Telegram.Admins {
		if id == userID {
			return true
		}
	}
	return false
}

// AdminIDs returns a copy of the configured admin allow-list.
func (c *Config) AdminIDs() []int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]int64, len(c.Telegram.Admins))
	copy(out, c.Telegram.Admins)
	return out
}

// ClaimAdmin records userID as the only admin (first /start flow).
// It returns true when the claim happened.
func (c *Config) ClaimAdmin(userID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Telegram.Admins) > 0 || !c.Telegram.FirstStartClaimsAdmin || userID <= 0 {
		return false
	}
	c.Telegram.Admins = []int64{userID}
	return true
}

// StateFile returns the persisted state file path.
func (c *Config) StateFile() string {
	return filepath.Join(c.Paths.DataDir, "state.json")
}

// MeetingsFile returns the SQLite database path for saved meetings.
func (c *Config) MeetingsFile() string {
	return filepath.Join(c.Paths.DataDir, "meetings.db")
}

// PlaylistsFile returns the SQLite database path for user playlists. It is a
// separate database from meetings.db on purpose: a damaged playlist library
// can be reset without touching saved meeting links.
func (c *Config) PlaylistsFile() string {
	return filepath.Join(c.Paths.DataDir, "playlists.db")
}

// AddAdmin appends a Telegram user id to the admin allow-list. It returns
// false when the id is invalid or already present.
func (c *Config) AddAdmin(userID int64) bool {
	if userID <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range c.Telegram.Admins {
		if id == userID {
			return false
		}
	}
	c.Telegram.Admins = append(c.Telegram.Admins, userID)
	return true
}

// RemoveAdmin drops a Telegram user id from the allow-list. The last admin
// cannot be removed: that would lock every operator out of the bot.
func (c *Config) RemoveAdmin(userID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Telegram.Admins) <= 1 {
		return false
	}
	for i, id := range c.Telegram.Admins {
		if id != userID {
			continue
		}
		c.Telegram.Admins = append(c.Telegram.Admins[:i], c.Telegram.Admins[i+1:]...)
		return true
	}
	return false
}

// TokenPreview redacts the bot token for logs and UI.
func (c *Config) TokenPreview() string {
	t := strings.TrimSpace(c.Telegram.Token)
	if t == "" {
		return "(unset)"
	}
	if i := strings.Index(t, ":"); i > 0 {
		return t[:i] + ":***"
	}
	return "***"
}
