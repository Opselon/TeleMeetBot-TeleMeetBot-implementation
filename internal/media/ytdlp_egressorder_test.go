package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// DIRECT FIRST — verified live 2026-11-02: unproxied egress extracts and
// downloads YouTube cleanly on an ordinary server, while a proxy YouTube has
// throttled (the rotating free pool at 172.17.0.1:3339) is what actually
// returns "Sign in to confirm you're not a bot" and "TLS/SSL connection has
// been closed (EOF)". The historical ladder ran the configured proxy FIRST
// and never tried a direct connection at all when any proxy was configured,
// so a zero-config install on a clean network still inherited a proxy it did
// not ask for, and a throttled proxy took the whole feature down. The ladder
// must place the direct route ahead of every configured proxy.
func TestEgressLadderPutsDirectFirst(t *testing.T) {
	t.Run("direct leads even when proxies are configured", func(t *testing.T) {
		m := newYtdlpTestManager(t, DefaultOptions(t.TempDir()), func(context.Context, []string) ([]byte, []byte, error) {
			return nil, nil, youtubeBotCheck
		})
		m.opts.Proxy = "http://172.17.0.1:3339"
		m.opts.FallbackProxies = []string{"socks5://172.17.0.1:9050", "socks5://127.0.0.1:9150"}

		got := m.egresses()
		if len(got) == 0 {
			t.Fatal("egresses() returned no routes")
		}
		if got[0] != "" {
			t.Fatalf("the first egress must be the direct route (\"\"), got %q (full: %q)", got[0], got)
		}
		want := []string{"", "http://172.17.0.1:3339", "socks5://172.17.0.1:9050", "socks5://127.0.0.1:9150"}
		if len(got) != len(want) {
			t.Fatalf("ladder length = %d, want %d (%q)", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ladder[%d] = %q, want %q (full: %q)", i, got[i], want[i], got)
			}
		}
	})
	t.Run("a zero-config install has exactly one route: direct", func(t *testing.T) {
		m := newYtdlpTestManager(t, DefaultOptions(t.TempDir()), func(context.Context, []string) ([]byte, []byte, error) {
			return nil, nil, youtubeBotCheck
		})
		got := m.egresses()
		if len(got) != 1 || got[0] != "" {
			t.Fatalf("an unconfigured manager must expose exactly one direct route, got %q", got)
		}
	})
	t.Run("egressLabel never prints an empty proxy", func(t *testing.T) {
		if got := egressLabel(""); got != "direct" {
			t.Errorf("egressLabel(\"\") = %q, want \"direct\"", got)
		}
		if got := egressLabel("   "); got != "direct" {
			t.Errorf("egressLabel(\"   \") = %q, want \"direct\"", got)
		}
		if got := egressLabel("socks5://127.0.0.1:9050"); got != "socks5://127.0.0.1:9050" {
			t.Errorf("egressLabel(proxy) = %q, want the proxy verbatim", got)
		}
	})
}

// The download must actually RUN over the direct route before touching a
// configured proxy — the egresses() order is meaningless if the loop skips
// it. The fake bot-checks the direct route (the historical failure that used
// to kill the whole session) and succeeds immediately over the proxy; if the
// ladder ever runs the proxy first, the direct route is never exercised and
// a zero-config user's download dies.
func TestDownloadRunsDirectRouteBeforeAnyProxy(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Proxy = "http://172.17.0.1:3339"

	var order []string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		p := proxyOf(args)
		if p == "" {
			order = append(order, "direct")
			// The direct route succeeds: the download must complete here and
			// never reach the configured proxy.
			writeStub(t, dir+"/vid.mp4")
			return []byte("136,1280,720,30,avc1.64001f,mp4a.40.2,mp4,1500\n"), nil, nil
		}
		order = append(order, p)
		return nil, nil, youtubeBotCheck
	})

	if _, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	}); err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if len(order) != 1 || order[0] != "direct" {
		t.Fatalf("the working direct route must be the only run, got order %q", order)
	}
	// A proxy that was never used must not be marked blocked: the registry
	// steers the media browser away from a route, and marking a healthy route
	// blocked would degrade unrelated sessions.
	if m.EgressBlocked(opts.Proxy) {
		t.Fatalf("an unused proxy must not be marked blocked")
	}
}

