package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/svcinst"
)

// Prompter is a tiny interactive prompt reader. It is deterministic and
// injectable so the wizard can be tested without a TTY.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
}

// WizardPrompter is the question surface used by the wizard (injectable for tests).
type WizardPrompter interface {
	Ask(label, def string) (string, error)
	AskRequired(label, def string) (string, error)
	Confirm(label string, def bool) (bool, error)
	Choose(label string, options []string, def int) (int, error)
}

// NewPrompter builds a Prompter.
func NewPrompter(in io.Reader, out io.Writer) *Prompter {
	return &Prompter{in: bufio.NewReader(in), out: out}
}

// Ask prints a labelled question and returns the answer (or the default).
func (p *Prompter) Ask(label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", err
	}
	ans := strings.TrimSpace(line)
	if ans == "" {
		ans = def
	}
	return ans, nil
}

// AskRequired loops until a non-empty answer arrives.
func (p *Prompter) AskRequired(label, def string) (string, error) {
	for {
		ans, err := p.Ask(label, def)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(ans) != "" {
			return strings.TrimSpace(ans), nil
		}
		fmt.Fprintln(p.out, "  (required — please type a value)")
	}
}

// Confirm asks a yes/no question.
func (p *Prompter) Confirm(label string, def bool) (bool, error) {
	suffix := "[y/N]"
	if def {
		suffix = "[Y/n]"
	}
	for {
		fmt.Fprintf(p.out, "%s %s: ", label, suffix)
		line, err := p.in.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return def, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(p.out, "  please answer y or n")
	}
}

// Choose presents a numbered list and returns the selected index.
func (p *Prompter) Choose(label string, options []string, def int) (int, error) {
	fmt.Fprintln(p.out, label)
	for i, o := range options {
		mark := " "
		if i == def {
			mark = "*"
		}
		fmt.Fprintf(p.out, "  %s %d) %s\n", mark, i+1, o)
	}
	for {
		ans, err := p.Ask("Choose", strconv.Itoa(def+1))
		if err != nil {
			return def, err
		}
		n, err := strconv.Atoi(ans)
		if err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		fmt.Fprintf(p.out, "  enter a number between 1 and %d\n", len(options))
	}
}

// WizardOptions configures the setup wizard.
type WizardOptions struct {
	// ConfigPath is where the config will be written; empty resolves the
	// standard location.
	ConfigPath string
	In         io.Reader
	Out        io.Writer
	Log        engine.Logger
	// NonInteractive applies defaults without asking (token from
	// TELEMEETBOT_TOKEN or an existing config).
	NonInteractive bool
	// SkipBrowserInstall forbids package installation.
	SkipBrowserInstall bool
	// InstallService installs and starts a systemd unit (root only).
	InstallService bool
	// CaptureTest runs a real tab-capture probe at the end.
	CaptureTest bool
	// TokenValidator overrides Telegram token validation (tests).
	TokenValidator func(string) (string, error)
	// Exec runs commands; defaults to runCmd (tests can stub it).
	Exec func(name string, args ...string) error
	// FindBrowser overrides chromium detection (tests inject deterministic mocks).
	FindBrowser func(bcfg engine.BrowserConfig) (string, error)
	// Prompter overrides the interactive question surface (tests).
	Prompter WizardPrompter
	// Profile sets the installation profile: ProfileFull or ProfileMinimal.
	Profile Profile
	// InstallerMode is true when invoked to install the binary to the system.
	InstallerMode bool
	// InstallDir overrides the destination directory when InstallerMode is true.
	InstallDir string
	// SkipMediaInstall forbids running package managers for media tools.
	SkipMediaInstall bool
}

