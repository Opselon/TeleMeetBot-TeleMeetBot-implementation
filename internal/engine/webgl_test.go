package engine

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// TestRegressionMeetRequiresWebGL verifies that the compatible configuration
// passed to Chromium provides a working WebGL context (WebGL 2 or WebGL 1).
//
// Root Cause of Production Regression (commit cb9714e):
// When browser.gpu: "off" was enabled by default, Chromium was launched with
// "--disable-gpu". While local getDisplayMedia and WebRTC audio continued to
// function, disabling the GPU completely tore down Chromium's WebGL rendering
// context (getContext("webgl") returning null). Google Meet requires a functional
// WebGL context to composite and initialize its video presentation stream;
// without it, Meet failed to transmit video frames to remote participants while
// continuing to transmit audio.
//
// This test asserts that when forMeet=true, the resolved launch configuration
// provides a valid WebGL context and successfully draws and reads back pixels.
func TestRegressionMeetRequiresWebGL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser regression test in short mode")
	}
	browserBin(t)

	// Even if cfg has GPU: "off", launchOptions with forMeet: true upgrades to
	// compatible mode to ensure WebGL is available for Google Meet.
	cfg := BrowserConfig{
		Executable: os.Getenv("TELEMEETBOT_BROWSER"),
		Headless:   true,
		GPU:        GPUModeOff,
	}
	meet := MeetOptions{PresentMode: "tab", CaptureSource: "TeleMeet Media"}
	b, closeFn, err := launchBrowser(launchOptions{browser: cfg, meet: meet, forMeet: true}, noopLogger{})
	if err != nil {
		t.Fatalf("failed to launch browser: %v", err)
	}
	defer closeFn()

	p, err := b.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("failed to open page: %v", err)
	}
	defer func() { _ = p.Close() }()

	res, err := p.Timeout(15 * time.Second).Eval(`() => {
		const out = {webgl2: false, webgl1: false, renderer: "", draw: "", err: ""};
		try {
			const c = document.createElement("canvas");
			c.width = 64;
			c.height = 64;
			let gl = c.getContext("webgl2");
			if (gl) {
				out.webgl2 = true;
			} else {
				gl = c.getContext("webgl");
				out.webgl1 = !!gl;
			}
			if (!gl) {
				out.err = "no gl context";
				return JSON.stringify(out);
			}
			const dbg = gl.getExtension("WEBGL_debug_renderer_info");
			out.renderer = dbg ? String(gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL)) : String(gl.getParameter(gl.RENDERER));
			gl.clearColor(0.2, 0.4, 0.6, 1.0);
			gl.clear(gl.COLOR_BUFFER_BIT);
			const px = new Uint8Array(4);
			gl.readPixels(0, 0, 1, 1, gl.RGBA, gl.UNSIGNED_BYTE, px);
			out.draw = px[0] + "," + px[1] + "," + px[2];
		} catch (e) {
			out.err = String(e);
		}
		return JSON.stringify(out);
	}`)
	if err != nil {
		t.Fatalf("failed to evaluate WebGL probe: %v", err)
	}

	var rep struct {
		Webgl2   bool   `json:"webgl2"`
		Webgl1   bool   `json:"webgl1"`
		Renderer string `json:"renderer"`
		Draw     string `json:"draw"`
		Err      string `json:"err"`
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &rep); err != nil {
		t.Fatalf("failed to parse WebGL probe result: %v", err)
	}

	t.Logf("WebGL check for Meet: %+v", rep)
	if !rep.Webgl2 && !rep.Webgl1 {
		t.Fatalf("regression: Meet browser configuration lacks WebGL context (err=%q)", rep.Err)
	}
	if rep.Draw == "" || rep.Draw == "0,0,0" {
		t.Fatalf("regression: WebGL clear failed to render pixels: draw=%q", rep.Draw)
	}
}