// The historical defect this whole ladder exists for: the direct route is
// bot-checked, the configured proxy is truncated mid-body, and only the
// fallback delivers. The ladder must walk all three in order and never give
// up before the fallback.
func TestEgressLadderWalksEveryRouteInOrder(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions(dir)
	opts.Proxy = "http://172.17.0.1:3339"
	opts.FallbackProxies = []string{"socks5://172.17.0.1:9050"}
	// A configured cookie jar answers the account gate on any route, so the
	// ladder still advances past a bot-check: this is the WITH-jar half of
	// the contract (the NO-jar half, where the ladder stops, is pinned by
	// cookies_test.go and by the account-gate fast fail).
	opts.CookiesFile = "/cookies.txt"

	var order []string
	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		p := proxyOf(args)
		order = append(order, p)
		switch {
		case p == "":
			return nil, []byte("ERROR: [youtube] X: Sign in to confirm you're not a bot"), exec.ErrNotFound
		case p == opts.Proxy:
			return nil, []byte("ERROR: [youtube] X: Unable to download API page: 15036 bytes read (caused by <IncompleteRead: 15036 bytes read>)"), exec.ErrNotFound
		default:
			writeStub(t, dir+"/vid.mp4")
			return []byte("136,1280,720,30,avc1.64001f,mp4a.40.2,mp4,1500\n"), nil, nil
		}
	})

	if _, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	}); err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	// Each route may run up to maxExecRetries+1 attempts, so collapse the
	// recorded order into the sequence of DISTINCT routes and compare that.
	want := []string{"", opts.Proxy, opts.FallbackProxies[0]}
	var distinct []string
	for _, p := range order {
		if len(distinct) == 0 || distinct[len(distinct)-1] != p {
			distinct = append(distinct, p)
		}
	}
	if len(distinct) != len(want) {
		t.Fatalf("ladder visited %d distinct routes (%q), want %d (%q)", len(distinct), distinct, len(want), want)
	}
	for i, w := range want {
		if distinct[i] != w {
			t.Fatalf("ladder position %d = %q, want %q (full order %q)", i, distinct[i], w, order)
		}
	}
}

// The final error must name every route the ladder tried, so the operator
// sees the diagnosis ("bot-checked on direct, truncated on the proxy") rather
// than a bare retry status. Only route identifiers are attached: proxy URLs
// carry no credentials in this pipeline, and stderr already carries the
// source's own text.
func TestFailedDownloadReportsTheEgressLadder(t *testing.T) {
	opts := DefaultOptions(t.TempDir())
	opts.Proxy = "http://172.17.0.1:3339"
	opts.FallbackProxies = []string{"socks5://172.17.0.1:9050"}
	// A cookie jar is set: the scenario's bot-check is an account gate, and
	// the fast fail stops the ladder only when NO jar could answer it. With
	// a jar configured the ladder still walks every route so the final error
	// can name them all.
	jar := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(jar, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.CookiesFile = jar

	m := newYtdlpTestManager(t, opts, func(_ context.Context, args []string) ([]byte, []byte, error) {
		return nil, []byte("ERROR: [youtube] X: Sign in to confirm you're not a bot"), exec.ErrNotFound
	})

	_, err := m.Download(context.Background(), DownloadRequest{
		URL: "https://www.youtube.com/watch?v=abcdefghijk", Kind: "youtube", ID: "vid",
	})
	if err == nil {
		t.Fatal("expected the download to fail when every route is bot-checked")
	}
	msg := err.Error()
	for _, want := range []string{"direct", opts.Proxy, opts.FallbackProxies[0], "egress ladder tried"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message must name %q, got: %s", want, msg)
		}
	}
}

