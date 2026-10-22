// Command telemeetbot is a fully headless Google Meet streaming bot with a
// Telegram dashboard. Run with --wizard for guided setup.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/bot"
	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/setup"
	"github.com/Opselon/TeleMeetBot/internal/store"
	"github.com/Opselon/TeleMeetBot/internal/web"
)

func main() {
	var (
		cfgFlag     = flag.String("config", "", "path to telemeetbot.yml (default: ./telemeetbot.yml or /etc/telemeetbot/config.yml)")
		wizardFlag  = flag.Bool("wizard", false, "run the interactive setup wizard")
		runAfter    = flag.Bool("run", false, "with --wizard: start the bot after a successful setup")
		checkFlag   = flag.Bool("check", false, "run doctor diagnostics and exit")
		captureFlag = flag.Bool("capture-test", false, "with --check: run a real tab-capture probe (starts Chromium)")
		versionFlag = flag.Bool("version", false, "print version and exit")

		// Live diagnosis: join-and-report against a real meeting link using
		// the engine the bot itself uses. Does not enter the dashboard.
		probeFlag     = flag.String("probe", "", "run a live one-shot probe against this Meet link and exit")
		probeMedia    = flag.String("probe-media", "https://www.youtube.com/watch?v=jNQXAC9IVRw", "media link for --probe")
		probeWaitSecs = flag.Int("probe-seconds", 90, "admit wait for --probe")
	)
	flag.Parse()

	if *versionFlag {
		fmt.Printf("telemeetbot %s (%s %s/%s)\n", config.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}

	cfgPath := config.ResolvePath(*cfgFlag)
	logger := newLogger("info")

	// Wizard mode (also triggered automatically when no config exists).
	if *wizardFlag || !config.Exists(cfgPath) {
		if !*wizardFlag {
			fmt.Printf("No config found at %s — starting setup wizard.\n\n", cfgPath)
		}
		cfg, err := setup.Run(setup.WizardOptions{
			ConfigPath:  cfgPath,
			Log:         logger,
			CaptureTest: false,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nsetup failed: %v\n", err)
			os.Exit(1)
		}
		cfgPath = cfg.Path()
		if *wizardFlag && !*runAfter {
			fmt.Println("\nSetup complete. Start the bot with:")
			fmt.Printf("  %s --config %s\n", os.Args[0], cfgPath)
			return
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot load config: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nRe-run the wizard: %s --wizard --config %s\n", err, os.Args[0], cfgPath)
		os.Exit(1)
	}

	logger = newLogger(cfg.Logging.Level, cfg.Logging.File)

	// Live diagnosis probe: join the meeting as a guest with the same
	// engine config the bot uses, stream the stage sequence to the log,
	// and exit. Diagnostics only — no dashboard, no Telegram traffic.
	if *probeFlag != "" {
		if *probeWaitSecs < 10 {
			*probeWaitSecs = 10
		}
		if err := runProbe(*probeFlag, *probeMedia, *probeWaitSecs, cfg, logger); err != nil {
			logger.Errorf("probe: %v", err)
			os.Exit(1)
		}
		return
	}

	if *checkFlag {
		report := setup.Doctor(cfg, cfgPath, setup.DoctorOptions{CaptureTest: *captureFlag, Log: logger})
		report.Print()
		if !report.OK() {
			os.Exit(1)
		}
		return
	}

	if err := run(cfg, cfgPath, logger); err != nil {
		logger.Errorf("fatal: %v", err)
		os.Exit(1)
	}
}

// run starts the bot, engine and health server until SIGINT/SIGTERM.
func run(cfg *config.Config, cfgPath string, log *slogLogger) error {
	log.Infof("TeleMeetBot %s starting (config %s)", config.Version, cfgPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Persisted history: warn about sessions killed with the previous process.
	st := store.Load(cfg.StateFile())
	interrupted := st.Interrupted()
	if len(interrupted) > 0 {
		log.Warnf("%d session(s) from a previous run were interrupted", len(interrupted))
	}

	bCfg, mCfg, meetCfg := engine.FromConfig(cfg)
	browserPath, browserErr := engine.ResolveBrowser(bCfg, log)
	if browserErr != nil {
		log.Warnf("browser problem: %v — sessions will fail until chromium is installed", browserErr)
	} else {
		log.Infof("chromium: %s", browserPath)
	}

	eng := engine.NewManager(bCfg, mCfg, meetCfg, log)

	api, err := bot.NewTelegramAPI(cfg.Telegram.Token)
	if err != nil {
		return err
	}
	b := bot.New(api, cfg, eng, st, log, cfgPath, browserPath, errorText(browserErr))
	if len(interrupted) > 0 {
		b.Broadcast(fmt.Sprintf("⚠️ <b>Restarted</b>\n\n%d session(s) were interrupted by the previous shutdown and are no longer in their meetings.", len(interrupted)))
	}

	healthSrv := web.New(cfg.Server.HealthAddr, b.Snapshot, log)
	errCh := make(chan error, 2)

	go func() { errCh <- b.Run(ctx) }()
	go func() { b.RunEvents(ctx) }()
	go func() { b.RefreshLoop(ctx) }()
	go func() { errCh <- healthSrv.Start(ctx) }()

	select {
	case <-ctx.Done():
		log.Infof("shutdown signal received; stopping sessions")
	case err := <-errCh:
		if err != nil {
			log.Errorf("component stopped: %v", err)
		}
		stop()
	}

	eng.StopAll()
	log.Infof("bye")
	return nil
}

// logger adapters ------------------------------------------------------------

// slogLogger adapts slog to the engine/bot logging interfaces.
type slogLogger struct{ l *slog.Logger }

func (s *slogLogger) Debugf(format string, args ...interface{}) {
	s.l.Debug(fmt.Sprintf(format, args...))
}
func (s *slogLogger) Infof(format string, args ...interface{}) {
	s.l.Info(fmt.Sprintf(format, args...))
}
func (s *slogLogger) Warnf(format string, args ...interface{}) {
	s.l.Warn(fmt.Sprintf(format, args...))
}
func (s *slogLogger) Errorf(format string, args ...interface{}) {
	s.l.Error(fmt.Sprintf(format, args...))
}

// newLogger builds the app logger (text on stderr, optional JSON file).
func newLogger(level string, file ...string) *slogLogger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	if len(file) > 0 && strings.TrimSpace(file[0]) != "" {
		if dir := filepath.Dir(file[0]); dir != "." {
			_ = os.MkdirAll(dir, 0o700)
		}
		if f, err := os.OpenFile(file[0], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			h = slog.NewTextHandler(f, &slog.HandlerOptions{Level: lvl})
		}
	}
	return &slogLogger{l: slog.New(h)}
}

// errorText renders an error for display (empty when nil).
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var _ = health.FormatUptime
var _ = time.Second
