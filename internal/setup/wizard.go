package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// Prompter is a tiny interactive prompt reader. It is deterministic and
// injectable so the wizard can be tested without a TTY.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
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
	p := NewPrompter(opts.In, opts.Out)
	out := opts.Out

	cfg := config.Default()
	path := opts.ConfigPath
	if path == "" {
		path = defaultConfigPath()
	}

	fmt.Fprintln(out, "┌──────────────────────────────────────────────┐")
	fmt.Fprintln(out, "│  TeleMeetBot · setup wizard                  │")
	fmt.Fprintln(out, "│  Google Meet streaming bot (Go + Chromium)   │")
	fmt.Fprintln(out, "└──────────────────────────────────────────────┘")
	fmt.Fprintln(out)

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
				install, err = p.Confirm("  Install chromium now with apt-get? (Debian/Ubuntu)", true)
				if err != nil {
					return nil, err
				}
			}
			if install {
				fmt.Fprintln(out, "  → apt-get update && apt-get install -y chromium (this can take a few minutes)")
				_ = opts.Exec("apt-get", "update")
				if ierr := opts.Exec("apt-get", "install", "-y", "chromium"); ierr != nil {
					fmt.Fprintf(out, "  ❌ install failed: %v\n", ierr)
					fmt.Fprintln(out, "     Use the Docker image instead: docker compose up -d  (see README)")
				} else if p2, err := detect(bcfg); err == nil {
					fmt.Fprintf(out, "  ✅ chromium installed at %s\n", p2)
					cfg.Browser.Executable = p2
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
	if err := cfg.Save(path); err != nil {
		return nil, fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(out, "\n✅ config saved to %s\n", path)

	// ---- Optional systemd service ---------------------------------------
	if opts.InstallService && os.Geteuid() == 0 && which("systemctl") != "" {
		if err := installService(path); err != nil {
			fmt.Fprintf(out, "⚠️  systemd unit: %v\n", err)
		} else {
			fmt.Fprintln(out, "✅ systemd unit installed and started (telemeetbot.service)")
		}
	} else if !opts.NonInteractive && os.Geteuid() == 0 && which("systemctl") != "" {
		install, err := p.Confirm("Install and start a systemd service now?", true)
		if err != nil {
			return nil, err
		}
		if install {
			if err := installService(path); err != nil {
				fmt.Fprintf(out, "⚠️  systemd unit: %v\n", err)
			} else {
				fmt.Fprintln(out, "✅ systemd unit installed and started (telemeetbot.service)")
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

	fmt.Fprintln(out, "\nNext steps:")
	fmt.Fprintf(out, "  • run the bot : %s --config %s\n", os.Args[0], path)
	fmt.Fprintln(out, "  • open Telegram, press /start, then ➕ New stream")
	fmt.Fprintf(out, "  • diagnostics : %s --check --config %s\n", os.Args[0], path)
	return cfg, nil
}

// installService writes and enables a systemd unit for the current binary.
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

// defaultConfigPath picks a sensible config location.
func defaultConfigPath() string {
	if os.Geteuid() == 0 {
		return "/etc/telemeetbot/config.yml"
	}
	return "telemeetbot.yml"
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

var _ = exec.Command
var _ = time.Second
