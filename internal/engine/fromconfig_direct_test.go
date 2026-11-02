package engine

import (
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/config"
)

// DIRECT-BY-DEFAULT: the download egress must not inherit browser.proxy.
// Verified live 2026-11-02: unproxied egress extracts and downloads YouTube
// cleanly on an ordinary server, while a proxy YouTube has throttled is what
// returns the bot-check and TLS EOF failures the whole fallback ladder exists
// for. The historical coupling meant an operator who configured a proxy only
// because the WATCH PAGE needed one silently forced yt-dlp through the same
// (possibly throttled) route — so a fresh install inherited a proxy it never
// asked for and a partially-blocked proxy took the whole feature down.
//
// This is the config-level half of the direct-first ladder; the runtime half
// lives in media.Manager.egresses() (TestEgressLadderPutsDirectFirst).
func TestDownloadProxyDoesNotInheritBrowserProxy(t *testing.T) {
	c := &config.Config{}
	c.Browser.Proxy = "http://172.17.0.1:3339"
	// No explicit download.proxy: the download must run DIRECT.
	if got := downloadProxyFor(c); got != "" {
		t.Errorf("downloadProxyFor with no download.proxy = %q, want \"\" (direct)", got)
	}
	// An explicit download.proxy wins and is reported verbatim.
	c.Download.Proxy = "socks5://127.0.0.1:9050"
	if got := downloadProxyFor(c); got != "socks5://127.0.0.1:9050" {
		t.Errorf("downloadProxyFor = %q, want the explicit proxy verbatim", got)
	}
}

// mediaEgresses still lists every configured egress for the media browser's
// route selection — the registry that steers the browser away from a route
// the download already proved rejected. trimmed and deduped.
func TestMediaEgressesAreDedupedAndTrimmed(t *testing.T) {
	c := &config.Config{}
	c.Browser.Proxy = "  http://172.17.0.1:3339  "
	c.Download.Proxy = "http://172.17.0.1:3339" // duplicate of browser's
	c.Download.FallbackProxies = []string{" socks5://172.17.0.1:9050 ", ""}

	got := mediaEgresses(c)
	want := []string{"http://172.17.0.1:3339", "socks5://172.17.0.1:9050"}
	if len(got) != len(want) {
		t.Fatalf("mediaEgresses = %q, want %q", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("mediaEgresses[%d] = %q, want %q", i, got[i], w)
		}
	}
}
