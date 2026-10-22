package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTabCaptureGrantsRealTab is the regression guard for the green-screen bug:
// Chromium answers getDisplayMedia with either the real media tab
// (label "web-contents-media-stream://…") or — when the fake-device/fake-ui
// switches are set — a synthetic source that renders Chromium's green test
// pattern. Only the former actually shares the video.
func TestTabCaptureGrantsRealTab(t *testing.T) {
	if os.Getenv("TELEMEETBOT_BROWSER") == "" {
		t.Skip("set TELEMEETBOT_BROWSER=/path/to/chrome to run browser integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rep, err := TestTabCapture(ctx, BrowserConfig{Headless: true}, MeetOptions{
		PresentMode:   "tab",
		CaptureSource: "TeleMeet Media",
	}, noopLogger{})
	t.Logf("capture report: video=%v label=%q px=%q err=%q", rep.Video, rep.Label, rep.Px, rep.Error)
	if err != nil {
		t.Fatalf("tab capture was denied: %v", err)
	}
	if !strings.HasPrefix(rep.Label, "web-contents-media-stream") {
		t.Fatalf("captured source is %q (px=%s), want the real media tab "+
			"(web-contents-media-stream). A fake capture device is being used — "+
			"check that use-fake-device-for-media-stream and "+
			"use-fake-ui-for-media-stream are NOT set in launchBrowser", rep.Label, rep.Px)
	}
}