// YouTube client configuration must be validated against the installed
// yt-dlp. `player_client` and its accepted-value list both landed after the
// 2024.03.05 release, so a binary from a distro backport or a pinned older
// build can silently reject the argv. The probe asks yt-dlp itself.
func TestYouTubeClientConfigValidatedAgainstInstalledYTDLP(t *testing.T) {
	t.Run("unsupported clients are filtered", func(t *testing.T) {
		caps := &ytdlpClientCapabilities{clients: map[string]bool{"android": true, "web": true}}
		kept, dropped := configuredYouTubeClients([]string{"android", "ios", "web"}, caps)
		if len(kept) != 2 || kept[0] != "android" || kept[1] != "web" {
			t.Fatalf("unsupported ios must be filtered, kept = %q", kept)
		}
		if len(dropped) != 1 || dropped[0] != "ios" {
			t.Fatalf("dropped must report exactly ios, got %q", dropped)
		}
	})
	t.Run("a missing capability list keeps the configured ladder", func(t *testing.T) {
		for _, caps := range []*ytdlpClientCapabilities{nil, {}} {
			kept, dropped := configuredYouTubeClients([]string{"android", "ios", "web"}, caps)
			if len(kept) != 3 || len(dropped) != 0 {
				t.Fatalf("an absent probe must keep the full ladder, kept = %q dropped = %q", kept, dropped)
			}
		}
	})
	t.Run("every client reported unsupported keeps the documented ladder", func(t *testing.T) {
		// yt-dlp's own list can lag the extractor, so an empty result must not
		// strip the argv down to a client list nothing understands.
		caps := &ytdlpClientCapabilities{clients: map[string]bool{"tv": true}}
		pairs, dropped := youtubeExtractorArgs(caps)
		joined := strings.Join(pairs, " ")
		if !strings.Contains(joined, "player_client="+strings.Join(youtubeClientOrder, ",")) {
			t.Fatalf("the documented client ladder must survive, pairs = %q (dropped %q)", pairs, dropped)
		}
	})
	t.Run("argv carries the filtered client list", func(t *testing.T) {
		m := newYtdlpTestManager(t, DefaultOptions(t.TempDir()), func(context.Context, []string) ([]byte, []byte, error) {
			return nil, nil, youtubeBotCheck
		})
		// Simulate an installed yt-dlp that reports android+web only. The
		// probe runs through ensureYTDLPClientCaps (the production path), so
		// the override is exercised exactly as fetchYTDLP would.
		m.ytdlpCapsProbe = func(context.Context, string) *ytdlpClientCapabilities {
			return &ytdlpClientCapabilities{clients: map[string]bool{"android": true, "web": true}}
		}
		m.ensureYTDLPClientCaps(context.Background(), "yt-dlp-probe")
		args := m.ytdlpArgs("https://www.youtube.com/watch?v=abcdefghijk", "vid", "", "", "")
		val := playerClientArgValue(args)
		if val != "android,web" {
			t.Fatalf("player_client value = %q, want %q (argv %q)", val, "android,web", args)
		}
	})
	t.Run("probe failure keeps the documented ladder and is not fatal", func(t *testing.T) {
		m := newYtdlpTestManager(t, DefaultOptions(t.TempDir()), func(context.Context, []string) ([]byte, []byte, error) {
			return nil, nil, youtubeBotCheck
		})
		// A probe whose binary is missing must still produce a usable argv.
		m.ytdlpCapsProbe = func(context.Context, string) *ytdlpClientCapabilities {
			return &ytdlpClientCapabilities{}
		}
		args := m.ytdlpArgs("https://www.youtube.com/watch?v=abcdefghijk", "vid", "", "", "")
		if val := playerClientArgValue(args); val != strings.Join(youtubeClientOrder, ",") {
			t.Fatalf("probe failure must keep the documented ladder, got %q", val)
		}
	})
	t.Run("the ladder carries the bot-check-resistant clients first", func(t *testing.T) {
		// Regression: the historical ladder was android,ios,web, which is
		// exactly the set YouTube challenges on a flagged egress. yt-dlp's
		// own defaults since 2026.10.21 are tv_samsung,visionos,web; a video
		// that bot-checks android/ios/web on every route still extracts
		// through tv_samsung+visionos. The ladder must therefore lead with
		// them (verified live 2026-11-04).
		want := []string{"tv_samsung", "visionos", "android", "ios", "web"}
		if !reflect.DeepEqual(youtubeClientOrder, want) {
			t.Fatalf("youtubeClientOrder = %q, want %q", youtubeClientOrder, want)
		}
	})
	t.Run("an old binary filters clients newer than its release", func(t *testing.T) {
		// Regression: --list-extractor-args was REMOVED in yt-dlp 2026.x, so
		// the capability probe exits 2 on the binary this image ships. The
		// version floor map must then keep the argv honest: a 2026.08.19
		// binary understands visionos but NOT tv_samsung, and a 2024 binary
		// understands neither.
		clients := func(version string) []string {
			caps := &ytdlpClientCapabilities{clients: map[string]bool{}}
			for _, c := range youtubeClientOrder {
				if floor, ok := ytdlpClientAddedVersion[c]; ok && ytdlpVersionAtLeast(version, floor) {
					caps.clients[c] = true
				}
			}
			kept, _ := configuredYouTubeClients(youtubeClientOrder, caps)
			return kept
		}
		if got := clients("2026.10.26"); !reflect.DeepEqual(got, []string{"tv_samsung", "visionos", "android", "ios", "web"}) {
			t.Errorf("2026.10.26 must support every ladder client, got %q", got)
		}
		if got := clients("2026.08.19"); !reflect.DeepEqual(got, []string{"visionos", "android", "ios", "web"}) {
			t.Errorf("2026.08.19 predates tv_samsung, got %q", got)
		}
		if got := clients("2024.03.05"); !reflect.DeepEqual(got, []string{"android", "ios", "web"}) {
			t.Errorf("2024.03.05 predates tv_samsung+visionos, got %q", got)
		}
	})
	t.Run("the version fallback probe validates clients against yt-dlp 2026.x", func(t *testing.T) {
		// Regression: on yt-dlp 2026.10.26 --list-extractor-args exits 2 with
		// "no such option" (the option was removed). probeYouTubeClientCapabilities
		// must recover via --version instead of silently keeping a ladder that
		// may name clients the binary does not honour.
		m := newYtdlpTestManager(t, DefaultOptions(t.TempDir()), func(context.Context, []string) ([]byte, []byte, error) {
			return nil, nil, youtubeBotCheck
		})
		m.ytdlpCapsProbe = nil // force the production probe path
		m.ytdlpVersionProbe = func(context.Context, string) string { return "2026.10.26" }
		caps := m.probeYouTubeClientCapabilities(context.Background(), "yt-dlp-probe")
		if len(caps.clients) == 0 {
			t.Fatalf("the version fallback must populate the client set on yt-dlp 2026.x")
		}
		for _, c := range youtubeClientOrder {
			if !caps.clients[c] {
				t.Errorf("yt-dlp 2026.10.26 must report support for %q", c)
			}
		}
		args := m.ytdlpArgs("https://www.youtube.com/watch?v=abcdefghijk", "vid", "", "", "")
		if val := playerClientArgValue(args); val != "tv_samsung,visionos,android,ios,web" {
			t.Fatalf("the argv must carry the full validated ladder, got %q", val)
		}
	})
}

