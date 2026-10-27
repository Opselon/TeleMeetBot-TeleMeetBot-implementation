// Command telemeetbot is a fully headless Google Meet streaming bot with a
// Telegram dashboard. Run with --wizard for guided setup.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/bot"
	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/googleauth"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/setup"
	"github.com/Opselon/TeleMeetBot/internal/store"
	"github.com/Opselon/TeleMeetBot/internal/web"
)

func main() {
	var (
		// serviceMode runs the process under the Windows Service Control
		// Manager. It is auto-detected (no console, SCM parent) and can also
		// be forced with --service for a manual sc.exe registration.
		serviceMode = flag.Bool("service", false, "run as a managed OS service (Windows SCM; auto-detected)")

		cfgFlag     = flag.String("config", "", "path to telemeetbot.yml (default: ./telemeetbot.yml or the platform's system config)")
		wizardFlag  = flag.Bool("wizard", false, "run the interactive setup wizard")
		runAfter    = flag.Bool("run", false, "with --wizard: start the bot after a successful setup")
		checkFlag   = flag.Bool("check", false, "run doctor diagnostics and exit")
		captureFlag = flag.Bool("capture-test", false, "with --check: run a real tab-capture probe (starts Chromium)")
		versionFlag = flag.Bool("version", false, "print version and exit")

		// Lifecycle: --uninstall removes the OS service registration and the
		// installed binary; --restore-backup rolls the config back to the .bak
		// the wizard took before overwriting it. Both are safe to run twice.
		uninstallFlag     = flag.Bool("uninstall", false, "stop and remove the OS service, then delete the binary")
		uninstallKeepBin  = flag.Bool("keep-binary", false, "with --uninstall: remove only the service, keep the binary")
		uninstallYes      = flag.Bool("yes", false, "with --uninstall: confirm stopping a running service without a prompt")
		restoreBackupFlag = flag.Bool("restore-backup", false, "restore the config from the .bak the wizard saved")

		// Live diagnosis: join-and-report against a real meeting link using
		// the engine the bot itself uses. Does not enter the dashboard.
		probeFlag        = flag.String("probe", "", "run a live one-shot probe against this Meet link and exit")
		probeMedia       = flag.String("probe-media", "https://www.youtube.com/watch?v=jNQXAC9IVRw", "media link for --probe")
		mediaProbeFlag   = flag.String("media-probe", "", "diagnose one media link without a meeting (plays it and samples rendered pixels)")
		publishProbeFlag = flag.String("publish-probe", "", "capture a real media link and publish it through a WebRTC loopback (measures encoder/RTP/decoder, no meeting needed)")
		watchProbeFlag   = flag.String("watch-probe", "", "join this Meet link as a silent guest and measure the presentation remote participants see")
		probeWaitSecs    = flag.Int("probe-seconds", 90, "admit wait for --probe; sample seconds for --media-probe")
	)
	flag.Parse()

	if *versionFlag {
		fmt.Printf("telemeetbot %s (%s %s/%s)\n", config.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}

	// Rollback: restore the config the wizard backed up before overwriting.
	// Runs before every other mode so it works on a broken install too.
	if *restoreBackupFlag {
		cfgPath := config.ResolvePath(*cfgFlag)
		if err := config.RestoreBackup(cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "restore backup: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ config restored from %s.bak\n", cfgPath)
		return
	}

	// Uninstall: stop + remove the OS service, then optionally remove the
	// binary itself. Config and data stay untouched — an uninstall must never
	// destroy the operator's token or session history.
	if *uninstallFlag {
		if err := runUninstall(*cfgFlag, !*uninstallKeepBin, *uninstallYes, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "uninstall: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Windows service mode: the SCM starts the process with no console, so
	// every diagnostic branch below is skipped and the bot runs under the
	// service loop instead. On non-Windows builds_svcMode() is always false
	// and the flag is a no-op.
	if *serviceMode || shouldAutoEnterServiceMode() {
		if err := runAsService(); err != nil {
			fmt.Fprintf(os.Stderr, "service: %v\n", err)
			os.Exit(1)
		}
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

	// Media-only diagnosis: play the link on a tab exactly like the player tab,
	// then sample playback + rendered pixels. Green/black/codec problems show
	// here without needing a meeting.
	if *mediaProbeFlag != "" {
		secs := *probeWaitSecs
		if secs < 10 {
			secs = 10
		}
		if err := runMediaProbe(*mediaProbeFlag, secs, cfg, logger); err != nil {
			logger.Errorf("media probe: %v", err)
			os.Exit(1)
		}
		return
	}

	// Publish pipeline diagnosis: capture a real media tab and push it through a
	// real WebRTC publication (encoder -> RTP -> decode). This is the instrument
	// that separates "the tab paints pixels" from "participants see video".
	if *publishProbeFlag != "" {
		secs := *probeWaitSecs
		if secs < 3 {
			secs = 3
		}
		if err := runPublishProbe(*publishProbeFlag, secs, cfg, logger); err != nil {
			logger.Errorf("publish probe: %v", err)
			os.Exit(1)
		}
		return
	}

	// Remote-viewer diagnosis: join a meeting as a silent guest and measure
	// what remote participants actually receive (inbound video frames and
	// screenshot motion in the center of the call).
	if *watchProbeFlag != "" {
		secs := *probeWaitSecs
		if secs < 5 {
			secs = 5
		}
		if err := runWatchProbe(*watchProbeFlag, secs, cfg, logger); err != nil {
			logger.Errorf("watch probe: %v", err)
			os.Exit(1)
		}
		return
	}

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

	// Termination signals: SIGINT/SIGTERM on Unix; on Windows SIGTERM cannot
	// be caught, so the context listens for os.Interrupt (Ctrl+C / Ctrl+Break)
	// and the service stop event instead.
	ctx, stop := termSignalContext(context.Background())
	defer stop()

	// Persisted history: warn about sessions killed with the previous process.
	st := store.Load(cfg.StateFile())
	interrupted := st.Interrupted()
	if len(interrupted) > 0 {
		log.Warnf("%d session(s) from a previous run were interrupted", len(interrupted))
	}

	// Saved meetings live in SQLite next to state.json. A failure here is
	// fatal: a missing meeting store would silently revert users to typing
	// Meet links, and an unreadable database must not be papered over.
	if _, err := store.BackupDB(cfg.MeetingsFile()); err != nil {
		log.Warnf("meeting db backup: %v", err)
	}
	meetings, err := store.NewMeetingStore(ctx, cfg.MeetingsFile())
	if err != nil {
		return fmt.Errorf("open meeting store %s: %w", cfg.MeetingsFile(), err)
	}
	defer meetings.Close()

	// Playlists live in their own SQLite database. A broken library must not
	// prevent the bot from starting: sessions and saved meetings still work,
	// so a failure here is a warning and the dashboard hides the feature.
	if _, err := store.BackupDB(cfg.PlaylistsFile()); err != nil {
		log.Warnf("playlist db backup: %v", err)
	}
	playlists, err := store.NewPlaylistStore(ctx, cfg.PlaylistsFile())
	if err != nil {
		log.Warnf("playlist library unavailable: %v", err)
		playlists = nil
	} else {
		defer playlists.Close()
	}
	var botOpts []bot.Option
	if playlists != nil {
		botOpts = append(botOpts, bot.WithPlaylists(playlists))
	}
	var gauth *googleauth.Manager
	gauthCfg := googleauth.Config{
		ClientID:           cfg.Google.ClientID,
		ClientSecret:       cfg.Google.ClientSecret,
		RedirectURL:        cfg.Google.RedirectURL,
		TokenEncryptionKey: cfg.Google.TokenEncryptionKey,
		Scopes:             cfg.Google.Scopes,
	}
	var gauthErr error
	gauth, gauthErr = googleauth.New(filepath.Join(cfg.Paths.DataDir, "user-profiles"), gauthCfg, log)
	if gauthErr != nil {
		log.Warnf("[PIPELINE:googleauth] initialize googleauth: %v", gauthErr)
	} else if gauth != nil {
		botOpts = append(botOpts, bot.WithGoogleAuth(gauth))
	}

	bCfg, mCfg, meetCfg := engine.FromConfig(cfg)
	browserPath, browserErr := engine.ResolveBrowser(bCfg, log)
	if browserErr != nil {
		log.Warnf("browser problem: %v — sessions will fail until chromium is installed", browserErr)
	} else {
		log.Infof("chromium: %s", browserPath)
	}

	eng := engine.NewManager(bCfg, mCfg, meetCfg, log)
	defer eng.ShutdownWarm()
	// Optional warm-up: pre-launch one idle Chromium so the first session
	// skips the cold start. Failures are non-fatal (sessions just launch
	// cold). It respects limits.max_sessions: the warm slot is one process.
	if bCfg.WarmUp && browserErr == nil {
		go func() {
			if err := eng.WarmUp(); err != nil {
				log.Warnf("browser warm-up failed (sessions will start cold): %v", err)
			}
		}()
	}
	// Hard fence at the engine level: the dashboard also checks, but a session
	// must never be able to start past limits.max_sessions.
	eng.SetMaxSessions(cfg.Limits.MaxSessions)
	// Download & Stream: the media manager sweeps expired files on startup
	// (before the first download can land) and then on a ticker. Missing
	// optional tools degrade to Browser mode with a warning, never a crash.
	if mm := eng.MediaManager(); mm != nil {
		mm.Start(ctx)
		defer mm.Close()
		log.Infof("download & stream enabled (dir=%s ttl=%s)", mm.Opts().Dir, mm.Opts().TTL)
	} else if cfg.Download.Enabled {
		log.Warnf("download & stream is enabled in the config but unavailable: check download.dir; sessions fall back to Browser mode")
	} else {
		log.Infof("download & stream disabled (download.enabled: false); Browser mode only")
	}
	// Keep the Go heap bounded on a small VPS: GOMEMLIMIT makes the GC work
	// harder instead of letting the heap grow with browser JSON payloads.
	// An explicit operator setting always wins.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(256 << 20)
	}

	api, err := bot.NewTelegramAPIWithEndpoint(cfg.Telegram.Token, cfg.Telegram.APIEndpoint)
	if err != nil {
		return err
	}
	b := bot.New(api, cfg, eng, st, meetings, log, cfgPath, browserPath, errorText(browserErr), botOpts...)
	if len(interrupted) > 0 {
		b.Broadcast(fmt.Sprintf("⚠️ <b>Restarted</b>\n\n%d session(s) were interrupted by the previous shutdown and are no longer in their meetings.", len(interrupted)))
	}

	var webOpts []web.Option
	if gauth != nil {
		webOpts = append(webOpts, web.WithGoogleAuth(gauth, b.NotifyGoogleConnected))
	}
	healthSrv := web.New(cfg.Server.HealthAddr, b.Snapshot, log, webOpts...)
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
