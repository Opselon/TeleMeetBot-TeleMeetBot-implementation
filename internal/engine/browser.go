package engine

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

// defaultUserAgent is used when browser.user_agent is not configured. Meet
// refuses headless "HeadlessChrome" users, so we present a stock Chrome UA.
const defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// launchOptions bundles everything launchBrowser needs.
type launchOptions struct {
	browser  BrowserConfig
	meet     MeetOptions
	userData string
}

// detectProxy finds an explicit or ambient SOCKS5 proxy (e.g. Tor on 127.0.0.1:9050 or
// Docker bridge host 172.17.0.1:9050).
func detectProxy(cfg BrowserConfig) string {
	if p := strings.TrimSpace(cfg.Proxy); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("TELEMEETBOT_PROXY")); p != "" {
		return p
	}
	candidates := []string{
		"127.0.0.1:9050",
		"172.17.0.1:9050",
		"host.docker.internal:9050",
	}
	for _, addr := range candidates {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return "socks5://" + addr
		}
	}
	return ""
}

// GPU backend modes accepted by browser.gpu.
const (
	// GPUModeOff disables the GPU process: Chromium composites the shared tab
	// in software without SwiftShader's per-frame GL round trips.
	GPUModeOff = "off"
	// GPUModeSwiftShader forces ANGLE/SwiftShader software GL.
	GPUModeSwiftShader = "swiftshader"
	// GPUModeAuto leaves the compositor choice to Chromium (real-GPU hosts).
	GPUModeAuto = "auto"
)

// gpuMode resolves browser.gpu. Headless defaults to software compositing:
// SwiftShader rasterises every shared frame through a software GL pipeline and
// measured ~6x the host CPU of plain software compositing for identical frames
// on a 2-core VPS.
func gpuMode(cfg BrowserConfig) string {
	switch strings.ToLower(strings.TrimSpace(cfg.GPU)) {
	case "":
		if cfg.Headless {
			return GPUModeOff
		}
		return GPUModeAuto
	case "default":
		return GPUModeAuto
	case GPUModeOff, "disable", "disabled", "none":
		return GPUModeOff
	case GPUModeSwiftShader, "angle", "swangle":
		return GPUModeSwiftShader
	case GPUModeAuto, "on", "hardware":
		return GPUModeAuto
	default:
		return GPUModeAuto
	}
}

// launchFlags is the Chromium command line for one browser (pure so the flag
// contract is testable without launching anything).
func launchFlags(cfg BrowserConfig, meet MeetOptions) []string {
	src := captureSource(cfg, meet)
	out := []string{
		"disable-dev-shm-usage",
		"disable-background-timer-throttling",
		"disable-backgrounding-occluded-windows",
		"disable-renderer-backgrounding",
		"disable-notifications",
		"disable-crash-reporter",
		"disable-blink-features=AutomationControlled",
		"autoplay-policy=no-user-gesture-required",
		// Do NOT set use-fake-device-for-media-stream or use-fake-ui-for-media-stream.
		// fake-device replaces every capture with Chromium's green test pattern, and
		// fake-ui makes getDisplayMedia auto-pick a *screen* source that either fails
		// to start (NotReadableError) or falls back to that same green pattern —
		// both end up presenting a green rectangle instead of the media tab.
		// Camera/mic permissions are granted through CDP instead (see grantMedia).
		"enable-usermedia-screen-capturing",
		"auto-select-desktop-capture-source=" + src,
		"auto-select-tab-capture-source-by-title=" + src,
		fmt.Sprintf("window-size=%d,%d", widthOf(cfg), heightOf(cfg)),
		// Headless Chrome exposes one virtual display; this sizes it so tab-capture
		// (and therefore the Meet share) is not stuck at 800x600.
		fmt.Sprintf("screen-info={%dx%d}", widthOf(cfg), heightOf(cfg)),
		"lang=" + languageOf(cfg),
		"accept-lang=" + languageOf(cfg),
		"user-agent=" + userAgentOf(cfg),
	}
	switch gpuMode(cfg) {
	case GPUModeSwiftShader:
		out = append(out, "enable-unsafe-swiftshader", "use-gl=angle", "use-angle=swiftshader", "disable-gpu-sandbox")
	case GPUModeOff:
		out = append(out, "disable-gpu")
	}
	return append(out, cfg.ExtraArgs...)
}