// parseYTDLPVersion / ytdlpVersionAtLeast guard features against binaries
// too old to support them. Unknown or partial versions must report false so
// nothing is ever assumed supported.
func TestYTDLPVersionParsing(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"2026.10.26", "2026.10.26", true},
		{"2026.10.26@master", "2026.10.26", true},
		{"2025.01.15", "2025.01.15", true},
		{"2024.03.05", "2024.03.05", true},
		{"", "", false},
		{"2026.10", "", false},      // too few components
		{"2026.10.26.1", "", false}, // too many components is not a release line
		{"v2026.10.26", "", false},  // a tag prefix is not a version line
		{"abc.def.ghi", "", false},
	}
	for _, tc := range cases {
		got, ok := parseYTDLPVersion(tc.in)
		if ok != tc.valid || got != tc.want {
			t.Errorf("parseYTDLPVersion(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.valid)
		}
	}
	if !ytdlpVersionAtLeast("2026.10.26", ytdlpMinClientListVersion) {
		t.Errorf("2026.10.26 must satisfy %s", ytdlpMinClientListVersion)
	}
	if ytdlpVersionAtLeast("2024.03.05", ytdlpMinClientListVersion) {
		t.Errorf("2024.03.05 must NOT satisfy %s", ytdlpMinClientListVersion)
	}
	if ytdlpVersionAtLeast("garbage", ytdlpMinClientListVersion) {
		t.Error("an unparseable version must never satisfy a minimum")
	}
	if !ytdlpVersionAtLeast("2025.01.15", "2025.01.15") {
		t.Error("an exact version must satisfy itself")
	}
}

// playerClientArgValue must find the client list no matter which
// --extractor-args pair carries it.
func TestPlayerClientArgValue(t *testing.T) {
	if got := playerClientArgValue([]string{"--extractor-args", "youtube:player_client=android,ios,web"}); got != "android,ios,web" {
		t.Errorf("got %q", got)
	}
	if got := playerClientArgValue([]string{"--extractor-args", "youtube:player_skip=configs", "--extractor-args", "youtube:player_client=android"}); got != "android" {
		t.Errorf("got %q", got)
	}
	if got := playerClientArgValue([]string{"--extractor-args", "other:player_client=x"}); got != "" {
		t.Errorf("a non-youtube extractor must not match, got %q", got)
	}
	if got := playerClientArgValue([]string{}); got != "" {
		t.Errorf("an empty argv must return empty, got %q", got)
	}
}
