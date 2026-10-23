package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTabCapturePublishesVideoFrames is the regression guard the perf commit was
// missing. --disable-gpu must not only leave pixels in a local <video> element:
// the captured track has to survive the WebRTC sender (encoding, RTP, decoding),
// otherwise participants see a black rectangle while every local check passes.
//
// It runs the real publication path (getDisplayMedia -> RTCPeerConnection ->
// loopback receiver) with the session's own launch flags for every GPU mode.
func TestTabCapturePublishesVideoFrames(t *testing.T) {
	browserBin(t)
	for _, mode := range []string{GPUModeOff, GPUModeSwiftShader} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
			defer cancel()
			rep, err := PublishProbe(ctx, BrowserConfig{
				Executable: os.Getenv("TELEMEETBOT_BROWSER"),
				Headless:   true,
				GPU:        mode,
			}, MeetOptions{PresentMode: "tab", CaptureSource: "TeleMeet Media"}, PublishProbeOptions{Seconds: 4}, testLogger{t})
			if err != nil {
				t.Fatalf("publish probe: %v", err)
			}
			if !rep.Pass() {
				t.Fatalf("capture was not published as live video with gpu=%s: %s", mode, rep.Describe())
			}
			if !strings.HasPrefix(rep.Label, "web-contents-media-stream") {
				t.Errorf("captured %q, want the media tab", rep.Label)
			}
			t.Logf("gpu=%s %s", mode, rep.Describe())
		})
	}
}
