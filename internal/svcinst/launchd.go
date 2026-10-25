//go:build !windows

package svcinst

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// launchdInstaller writes a LaunchAgent under ~/Library/LaunchAgents and
// loads it with launchctl. macOS has no per-user systemd; a user agent is
// the native way to keep the bot running and restart it at login.
type launchdInstaller struct{}

// agentPath is where the plist lives for a given label.
func agentPath(label string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

// Install writes the plist and loads it with launchctl bootstrap.
func (launchdInstaller) Install(opts Options) error {
	normalize(&opts)
	if err := mustExist(opts.Binary); err != nil {
		return err
	}
	if opts.ConfigPath == "" {
		return fmt.Errorf("no config path given")
	}
	workDir := opts.DataDir
	if workDir == "" {
		workDir = filepath.Dir(opts.ConfigPath)
	}

	// StandardOutPath/StandardErrorPath keep the logs next to the data dir,
	// which is where an operator looks first on macOS (there is no journald).
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>--config</string>
        <string>%s</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, opts.Name, opts.Binary, opts.ConfigPath, workDir,
		filepath.Join(workDir, "telemeetbot.out.log"),
		filepath.Join(workDir, "telemeetbot.err.log"))

	dst := agentPath(opts.Name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(dst, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	// Boot out any previous generation first: launchctl refuses to bootstrap
	// an already-loaded label with "Input/output error".
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	_ = run("launchctl", "bootout", uid+"/"+opts.Name)
	if err := run("launchctl", "bootstrap", uid, dst); err != nil {
		return err
	}
	return run("launchctl", "enable", uid+"/"+opts.Name)
}

// Uninstall boots the agent out and deletes the plist.
func (launchdInstaller) Uninstall(name string) error {
	if name == "" {
		name = DefaultName
	}
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	_ = run("launchctl", "bootout", uid+"/"+name)
	if err := os.Remove(agentPath(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}
	return nil
}

// Status reads the agent state from launchctl print.
func (launchdInstaller) Status(name string) (string, bool, error) {
	if name == "" {
		name = DefaultName
	}
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	out, _ := execOutput("launchctl", "print", uid+"/"+name)
	if strings.TrimSpace(out) == "" {
		return "not-loaded", false, nil
	}
	// launchctl print reports "state = running" for a live agent.
	if strings.Contains(out, "state = running") {
		return "running", true, nil
	}
	return "loaded", false, nil
}
