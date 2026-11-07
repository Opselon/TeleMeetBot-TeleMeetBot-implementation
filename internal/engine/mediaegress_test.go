package engine

import (
	"sync"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/go-rod/rod"
)

// The defect (session s-7d2d1b03): Download & Stream failed on the primary
// egress (SUBFREE at 172.17.0.1:3339 — "Sign in to confirm you're not a bot"),
// the session fell back to browser playback, and the media tab was launched
// through THE SAME blocked proxy — so YouTube bot-checked the watch page too
// and the session died with "media unavailable: YouTube wants a sign-in".
//
// YouTube's bot check is per-IP and applies to the watch page exactly as hard
// as it does to the innertube API. Once the download ladder has proof a route
// is rejected, the media browser must not be handed that route again: the
// browser launch has to pick the first egress the registry has NOT blocked.
func TestBrowserLaunchAvoidsEgressTheDownloadProvedBlocked(t *testing.T) {
	var mu sync.Mutex
	var seen []launchOptions
	mgr := NewManager(
		BrowserConfig{PerSession: true, Proxy: "http://172.17.0.1:3339"},
		MediaOptions{
			DownloadEnabled:   true,
			DownloadProxy:     "http://172.17.0.1:3339",
			DownloadFallbacks: []string{"socks5://172.17.0.1:9050"},
			MediaEgresses:     []string{"http://172.17.0.1:3339", "socks5://172.17.0.1:9050"},
			DownloadBaseDir:   t.TempDir(),
		},
		MeetOptions{GuestName: "Test Bot"},
		dummyLogger{},
	)
	mgr.launchFn = func(opts launchOptions) (*rod.Browser, func(), error) {
		mu.Lock()
		seen = append(seen, opts)
		mu.Unlock()
		return &rod.Browser{}, func() {}, nil
	}

	// Simulate exactly what fetchYTDLP records when the primary egress is
	// refused and the ladder advances: the primary is marked blocked.
	if mgr.mediaMgr == nil {
		t.Fatalf("media manager must exist for this test (DownloadEnabled=true)")
	}
	mgr.mediaMgr.MarkEgressBlocked("http://172.17.0.1:3339")

	if _, rel, err := mgr.acquireBrowser(); err != nil {
		t.Fatalf("acquire: %v", err)
	} else {
		rel()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("expected one launch, got %d", len(seen))
	}
	if got := seen[0].mediaProxy; got != "socks5://172.17.0.1:9050" {
		t.Fatalf("media browser must avoid the blocked egress: mediaProxy = %q, want %q", got, "socks5://172.17.0.1:9050")
	}
	if seen[0].browser.Proxy != "http://172.17.0.1:3339" {
		t.Fatalf("the configured proxy must stay untouched for the Meet leg: %q", seen[0].browser.Proxy)
	}
}

// The registry is advisory, never a launch killer: when every configured
// egress lost at least once the configured one is kept, so a session always
// has a route to try.
func TestBrowserLaunchKeepsConfiguredEgressWhenAllAreBlocked(t *testing.T) {
	var mu sync.Mutex
	var seen []launchOptions
	mgr := NewManager(
		BrowserConfig{PerSession: true, Proxy: "http://172.17.0.1:3339"},
		MediaOptions{
			DownloadEnabled:   true,
			DownloadProxy:     "http://172.17.0.1:3339",
			DownloadFallbacks: []string{"socks5://172.17.0.1:9050"},
			MediaEgresses:     []string{"http://172.17.0.1:3339", "socks5://172.17.0.1:9050"},
			DownloadBaseDir:   t.TempDir(),
		},
		MeetOptions{GuestName: "Test Bot"},
		dummyLogger{},
	)
	mgr.launchFn = func(opts launchOptions) (*rod.Browser, func(), error) {
		mu.Lock()
		seen = append(seen, opts)
		mu.Unlock()
		return &rod.Browser{}, func() {}, nil
	}
	mgr.mediaMgr.MarkEgressBlocked("http://172.17.0.1:3339")
	mgr.mediaMgr.MarkEgressBlocked("socks5://172.17.0.1:9050")

	if _, rel, err := mgr.acquireBrowser(); err != nil {
		t.Fatalf("acquire: %v", err)
	} else {
		rel()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("expected one launch, got %d", len(seen))
	}
	if got := seen[0].mediaProxy; got != "" {
		t.Fatalf("no unblocked egress exists, the configured one must be kept: mediaProxy = %q, want empty", got)
	}
}

