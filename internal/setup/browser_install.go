package setup

import (
	"os/exec"
	"runtime"
	"strings"
)

// browserInstallQuestion returns the platform-appropriate "install Chromium
// now?" prompt. The wizard never claims an automation it cannot perform.
func browserInstallQuestion() string {
	switch runtime.GOOS {
	case "darwin":
		return "  Install Chromium now with Homebrew (brew install --cask chromium)?"
	case "windows":
		return "  Install Chromium now with winget? (Microsoft Store package)"
	default:
		return "  Install chromium now with apt-get? (Debian/Ubuntu)"
	}
}

// browserInstallCommand returns the package-manager argv that installs
// Chromium on this platform, or nil when the platform has no supported
// automatic installer. Returning nil is a first-class case: the wizard then
// prints a manual hint instead of silently running a wrong command.
func browserInstallCommand() []string {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("brew"); err != nil {
			return nil
		}
		return []string{"brew", "install", "--cask", "chromium"}
	case "windows":
		// winget is present on every supported Windows 10/11 build; the
		// Microsoft Store package is "Chromium" (the community build).
		if _, err := exec.LookPath("winget"); err == nil {
			return []string{"winget", "install", "--id", "Chromium.Chromium", "--accept-source-agreements", "--accept-package-agreements"}
		}
		return nil
	default:
		if _, err := exec.LookPath("apt-get"); err != nil {
			return nil
		}
		return []string{"apt-get", "install", "-y", "chromium"}
	}
}

// browserFallbackHint tells the operator what to do when the automatic
// Chromium install is unavailable or failed. It is platform-aware so the
// message always names a command that works where the user actually is.
func browserFallbackHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "     Install it manually: brew install --cask chromium  — or use Docker: docker compose up -d"
	case "windows":
		return "     Install it manually from https://www.chromium.org — or use Docker: docker compose up -d"
	default:
		return "     Use the Docker image instead: docker compose up -d  (see README)"
	}
}

// hasPackageMaker reports whether any supported automatic installer exists.
// Used by the doctor to phrase its Chromium advice.
func hasPackageMaker() bool {
	return browserInstallCommand() != nil
}

var _ = strings.TrimSpace
var _ = hasPackageMaker
