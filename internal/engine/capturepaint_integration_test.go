package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTabCapturePaintsNonBlackFrames is the guard for dropping SwiftShader: the
// CPU fix (--disable-gpu, software compositing) must still deliver real,
// changing pixels through getDisplayMedia. A black or frozen capture would
// otherwise only show up as a blank share inside a live meeting.
func TestTabCapturePaintsNonBlackFrames(t *testing.T) {
	if os.Getenv("TELEMEETBOT_BROWSER") == "" {
		t.Skip("set TELEMEETBOT_BROWSER=/path/to/chrome to run browser integration tests")
	}
	for _, mode := range []string{GPUModeOff, GPUModeSwiftShader} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			rep, err := TestTabCapture(ctx, BrowserConfig{Headless: true, GPU: mode}, MeetOptions{
				PresentMode:   "tab",
				CaptureSource: "TeleMeet Media",
			}, noopLogger{})
			if err != nil {
				t.Fatalf("tab capture was denied with gpu=%s: %v", mode, err)
			}
			t.Logf("gpu=%s report: video=%v audio=%v frames=%d px=%q mean=%.1f delta=%.2f",
				mode, rep.Video, rep.Audio, rep.Frames, rep.Px, rep.Variance, rep.Delta)
			if !strings.HasPrefix(rep.Label, "web-contents-media-stream") {
				t.Fatalf("captured %q, want the real media tab", rep.Label)
			}
			if rep.Px == "0,0,0" || rep.Px == "" || rep.Px == "err" {
				t.Fatalf("captured page samples black (%q); compositing is broken with gpu=%s", rep.Px, mode)
			}
			if rep.Variance < 20 {
				t.Fatalf("captured frames are too dark/flat (mean=%.1f) with gpu=%s", rep.Variance, mode)
			}
			if rep.Delta < 0.5 {
				t.Fatalf("captured frames are not changing (delta=%.2f) with gpu=%s: the share would be frozen", rep.Delta, mode)
			}
		})
	}
}