// A direct-connection deployment (no proxy configured anywhere) must not be
// destabilised by the registry: "direct" has no alternative, so the launch
// proceeds exactly as configured.
func TestBrowserLaunchUnchangedWhenNoProxyConfigured(t *testing.T) {
	var mu sync.Mutex
	var seen []launchOptions
	mgr := NewManager(
		BrowserConfig{PerSession: true},
		MediaOptions{DownloadEnabled: true, DownloadBaseDir: t.TempDir()},
		MeetOptions{GuestName: "Test Bot"},
		dummyLogger{},
	)
	mgr.launchFn = func(opts launchOptions) (*rod.Browser, func(), error) {
		mu.Lock()
		seen = append(seen, opts)
		mu.Unlock()
		return &rod.Browser{}, func() {}, nil
	}
	if _, rel, err := mgr.acquireBrowser(); err != nil {
		t.Fatalf("acquire: %v", err)
	} else {
		rel()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("expected one launch, got %d", len(seen))
	}
	if got := seen[0].mediaProxy; got != "" {
		t.Fatalf("direct deployments must not gain a media proxy: %q", got)
	}
}

// mediaBrowserCfg is the pure half of the override: it must substitute the
// chosen egress for the launch and leave everything else (UA, flags source,
// window size) identical.
func TestMediaBrowserCfgOverridesOnlyProxy(t *testing.T) {
	base := BrowserConfig{Proxy: "http://172.17.0.1:3339", WindowWidth: 1920, WindowHeight: 1080, UserAgent: "ua"}
	if got := mediaBrowserCfg(launchOptions{browser: base}); got.Proxy != "http://172.17.0.1:3339" {
		t.Fatalf("without an override the configured proxy must survive: %q", got.Proxy)
	}
	over := mediaBrowserCfg(launchOptions{browser: base, mediaProxy: "socks5://172.17.0.1:9050"})
	if over.Proxy != "socks5://172.17.0.1:9050" {
		t.Fatalf("override must replace the proxy: %q", over.Proxy)
	}
	if over.WindowWidth != 1920 || over.WindowHeight != 1080 || over.UserAgent != "ua" {
		t.Fatalf("override must not touch the rest of the config: %+v", over)
	}
	if base.Proxy != "http://172.17.0.1:3339" {
		t.Fatalf("the caller's config must not be mutated: %q", base.Proxy)
	}
}

// mediaEgresses dedupes and orders: browser proxy first, then the download
// primary, then the fallbacks. The production config carries the same
// duplication (browser.proxy == download.proxy) to prove the dedupe.
func TestMediaEgressesOrderAndDedupe(t *testing.T) {
	c := &config.Config{}
	c.Browser.Proxy = "http://172.17.0.1:3339"
	c.Download.Proxy = "http://172.17.0.1:3339" // duplicate of browser.proxy
	c.Download.FallbackProxies = []string{"socks5://172.17.0.1:9050"}
	got := mediaEgresses(c)
	want := []string{"http://172.17.0.1:3339", "socks5://172.17.0.1:9050"}
	if len(got) != len(want) {
		t.Fatalf("mediaEgresses = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mediaEgresses[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestApplyMediaEgressSwitchesDirectToFallback(t *testing.T) {
	mgr := NewManager(
		BrowserConfig{PerSession: true, Proxy: "direct"},
		MediaOptions{
			DownloadEnabled:   true,
			DownloadProxy:     "",
			DownloadFallbacks: []string{"http://172.17.0.1:3339", "socks5://172.17.0.1:9050"},
			MediaEgresses:     []string{"http://172.17.0.1:3339", "socks5://172.17.0.1:9050"},
			DownloadBaseDir:   t.TempDir(),
		},
		MeetOptions{GuestName: "Test Bot"},
		dummyLogger{},
	)
	if mgr.mediaMgr == nil {
		t.Fatalf("media manager must exist")
	}

	// Mark direct egress as blocked (e.g. after yt-dlp direct failed)
	mgr.mediaMgr.MarkEgressBlocked("direct")

	opts := launchOptions{browser: mgr.browserCfg}
	mgr.applyMediaEgress(&opts)

	if opts.mediaProxy != "http://172.17.0.1:3339" {
		t.Fatalf("applyMediaEgress failed: mediaProxy = %q, want %q", opts.mediaProxy, "http://172.17.0.1:3339")
	}
}