// launchBrowser starts Chromium with the flags needed to join Meet and share a
// tab automatically (no screen picker dialog, no user gesture). The returned
// close function must be called exactly once (it is idempotent).
func launchBrowser(opts launchOptions, log Logger) (*rod.Browser, func(), error) {
	cfg := opts.browser
	bin, err := ResolveBrowser(cfg, log)
	if err != nil {
		return nil, nil, err
	}

	l := launcher.New().
		Bin(bin).
		NoSandbox(true).
		Leakless(true) // reaps the whole tree even if our process dies

	if cfg.Headless {
		l.Set("headless", "new")
	} else {
		l.Headless(false)
	}
	if proxy := detectProxy(cfg); proxy != "" {
		l.Proxy(proxy)
		bypass := "meet.google.com;*.google.com;accounts.google.com;localhost;127.0.0.1;<local>;172.16.0.0/12;10.0.0.0/8;192.168.0.0/16"
		l.Set("proxy-bypass-list", bypass)
		log.Infof("[PIPELINE:browser] media proxy enabled: %s (Meet bypassed)", proxy)
	}

	for _, f := range launchFlags(cfg, opts.meet) {
		if name, vals, ok := splitArg(f); ok {
			l.Set(flags.Flag(name), vals...)
		}
	}

	// Headless mode has no virtual display; when headless=false attach to the
	// configured X display (e.g. Xvfb :99).
	if !cfg.Headless && strings.TrimSpace(cfg.Display) != "" {
		l.Env("DISPLAY=" + cfg.Display)
	}

	dir := ""
	if opts.userData != "" {
		dir = opts.userData
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, err
		}
		l.UserDataDir(dir)
	}

	url, err := l.Launch()
	if err != nil {
		return nil, nil, fmt.Errorf("launch chromium %s: %w", bin, err)
	}
	b := rod.New().ControlURL(url)
	if err := b.Connect(); err != nil {
		l.Kill()
		return nil, nil, fmt.Errorf("connect to chromium: %w", err)
	}
	grantMedia(b, log)

	var closeOnce sync.Once
	closeFn := func() {
		closeOnce.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				_ = b.Close()
				close(done)
			}()
			select {
			case <-done:
			case <-ctx.Done():
			}
			l.Kill()
			if dir != "" {
				_ = os.RemoveAll(dir)
			}
		})
	}
	return b, closeFn, nil
}

// grantMedia pre-authorizes camera/microphone for Meet via CDP. The old
// --use-fake-ui-for-media-stream switch did this too, but it also hijacked
// getDisplayMedia into choosing a screen source, which broke tab sharing.
func grantMedia(b *rod.Browser, log Logger) {
	err := proto.BrowserGrantPermissions{
		Origin: "https://meet.google.com",
		Permissions: []proto.BrowserPermissionType{
			proto.BrowserPermissionTypeAudioCapture,
			proto.BrowserPermissionTypeVideoCapture,
		},
	}.Call(b)
	if err != nil {
		log.Warnf("[PIPELINE:browser] grant camera/mic permission: %v", err)
		return
	}
	log.Infof("[PIPELINE:browser] camera/mic permission granted for meet.google.com")
}

// captureSource picks the auto-select value Chromium uses to answer
// getDisplayMedia without showing the picker.
func captureSource(cfg BrowserConfig, meet MeetOptions) string {
	if src := strings.TrimSpace(meet.CaptureSource); src != "" {
		return src
	}
	if meet.PresentMode == "screen" {
		return "Entire screen (monitor 1)"
	}
	return "TeleMeet Media"
}

func widthOf(cfg BrowserConfig) int {
	if cfg.WindowWidth > 0 {
		return cfg.WindowWidth
	}
	return 1280
}

func heightOf(cfg BrowserConfig) int {
	if cfg.WindowHeight > 0 {
		return cfg.WindowHeight
	}
	return 800
}

func languageOf(cfg BrowserConfig) string {
	if cfg.Lang != "" {
		return cfg.Lang
	}
	return "en-US"
}

func userAgentOf(cfg BrowserConfig) string {
	if cfg.UserAgent != "" {
		return cfg.UserAgent
	}
	return defaultUserAgent
}

// splitArg parses "name=value" or bare "name" extra CLI args.
func splitArg(a string) (name string, vals []string, ok bool) {
	a = strings.TrimSpace(a)
	if a == "" {
		return "", nil, false
	}
	if strings.Contains(a, "=") {
		parts := strings.SplitN(a, "=", 2)
		return parts[0], strings.Split(parts[1], ","), true
	}
	return a, nil, true
}

// checkExecutable verifies a browser path exists and is executable.
func checkExecutable(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("browser path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("browser not found at %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("browser path %s is a directory", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

// ResolveBrowser finds a usable Chromium: explicit config first, then PATH, then
// well-known package locations. It never downloads anything (the Docker image
// ships Chromium; on bare metal the wizard installs it).
func ResolveBrowser(cfg BrowserConfig, log Logger) (string, error) {
	if path := strings.TrimSpace(cfg.Executable); path != "" {
		if err := checkExecutable(path); err != nil {
			return "", err
		}
		return path, nil
	}
	if env := strings.TrimSpace(os.Getenv("TELEMEETBOT_BROWSER")); env != "" {
		if err := checkExecutable(env); err == nil {
			return env, nil
		}
	}
	if p, ok := launcher.LookPath(); ok {
		return p, nil
	}
	for _, p := range []string{
		"/usr/bin/chromium", "/usr/bin/chromium-browser", "/usr/lib/chromium/chromium",
		"/usr/lib/chromium-browser/chromium-browser", "/snap/bin/chromium",
		"/opt/chrome-for-testing/chrome-linux64/chrome",
	} {
		if err := checkExecutable(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no chromium found: install chromium or set browser.executable (e.g. chromium, google-chrome)")
}

// newPage creates a page and applies the UA override so pages do not see a
// HeadlessChrome user agent.
func newPage(b *rod.Browser, cfg BrowserConfig) (*rod.Page, error) {
	p, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, err
	}
	_, _ = p.EvalOnNewDocument(`
		try {
			Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
			window.chrome = window.chrome || { runtime: {} };
		} catch (e) {}
	`)
	req := &proto.NetworkSetUserAgentOverride{
		UserAgent:      userAgentOf(cfg),
		AcceptLanguage: languageOf(cfg) + ",en;q=0.9",
		Platform:       "Win32",
	}
	if err := p.SetUserAgent(req); err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}
