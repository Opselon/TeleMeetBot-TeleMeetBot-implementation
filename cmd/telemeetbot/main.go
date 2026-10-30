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
	"github.com/Opselon/TeleMeetBot/internal/dashboard"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/googleauth"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/localbotapi"
	"github.com/Opselon/TeleMeetBot/internal/logx"
	"github.com/Opselon/TeleMeetBot/internal/setup"
	"github.com/Opselon/TeleMeetBot/internal/store"
	"github.com/Opselon/TeleMeetBot/internal/userstate"
	"github.com/Opselon/TeleMeetBot/internal/web"
)

func main() {
	// Support bare subcommands: `telemeetbot setup`, `telemeetbot doctor`, etc.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			os.Args = append([]string{os.Args[0], "--setup"}, os.Args[2:]...)
		case "doctor":
			os.Args = append([]string{os.Args[0], "--doctor"}, os.Args[2:]...)
		case "uninstall":
			os.Args = append([]string{os.Args[0], "--uninstall"}, os.Args[2:]...)
		case "version":
			os.Args = append([]string{os.Args[0], "--version"}, os.Args[2:]...)
		}
	}

	var (
		// serviceMode runs the process under the Windows Service Control
		// Manager. It is auto-detected (no console, SCM parent) and can also
		// be forced with --service for a manual sc.exe registration.
		serviceMode = flag.Bool("service", false, "run as a managed OS service (Windows SCM; auto-detected)")

		cfgFlag     = flag.String("config", "", "path to telemeetbot.yml (default: ./telemeetbot.yml or the platform's system config)")
		wizardFlag  = flag.Bool("wizard", false, "run the interactive setup wizard")
		setupFlag   = flag.Bool("setup", false, "run the setup and configuration wizard")
		installFlag = flag.Bool("install", false, "install binary and run setup wizard")
		runAfter    = flag.Bool("run", false, "with --wizard: start the bot after a successful setup")
		checkFlag   = flag.Bool("check", false, "run doctor diagnostics and exit")
		doctorFlag  = flag.Bool("doctor", false, "run diagnostics and exit (alias for --check)")
		profileFlag = flag.String("profile", "", "setup profile: full (default) or minimal")
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

	isSetupExe := strings.Contains(strings.ToLower(filepath.Base(os.Args[0])), "setup")
	isWizard := *wizardFlag || *setupFlag || *installFlag || isSetupExe
	isDoctor := *checkFlag || *doctorFlag

	// Wizard mode (also triggered automatically when no config exists).
	if isWizard || !config.Exists(cfgPath) {
		if !isWizard {
			fmt.Printf("No config found at %s — starting setup wizard.\n\n", cfgPath)
		}
		cfg, err := setup.Run(setup.WizardOptions{
			ConfigPath:    cfgPath,
			Log:           logger,
			CaptureTest:   false,
			Profile:       setup.Profile(*profileFlag),
			InstallerMode: *installFlag || isSetupExe,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nsetup failed: %v\n", err)
			os.Exit(1)
		}
		cfgPath = cfg.Path()
		if isWizard && !*runAfter {
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

	// One recorder for the whole process, shared by every handler wrap.
	logRec = logx.NewRecorder(2000)
	logger = newLogger(cfg.Logging.Level, cfg.Logging.File)

	// Secret redaction for captured logs: the dashboard can render log
	// entries, so credential material must be scrubbed before it lands in
	// the ring buffer. Best-effort hygiene, not a substitute for never
	// logging secrets in the first place.
	logx.SetRedactors(cfg.Telegram.Token, cfg.Google.ClientSecret, cfg.Telegram.LocalBotAPI.APIHash)

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

	if isDoctor {
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

	// Central log capture: every slog line is teed into the recorder (see
	// newLogger) so the web dashboard can tail and filter logs without
	// touching the hot path.
	rec := logRec
	if rec == nil {
		rec = logx.NewRecorder(2000)
		logRec = rec
	}

	// Per-user state tracker: one observable view of what every user is
	// doing, shared by the bot and the web dashboard.
	tracker := userstate.New(func(from, to userstate.Info) {
		if to.State == userstate.StateIdle {
			return
		}
		if from.State == "" || from.State == userstate.StateIdle {
			rec.Record(logx.Entry{Level: logx.LevelInfo, Component: "userstate",
				Message: fmt.Sprintf("user %d -> %s", to.UserID, to.State)})
			return
		}
		rec.Record(logx.Entry{Level: logx.LevelInfo, Component: "userstate",
			Message: fmt.Sprintf("user %d: %s -> %s", to.UserID, from.State, to.State)})
	})
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tracker.Prune(time.Hour)
			}
		}
	}()

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

	// Per-user preferences and session-queue metadata live in users.db. Like
	// meetings.db, a failure is fatal: silently losing every user's settings
	// on every restart would be worse than refusing to start.
	if _, err := store.BackupDB(cfg.UsersFile()); err != nil {
		log.Warnf("user db backup: %v", err)
	}
	users, err := store.NewUserStore(ctx, cfg.UsersFile())
	if err != nil {
		return fmt.Errorf("open user store %s: %w", cfg.UsersFile(), err)
	}
	defer users.Close()

	// One-shot migration: preferences the legacy state.json still holds are
	// lifted into users.db (backed up first, never overwritten, safe to
	// re-run). A missing or unreadable state.json is not an error.
	migrated, err := store.MigrateLegacyPrefs(ctx, users, cfg.StateFile())
	if err != nil {
		log.Warnf("legacy preference migration: %v", err)
	} else if migrated > 0 {
		log.Infof("migrated %d user preference(s) from state.json to users.db", migrated)
	}

	// Queue metadata that survived the previous process cannot resume
	// playback: mark it interrupted so /queue can show what was lost.
	if n, err := users.MarkQueuesInterrupted(ctx); err != nil {
		log.Warnf("mark interrupted queues: %v", err)
	} else if n > 0 {
		log.Warnf("%d session queue(s) from a previous run were interrupted", n)
	}

	var botOpts []bot.Option
	if playlists != nil {
		botOpts = append(botOpts, bot.WithPlaylists(playlists))
	}
	botOpts = append(botOpts, bot.WithSettings(users))
	botOpts = append(botOpts, bot.WithUserTracker(tracker))
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
		// Keep-warm: the first session consumes the warm slot; this loop
		// refills it whenever the engine is idle again, so EVERY session
		// skips the cold start, not just the first. Failures retry on the
		// next tick and never take the bot down.
		go eng.KeepWarm(ctx, time.Minute)
	}

	// Memory watchdog: the Go heap is bounded by GOMEMLIMIT, but a stalled
	// GC phase (browser JSON payloads, log fan-out) can creep toward it.
	// When the live heap crosses 80% of the limit, force a full GC cycle and
	// return the freed pages to the OS. This keeps long-lived processes at a
	// steady footprint instead of drifting up over days.
	if os.Getenv("GOMEMLIMIT") == "" {
		go memoryWatchdog(ctx, log, 256<<20)
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

	var tbapiMgr *localbotapi.Manager
	if cfg.Telegram.LocalBotAPI.Enabled {
		tbapiMgr = localbotapi.New(cfg.Telegram.LocalBotAPI, log)
		if err := tbapiMgr.Start(ctx); err != nil {
			return fmt.Errorf("start local telegram-bot-api server: %w", err)
		}
		defer tbapiMgr.Stop()
		if cfg.Telegram.APIEndpoint == "" {
			cfg.Telegram.APIEndpoint = tbapiMgr.Endpoint()
		}
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

	// Web dashboard (opt-in): Telegram Login for auth, the same admin
	// allow-list for authorization, and the engine/log/user surfaces for
	// control. A dashboard failure never takes the bot down.
	if cfg.Dash.Enabled {
		dashCfg := dashboard.Config{
			Enabled:       true,
			Addr:          cfg.Dash.Addr,
			BotToken:      cfg.Telegram.Token,
			BotUsername:   api.BotUsername(),
			PublicBaseURL: cfg.Dash.PublicBaseURL,
			SessionTTL:    time.Duration(cfg.Dash.SessionHours) * time.Hour,
			SecretPath:    filepath.Join(cfg.Paths.DataDir, "dashboard-secret.key"),
			IsAdmin:       cfg.IsAdmin,
		}
		if dashSrv, derr := dashboard.New(dashCfg, dashboard.Dependencies{
			Health: b.Snapshot,
			Engine: eng,
			Logs:   rec,
			Users:  tracker,
		}, log); derr != nil {
			log.Warnf("web dashboard unavailable: %v", derr)
		} else {
			go func() { errCh <- dashSrv.Start(ctx) }()
			log.Infof("web dashboard enabled at http://%s (login: Telegram admin account)", cfg.Dash.Addr)
		}
	}

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

	// Tell the operators what is happening: a silent shutdown looks exactly
	// like a crash from the outside.
	b.Broadcast("🛑 <b>Shutting down</b> — stopping active sessions…")
	eng.StopAll()
	eng.ShutdownWarm()
	b.Broadcast("✅ <b>Stopped</b> — all sessions closed cleanly. See you.")
	log.Infof("bye")
	return nil
}

// memoryWatchdog forces a full GC + OS page return when the live heap
// crosses 80% of limit. Periodic and cheap; a failure of the GC to reclaim
// is logged once per crossing, not per tick.
func memoryWatchdog(ctx context.Context, log *slogLogger, limit int64) {
	const threshold = 0.8
	var lastTrigger time.Time
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if int64(ms.HeapInuse) < int64(float64(limit)*threshold) {
				continue
			}
			// Avoid re-triggering every minute while the heap sits high.
			if !lastTrigger.IsZero() && time.Since(lastTrigger) < 5*time.Minute {
				continue
			}
			lastTrigger = time.Now()
			log.Warnf("[PIPELINE:runtime] heap %.1f MiB crossed %.0f%% of the %d MiB soft limit — forcing full GC",
				float64(ms.HeapInuse)/(1<<20), threshold*100, limit/(1<<20))
			runtime.GC()
			debug.FreeOSMemory()
		}
	}
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

// logRec is the process-wide log recorder. It is created after config load
// and teed from every subsequent log line; before that (early setup, wizard)
// nil simply means no capture.
var logRec *logx.Recorder

// newLogger builds the app logger (text on stdout, optional file), teed into
// the central log recorder so the web dashboard can tail and filter logs.
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
	var h slog.Handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	if len(file) > 0 && strings.TrimSpace(file[0]) != "" {
		if dir := filepath.Dir(file[0]); dir != "." {
			_ = os.MkdirAll(dir, 0o700)
		}
		if f, err := os.OpenFile(file[0], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			h = slog.NewTextHandler(f, &slog.HandlerOptions{Level: lvl})
		}
	}
	if logRec != nil {
		h = logx.NewTeeHandler(h, logRec)
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
