package setup

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// Profile represents the installation profile selected by the operator.
type Profile string

const (
	// ProfileFull installs core browser streaming AND all media tools (yt-dlp, ffmpeg, deno)
	// for full Download & Stream capabilities.
	ProfileFull Profile = "full"

	// ProfileMinimal installs core browser streaming only (direct Google Meet browser playback).
	// Media download features stay disabled or degraded.
	ProfileMinimal Profile = "minimal"
)

// DependencyID is the unique identifier for a runtime tool.
type DependencyID string

const (
	DepChromium DependencyID = "chromium"
	DepYTDLP    DependencyID = "yt-dlp"
	DepFFmpeg   DependencyID = "ffmpeg"
	DepDeno     DependencyID = "deno"
)

// DependencyInfo reports detection and installation metadata for a dependency.
type DependencyInfo struct {
	ID                  DependencyID
	Name                string
	Description         string
	Category            string // "core" or "media"
	RequiredForFull     bool
	RequiredForMinimal  bool
	Found               bool
	Path                string
	Version             string
	InstallCommand      []string
	FallbackHint        string
	VerificationWarning string
}

// PackageManager represents the discovered system package manager.
type PackageManager struct {
	Name string
	Path string
}

// DetectPackageManager finds a supported system package manager on the current platform.
func DetectPackageManager() *PackageManager {
	switch runtime.GOOS {
	case "windows":
		if p, err := exec.LookPath("winget"); err == nil {
			return &PackageManager{Name: "winget", Path: p}
		}
	case "darwin":
		if p, err := exec.LookPath("brew"); err == nil {
			return &PackageManager{Name: "brew", Path: p}
		}
	default: // linux / bsd
		for _, mgr := range []string{"apt-get", "dnf", "pacman", "zypper", "apk"} {
			if p, err := exec.LookPath(mgr); err == nil {
				return &PackageManager{Name: mgr, Path: p}
			}
		}
	}
	return nil
}

// CheckAllDependencies inspects all runtime dependencies against the config and environment.
func CheckAllDependencies(cfg *config.Config, log engine.Logger) []DependencyInfo {
	deps := []DependencyID{DepChromium, DepYTDLP, DepFFmpeg, DepDeno}
	out := make([]DependencyInfo, 0, len(deps))
	for _, id := range deps {
		out = append(out, CheckDependency(id, cfg, log))
	}
	return out
}

// CheckDependency inspects a single dependency.
func CheckDependency(id DependencyID, cfg *config.Config, log engine.Logger) DependencyInfo {
	switch id {
	case DepChromium:
		return checkChromium(cfg, log)
	case DepYTDLP:
		return checkYTDLP(cfg)
	case DepFFmpeg:
		return checkFFmpeg(cfg)
	case DepDeno:
		return checkDeno()
	default:
		return DependencyInfo{ID: id, Name: string(id)}
	}
}

func checkChromium(cfg *config.Config, log engine.Logger) DependencyInfo {
	info := DependencyInfo{
		ID:                 DepChromium,
		Name:               "Chromium / Chrome",
		Description:        "Required for Google Meet WebRTC capture and presentation",
		Category:           "core",
		RequiredForFull:    true,
		RequiredForMinimal: true,
	}

	bcfg, _, _ := engine.FromConfig(cfg)
	bin, err := engine.ResolveBrowser(bcfg, log)
	if err == nil && bin != "" {
		info.Found = true
		info.Path = bin
		info.Version = getToolVersion(bin, "--version")
	}

	pm := DetectPackageManager()
	switch runtime.GOOS {
	case "windows":
		if pm != nil && pm.Name == "winget" {
			info.InstallCommand = []string{"winget", "install", "--id", "Chromium.Chromium", "--accept-source-agreements", "--accept-package-agreements"}
		}
		info.FallbackHint = "Install Chromium or Google Chrome from https://www.google.com/chrome/ or https://www.chromium.org"
	case "darwin":
		if pm != nil && pm.Name == "brew" {
			info.InstallCommand = []string{"brew", "install", "--cask", "chromium"}
		}
		info.FallbackHint = "Install via Homebrew: brew install --cask chromium (or Google Chrome from google.com/chrome)"
	default:
		if pm != nil {
			switch pm.Name {
			case "apt-get":
				info.InstallCommand = []string{"apt-get", "install", "-y", "chromium"}
			case "dnf":
				info.InstallCommand = []string{"dnf", "install", "-y", "chromium"}
			case "pacman":
				info.InstallCommand = []string{"pacman", "-S", "--noconfirm", "chromium"}
			case "apk":
				info.InstallCommand = []string{"apk", "add", "chromium"}
			case "zypper":
				info.InstallCommand = []string{"zypper", "install", "-y", "chromium"}
			}
		}
		info.FallbackHint = "Install Chromium via your distribution package manager (e.g. apt-get install -y chromium) or use Docker"
	}
	return info
}