// Run executes the interactive setup and returns the saved config.
func Run(opts WizardOptions) (*config.Config, error) {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Exec == nil {
		opts.Exec = runCmd
	}
	validator := opts.TokenValidator
	if validator == nil {
		validator = func(tok string) (string, error) {
			u, err := validateToken(tok)
			if err != nil {
				return "", err
			}
			return u, nil
		}
	}
	var p WizardPrompter = NewPrompter(opts.In, opts.Out)
	if opts.Prompter != nil {
		p = opts.Prompter
	}
	out := opts.Out

	cfg := config.Default()
	path := opts.ConfigPath
	if path == "" {
		path = defaultConfigPath()
	}
	// A leading ~ means the user's home on every platform.
	path = config.ExpandUserPath(path)

	fmt.Fprintln(out, "┌──────────────────────────────────────────────┐")
	fmt.Fprintln(out, "│  TeleMeetBot · setup wizard                  │")
	fmt.Fprintln(out, "│  Google Meet streaming bot (Go + Chromium)   │")
	fmt.Fprintln(out, "└──────────────────────────────────────────────┘")
	fmt.Fprintln(out)

	elevatedStr := "No (user-space mode)"
	if svcinst.IsElevated() {
		elevatedStr = "Yes (administrator / root privileges)"
	}
	svcSys := svcinst.System()
	svcStr := string(svcSys)
	if svcSys == svcinst.KindNone {
		svcStr = "none detected (manual start / container mode)"
	}
	fmt.Fprintf(out, "• Environment : %s/%s | Elevated: %s | Service: %s\n\n", runtime.GOOS, runtime.GOARCH, elevatedStr, svcStr)

	var installedBin string
	if opts.InstallerMode {
		targetDir := opts.InstallDir
		if targetDir == "" {
			targetDir = defaultInstallDir()
		}
		if !opts.NonInteractive {
			var err error
			targetDir, err = p.Ask("Installation directory for binary", targetDir)
			if err != nil {
				return nil, err
			}
		}
		var err error
		installedBin, err = installBinaryTransactionally(targetDir, out)
		if err != nil {
			return nil, fmt.Errorf("application install failed: %w", err)
		}
	}

	if opts.Profile == "" {
		if opts.NonInteractive {
			opts.Profile = ProfileFull
		} else {
			choice, err := p.Choose("Installation profile", []string{
				"full    — Google Meet + Download & Stream media engine (Chromium, yt-dlp, FFmpeg, Deno)",
				"minimal — Google Meet presentation only (Chromium only, direct browser streaming)",
			}, 0)
			if err != nil {
				return nil, err
			}
			if choice == 1 {
				opts.Profile = ProfileMinimal
			} else {
				opts.Profile = ProfileFull
			}
		}
	}
	if opts.Profile == ProfileMinimal {
		cfg.Download.Enabled = false
	} else {
		cfg.Download.Enabled = true
	}

	if !opts.NonInteractive {
		var err error
		path, err = p.Ask("Config file path", path)
		if err != nil {
			return nil, err
		}
	}
	if existing := config.Exists(path); existing {
		loaded, err := config.Load(path)
		if err == nil {
			cfg = loaded
			fmt.Fprintf(out, "→ editing existing config: %s\n\n", path)
		} else {
			fmt.Fprintf(out, "! existing config is unreadable (%v); starting fresh\n\n", err)
		}
	}

	// ---- 1. Telegram token ------------------------------------------------
	token := cfg.Telegram.Token
	if env := strings.TrimSpace(os.Getenv("TELEMEETBOT_TOKEN")); env != "" && opts.NonInteractive {
		token = env
	}
	if !opts.NonInteractive {
		for attempt := 1; attempt <= 3; attempt++ {
			var err error
			token, err = p.AskRequired("1/7 Telegram bot token (from @BotFather)", token)
			if err != nil {
				return nil, err
			}
			fmt.Fprint(out, "  validating token… ")
			uname, err := validator(token)
			if err != nil {
				fmt.Fprintf(out, "❌ %v\n", err)
				continue
			}
			fmt.Fprintf(out, "✅ connected to @%s\n", uname)
			break
		}
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("a Telegram bot token is required")
	}
	cfg.Telegram.Token = strings.TrimSpace(token)

	// ---- 2. Admins -------------------------------------------------------
	adminDefault := joinInts(cfg.Telegram.Admins)
	if !opts.NonInteractive {
		raw, err := p.Ask("2/7 Admin Telegram user IDs, comma separated (blank = first /start claims the bot)", adminDefault)
		if err != nil {
			return nil, err
		}
		ids, err := parseIDs(raw)
		if err != nil {
			return nil, err
		}
		cfg.Telegram.Admins = ids
	}
	cfg.Telegram.FirstStartClaimsAdmin = len(cfg.Telegram.Admins) == 0
	if !opts.NonInteractive && len(cfg.Telegram.Admins) == 0 {
		claim, err := p.Confirm("3/7 Allow the first /start to claim the bot as admin?", true)
		if err != nil {
			return nil, err
		}
		cfg.Telegram.FirstStartClaimsAdmin = claim
	}

	// ---- 3. Chromium -----------------------------------------------------
	fmt.Fprintln(out, "\nBrowser check:")
	bcfg, _, _ := engine.FromConfig(cfg)
	detect := opts.FindBrowser
	if detect == nil {
		detect = func(bcfg engine.BrowserConfig) (string, error) {
			return engine.ResolveBrowser(bcfg, opts.Log)
		}
	}
	bin, err := detect(bcfg)
	if err != nil {
		fmt.Fprintf(out, "  ⚠️  %v\n", err)
		if !opts.SkipBrowserInstall {
			install := true
			if !opts.NonInteractive {
				install, err = p.Confirm(browserInstallQuestion(), true)
				if err != nil {
					return nil, err
				}
			}
			if install {
				if pkgCmd := browserInstallCommand(); pkgCmd != nil {
					fmt.Fprintf(out, "  → %s %s (this can take a few minutes)\n", pkgCmd[0], strings.Join(pkgCmd[1:], " "))
					// Run the package manager exactly once: an apt-get/brew
					// install is slow and idempotent, but running it twice in
					// a row doubles the wait and double-logs the output.
					ierr := opts.Exec(pkgCmd[0], pkgCmd[1:]...)
					if ierr != nil {
						fmt.Fprintf(out, "  ❌ install failed: %v\n", ierr)
						fmt.Fprintln(out, browserFallbackHint())
					} else if p2, err := detect(bcfg); err == nil {
						fmt.Fprintf(out, "  ✅ chromium installed at %s\n", p2)
						cfg.Browser.Executable = p2
					}
				} else {
					fmt.Fprintln(out, "  ℹ️  no automatic Chromium installer for this platform")
					fmt.Fprintln(out, browserFallbackHint())
				}
			}
		}
		if !opts.NonInteractive {
			manual, err := p.Ask("  Explicit chromium path (blank keeps auto-detect)", cfg.Browser.Executable)
			if err != nil {
				return nil, err
			}
			cfg.Browser.Executable = manual
		}
	} else {
		fmt.Fprintf(out, "  ✅ %s\n", bin)
	}

	// Media tools (yt-dlp, FFmpeg, Deno) for Full profile
	if cfg.Download.Enabled {
		fmt.Fprintln(out, "\nMedia pipeline check (Full profile):")
		for _, depID := range []DependencyID{DepYTDLP, DepFFmpeg, DepDeno} {
			dep := CheckDependency(depID, cfg, opts.Log)
			if dep.Found {
				fmt.Fprintf(out, "  ✅ %s: %s (%s)\n", dep.Name, dep.Path, dep.Version)
				continue
			}
			fmt.Fprintf(out, "  ⚠️  %s: not found\n", dep.Name)
			if len(dep.InstallCommand) > 0 && !opts.SkipMediaInstall {
				install := true
				if !opts.NonInteractive {
					install, _ = p.Confirm(fmt.Sprintf("     Install %s using %s now?", dep.Name, dep.InstallCommand[0]), true)
				}
				if install {
					fmt.Fprintf(out, "     Running: %s...\n", strings.Join(dep.InstallCommand, " "))
					if err := opts.Exec(dep.InstallCommand[0], dep.InstallCommand[1:]...); err != nil {
						fmt.Fprintf(out, "     ⚠️  install error: %v\n", err)
					} else {
						re := CheckDependency(depID, cfg, opts.Log)
						if re.Found {
							fmt.Fprintf(out, "     ✅ %s installed: %s (%s)\n", re.Name, re.Path, re.Version)
						}
					}
				}
			}
			if !dep.Found && dep.FallbackHint != "" {
				fmt.Fprintf(out, "     Tip: %s\n", dep.FallbackHint)
			}
		}
	}

	// ---- 4. Presentation mode -------------------------------------------
	mode := 0
	if cfg.Meet.PresentMode == "screen" {
		mode = 1
	}
	if !opts.NonInteractive {
		var err error
		mode, err = p.Choose("4/7 How should Meet share the stream?", []string{
			"tab    — share the media tab (headless, recommended)",
			"screen — share a whole X display (install Xvfb; heavier)",
		}, mode)
		if err != nil {
			return nil, err
		}
	}
	if mode == 1 {
		cfg.Meet.PresentMode = "screen"
		cfg.Meet.CaptureSource = "Entire screen (monitor 1)"
		if !opts.NonInteractive {
			if cfg.Browser.Display, err = p.Ask("  X display (Xvfb)", cfg.Browser.Display); err != nil {
				return nil, err
			}
		}
		if which("Xvfb") == "" {
			fmt.Fprintln(out, "  ⚠️  Xvfb not found; screen mode needs it: apt-get install -y xvfb")
		}
	} else {
		cfg.Meet.PresentMode = "tab"
		cfg.Meet.CaptureSource = "TeleMeet Media"
	}

	// ---- 5. Limits / defaults -------------------------------------------
	if !opts.NonInteractive {
		var err error
		raw, err := p.Ask("5/7 Max concurrent sessions", strconv.Itoa(cfg.Limits.MaxSessions))
		if err != nil {
			return nil, err
		}
		if n, cerr := strconv.Atoi(strings.TrimSpace(raw)); cerr == nil && n >= 1 && n <= 8 {
			cfg.Limits.MaxSessions = n
		}
		vol, err := p.Ask("    Default volume % (0-100)", strconv.Itoa(cfg.Media.DefaultVolume))
		if err != nil {
			return nil, err
		}
		if n, cerr := strconv.Atoi(strings.TrimSpace(vol)); cerr == nil && n >= 0 && n <= 100 {
			cfg.Media.DefaultVolume = n
		}
	}

	// ---- 6. Health endpoint & data dir ----------------------------------
	if !opts.NonInteractive {
		var err error
		if cfg.Server.HealthAddr, err = p.Ask("6/7 Health endpoint bind address", cfg.Server.HealthAddr); err != nil {
			return nil, err
		}
		if cfg.Paths.DataDir, err = p.Ask("    Data directory", cfg.Paths.DataDir); err != nil {
			return nil, err
		}
	}

	// ---- 7. Save ---------------------------------------------------------
	fmt.Fprintln(out, "\nSummary:")
	fmt.Fprintf(out, "  token      : %s\n", hardenToken(cfg.Telegram.Token))
	fmt.Fprintf(out, "  admins     : %v (first-start claim: %t)\n", cfg.Telegram.Admins, cfg.Telegram.FirstStartClaimsAdmin)
	fmt.Fprintf(out, "  chromium   : %s\n", nonEmpty(cfg.Browser.Executable, "auto-detect"))
	fmt.Fprintf(out, "  share mode : %s (%s)\n", cfg.Meet.PresentMode, cfg.Meet.CaptureSource)
	fmt.Fprintf(out, "  sessions   : %d max\n", cfg.Limits.MaxSessions)
	fmt.Fprintf(out, "  health     : http://%s/healthz\n", cfg.Server.HealthAddr)
	fmt.Fprintf(out, "  data dir   : %s\n", cfg.Paths.DataDir)
	fmt.Fprintf(out, "  config     : %s\n", path)

	proceed := true
	if !opts.NonInteractive {
		var err error
		proceed, err = p.Confirm("7/7 Save this configuration?", true)
		if err != nil {
			return nil, err
		}
	}
	if !proceed {
		return nil, errors.New("setup cancelled; nothing was written")
	}
	if err := os.MkdirAll(cfg.Paths.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// Back up an existing config before overwriting it: a re-run over a live
	// install must be revertable with `telemeetbot --wizard` alone. The backup
	// is best-effort — a missing file is the fresh-install case.
	if config.Exists(path) {
		if bak, err := config.Backup(path); err != nil {
			fmt.Fprintf(out, "  ⚠️  could not back up the existing config: %v\n", err)
		} else {
			fmt.Fprintf(out, "  ↩️  previous config backed up to %s (restore with --restore-backup)\n", bak)
		}
	}
	if err := cfg.Save(path); err != nil {
		return nil, fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(out, "\n✅ config saved to %s\n", path)

	// ---- Optional OS service ---------------------------------------------
	// One question across every platform: systemd on Linux, a Windows
	// service through the SCM, a launchd agent on macOS. The installer is
	// nil when the host has no usable service system (a container without
	// systemd), and the wizard then says so instead of failing.
	installSvc := opts.InstallService
	if installSvc {
		if err := installManagedService(path, installedBin); err != nil {
			fmt.Fprintf(out, "⚠️  service install: %v\n", err)
		}
	} else if !opts.NonInteractive {
		ask, err := p.Confirm(serviceQuestion(), true)
		if err != nil {
			return nil, err
		}
		if ask {
			if err := installManagedService(path, installedBin); err != nil {
				fmt.Fprintf(out, "⚠️  service install: %v\n", err)
			}
		}
	}

	// ---- Optional capture probe -----------------------------------------
	runProbe := opts.CaptureTest
	if !opts.NonInteractive && !opts.CaptureTest {
		var err error
		runProbe, err = p.Confirm("Run a real tab-capture test now? (starts Chromium once)", true)
		if err != nil {
			return nil, err
		}
	}
	if runProbe {
		fmt.Fprintln(out, "\nCapture test (this verifies Meet's \"A tab\" share can be granted headlessly):")
		rep := Doctor(cfg, path, DoctorOptions{CaptureTest: true, SkipNetwork: true, Log: opts.Log})
		for _, c := range rep.Checks {
			if c.Name != "tab capture" {
				continue
			}
			if c.OK {
				fmt.Fprintf(out, "  ✅ %s\n", c.Detail)
			} else {
				fmt.Fprintf(out, "  ❌ %s\n", c.Detail)
			}
		}
	}

	fmt.Fprintln(out, "\n============================================================")
	fmt.Fprintln(out, "              TeleMeetBot Setup Report")
	fmt.Fprintln(out, "============================================================")
	fmt.Fprintf(out, "• Version      : %s\n", config.Version)
	fmt.Fprintf(out, "• Platform     : %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(out, "• Profile      : %s\n", opts.Profile)
	if installedBin != "" {
		fmt.Fprintf(out, "• Binary       : %s\n", installedBin)
	}
	fmt.Fprintf(out, "• Config       : %s\n", path)
	fmt.Fprintf(out, "• Data Dir     : %s\n", cfg.Paths.DataDir)
	if cfg.Telegram.Token != "" {
		fmt.Fprintf(out, "• Telegram Bot : Token configured (%s)\n", hardenToken(cfg.Telegram.Token))
	}
	fmt.Fprintf(out, "• Health API   : http://%s/healthz\n", cfg.Server.HealthAddr)

	fmt.Fprintln(out, "\nManagement & Diagnostics:")
	binCmd := os.Args[0]
	if installedBin != "" {
		binCmd = installedBin
	}
	fmt.Fprintf(out, "  • Start bot  : %s --config %s\n", binCmd, path)
	fmt.Fprintf(out, "  • Diagnostics: %s --check --config %s\n", binCmd, path)
	fmt.Fprintf(out, "  • Re-run     : %s --wizard --config %s\n", binCmd, path)
	fmt.Fprintf(out, "  • Rollback   : %s --restore-backup\n", binCmd)
	fmt.Fprintf(out, "  • Uninstall  : %s --uninstall\n", binCmd)
	fmt.Fprintln(out, "============================================================")
	return cfg, nil
}

// installManagedService installs and starts TeleMeetBot as an OS service on
// whatever supervisor the host actually has: systemd on Linux, the SCM on
// Windows, launchd on macOS. Returns a clear error when the host has none.
func installManagedService(cfgPath string, binOverride string) error {
	inst := svcinst.New()
	if inst == nil {
		return errors.New("no service system found (systemd/launchd/SCM); run the binary directly or use Docker")
	}
	exe := binOverride
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return fmt.Errorf("resolve own executable: %w", err)
		}
	}
	if err := inst.Install(svcinst.Options{
		Binary:     exe,
		ConfigPath: cfgPath,
		DataDir:    filepath.Dir(cfgPath),
	}); err != nil {
		return err
	}
	fmt.Printf("✅ service installed and started (%s)\n", svcinst.DefaultName)
	return nil
}

// serviceQuestion returns the platform-appropriate install question.
func serviceQuestion() string {
	switch runtime.GOOS {
	case "windows":
		return "Install and start TeleMeetBot as a Windows service now? (needs an elevated shell)"
	case "darwin":
		return "Install and start a launchd agent now? (runs at login, restarts on crash)"
	default:
		return "Install and start a systemd service now?"
	}
}

// installService writes and enables a systemd unit for the current binary.
// Deprecated: kept for callers that ask for systemd by name; installManagedService
// covers every platform.
func installService(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=TeleMeetBot — Google Meet streaming bot
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --config %s
Restart=always
RestartSec=5
WorkingDirectory=%s
# Chromium sandbox in containers
Environment=HOME=/root

[Install]
WantedBy=multi-user.target
`, exe, cfgPath, filepath.Dir(cfgPath))

	const unitPath = "/etc/systemd/system/telemeetbot.service"
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := runCmd("systemctl", "daemon-reload"); err != nil {
		return err
	}
	return runCmd("systemctl", "enable", "--now", "telemeetbot")
}

// defaultConfigPath picks a sensible config location for the platform.
func defaultConfigPath() string {
	return config.DefaultConfigPath()
}

// parseIDs parses "123, 456" into int64s.
func parseIDs(raw string) ([]int64, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '\t'
	})
	var out []int64
	for _, f := range fields {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid Telegram user id %q", f)
		}
		out = append(out, id)
	}
	return out, nil
}

func joinInts(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func hardenToken(t string) string {
	if i := strings.Index(t, ":"); i > 0 {
		return t[:i] + ":***"
	}
	return "***"
}

func nonEmpty(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	return a
}

func defaultInstallDir() string {
	if runtime.GOOS == "windows" {
		if svcinst.IsElevated() {
			if pf := os.Getenv("ProgramFiles"); pf != "" {
				return filepath.Join(pf, "TeleMeetBot")
			}
			return `C:\Program Files\TeleMeetBot`
		}
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return filepath.Join(la, "Programs", "TeleMeetBot")
		}
		return filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local", "Programs", "TeleMeetBot")
	}
	// Unix / macOS
	if svcinst.IsElevated() {
		return "/usr/local/bin"
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return filepath.Join(home, ".local", "bin")
}

func installBinaryTransactionally(targetDir string, out io.Writer) (string, error) {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", fmt.Errorf("cannot create installation directory %s: %w", targetDir, err)
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve own executable: %w", err)
	}
	selfResolved, err := filepath.EvalSymlinks(self)
	if err == nil {
		self = selfResolved
	}
	binName := filepath.Base(self)
	if strings.Contains(strings.ToLower(binName), "setup") {
		if runtime.GOOS == "windows" {
			binName = "telemeetbot.exe"
		} else {
			binName = "telemeetbot"
		}
	}
	destPath := filepath.Join(targetDir, binName)
	if filepath.Clean(self) == filepath.Clean(destPath) {
		return destPath, nil
	}

	tmpPath := destPath + ".tmp"
	srcFile, err := os.Open(self)
	if err != nil {
		return "", fmt.Errorf("open source binary: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("stage binary to %s: %w", tmpPath, err)
	}
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("copy binary: %w", err)
	}
	dstFile.Close()

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("install binary to %s: %w", destPath, err)
	}
	fmt.Fprintf(out, "  ✅ installed binary to %s\n", destPath)
	return destPath, nil
}

var _ = exec.Command
var _ = time.Second
