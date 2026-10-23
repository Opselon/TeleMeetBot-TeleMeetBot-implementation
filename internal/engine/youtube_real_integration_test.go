package engine

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestIntegrationRealYouTubeConsentAndPlayback drives the real thing: a live
// YouTube watch URL, the real "Before you continue to YouTube" wall, the real
// player. It is opt-in because it needs internet egress (and, on datacenter IPs,
// the proxy egress that keeps YouTube from serving a bot check):
//
//	TELEMEETBOT_YOUTUBE_URL=https://www.youtube.com/watch?v=... \
//	TELEMEETBOT_PROXY=http://172.17.0.1:3339 \
//	TELEMEETBOT_BROWSER=/opt/chrome-for-testing/chrome-linux64/chrome \
//	go test ./internal/engine -run TestIntegrationRealYouTube -v -count=1
//
// It passes only when the consent wall is dismissed, the surface is fitted, the
// video advances and the sampled frames are real pixels — the same bars the live
// session enforces.
func TestIntegrationRealYouTubeConsentAndPlayback(t *testing.T) {
	link := os.Getenv("TELEMEETBOT_YOUTUBE_URL")
	if link == "" {
		t.Skip("set TELEMEETBOT_YOUTUBE_URL=https://www.youtube.com/watch?v=... to run the real YouTube consent/playback test")
	}
	bin := browserBin(t)
	cfg := BrowserConfig{
		Executable:   bin,
		Headless:     true,
		WindowWidth:  1280,
		WindowHeight: 800,
		Lang:         envOr("TELEMEETBOT_LANG", "en-US"),
		Proxy:        os.Getenv("TELEMEETBOT_PROXY"),
	}
	seconds := 12
	if s := os.Getenv("TELEMEETBOT_YOUTUBE_SECONDS"); s != "" {
		if d, err := time.ParseDuration(s + "s"); err == nil && d > 0 {
			seconds = int(d.Seconds())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	rep, err := ProbeMedia(ctx, cfg, link, seconds, testLogger{t})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	t.Logf("consent=%q viewport=%q overlayFree=%t ready=%d advance=%.2f verdict=%q",
		rep.Consent, rep.Viewport, rep.OverlayFree, rep.ReadyState, rep.Advance, rep.Verdict)

	if rep.ConsentPresent {
		t.Fatalf("consent wall was never dismissed: %s (%s)", rep.Consent, rep.Verdict)
	}
	if rep.ReadyState < 2 {
		t.Fatalf("video page never got ready (readyState=%d): %s", rep.ReadyState, rep.Verdict)
	}
	if rep.Advance < 1.0 || !rep.Progressed {
		t.Fatalf("playback did not advance (advance=%.2fs): %s", rep.Advance, rep.Verdict)
	}
	if rep.BlackFrames || rep.GreenFrames {
		t.Fatalf("frames are not real video (black=%t green=%t): %s", rep.BlackFrames, rep.GreenFrames, rep.Verdict)
	}
	if !rep.OverlayFree {
		t.Fatalf("the surface still has an overlay over the video: %s", rep.Viewport)
	}
	if rep.Verification != "" {
		t.Fatalf("playback verification reported a problem: %s", rep.Verification)
	}
}

// envOr returns the environment value or a default.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