func checkYTDLP(cfg *config.Config) DependencyInfo {
	info := DependencyInfo{
		ID:                 DepYTDLP,
		Name:               "yt-dlp",
		Description:        "Extractor for high-speed media downloads and playlist expansion",
		Category:           "media",
		RequiredForFull:    true,
		RequiredForMinimal: false,
	}

	var path string
	if cfg != nil && strings.TrimSpace(cfg.Download.YTDLPPath) != "" {
		path = strings.TrimSpace(cfg.Download.YTDLPPath)
	} else {
		path, _ = exec.LookPath("yt-dlp")
	}

	if path != "" {
		info.Found = true
		info.Path = path
		info.Version = getToolVersion(path, "--version")
	}

	pm := DetectPackageManager()
	switch runtime.GOOS {
	case "windows":
		if pm != nil && pm.Name == "winget" {
			info.InstallCommand = []string{"winget", "install", "--id", "yt-dlp.yt-dlp", "--accept-source-agreements", "--accept-package-agreements"}
		}
		info.FallbackHint = "Download yt-dlp.exe from https://github.com/yt-dlp/yt-dlp/releases/latest"
	case "darwin":
		if pm != nil && pm.Name == "brew" {
			info.InstallCommand = []string{"brew", "install", "yt-dlp"}
		}
		info.FallbackHint = "Install via Homebrew: brew install yt-dlp"
	default:
		if pm != nil {
			switch pm.Name {
			case "apt-get":
				info.InstallCommand = []string{"apt-get", "install", "-y", "yt-dlp"}
			case "dnf":
				info.InstallCommand = []string{"dnf", "install", "-y", "yt-dlp"}
			case "pacman":
				info.InstallCommand = []string{"pacman", "-S", "--noconfirm", "yt-dlp"}
			case "apk":
				info.InstallCommand = []string{"apk", "add", "yt-dlp"}
			case "zypper":
				info.InstallCommand = []string{"zypper", "install", "-y", "yt-dlp"}
			}
		}
		info.FallbackHint = "Download standalone binary from https://github.com/yt-dlp/yt-dlp/releases/latest or use your package manager"
	}
	return info
}

func checkFFmpeg(cfg *config.Config) DependencyInfo {
	info := DependencyInfo{
		ID:                 DepFFmpeg,
		Name:               "FFmpeg",
		Description:        "Audio/video container remuxing, stream merging, and format transcoding",
		Category:           "media",
		RequiredForFull:    true,
		RequiredForMinimal: false,
	}

	var path string
	if cfg != nil && strings.TrimSpace(cfg.Download.FFmpegPath) != "" {
		path = strings.TrimSpace(cfg.Download.FFmpegPath)
	} else {
		path, _ = exec.LookPath("ffmpeg")
	}

	if path != "" {
		info.Found = true
		info.Path = path
		info.Version = getToolVersion(path, "-version")
	}

	pm := DetectPackageManager()
	switch runtime.GOOS {
	case "windows":
		if pm != nil && pm.Name == "winget" {
			info.InstallCommand = []string{"winget", "install", "--id", "Gyan.FFmpeg", "--accept-source-agreements", "--accept-package-agreements"}
		}
		info.FallbackHint = "Download FFmpeg builds from https://ffmpeg.org/download.html or https://www.gyan.dev/ffmpeg/builds/"
	case "darwin":
		if pm != nil && pm.Name == "brew" {
			info.InstallCommand = []string{"brew", "install", "ffmpeg"}
		}
		info.FallbackHint = "Install via Homebrew: brew install ffmpeg"
	default:
		if pm != nil {
			switch pm.Name {
			case "apt-get":
				info.InstallCommand = []string{"apt-get", "install", "-y", "ffmpeg"}
			case "dnf":
				info.InstallCommand = []string{"dnf", "install", "-y", "ffmpeg"}
			case "pacman":
				info.InstallCommand = []string{"pacman", "-S", "--noconfirm", "ffmpeg"}
			case "apk":
				info.InstallCommand = []string{"apk", "add", "ffmpeg"}
			case "zypper":
				info.InstallCommand = []string{"zypper", "install", "-y", "ffmpeg"}
			}
		}
		info.FallbackHint = "Install ffmpeg via your distribution package manager (e.g. apt-get install -y ffmpeg)"
	}
	return info
}

func checkDeno() DependencyInfo {
	info := DependencyInfo{
		ID:                 DepDeno,
		Name:               "Deno (JavaScript Runtime)",
		Description:        "Required by yt-dlp to solve YouTube JS challenges without format loss",
		Category:           "media",
		RequiredForFull:    false, // Recommended, yt-dlp can work degraded without it
		RequiredForMinimal: false,
	}

	path, _ := exec.LookPath("deno")
	if path != "" {
		info.Found = true
		info.Path = path
		info.Version = getToolVersion(path, "--version")
	}

	pm := DetectPackageManager()
	switch runtime.GOOS {
	case "windows":
		if pm != nil && pm.Name == "winget" {
			info.InstallCommand = []string{"winget", "install", "--id", "DenoLand.Deno", "--accept-source-agreements", "--accept-package-agreements"}
		}
		info.FallbackHint = "Install via: irm https://deno.land/install.ps1 | iex"
	case "darwin":
		if pm != nil && pm.Name == "brew" {
			info.InstallCommand = []string{"brew", "install", "deno"}
		}
		info.FallbackHint = "Install via Homebrew: brew install deno"
	default:
		info.FallbackHint = "Install via: curl -fsSL https://deno.land/install.sh | sh"
	}
	return info
}

// getToolVersion runs tool with arg and returns the first line of output trimmed.
func getToolVersion(bin, arg string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, arg).Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	return line
}
