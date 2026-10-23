package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// WebGLReport is the result of a WebGL context probe inside a browser launched
// the way a real Meet session launches it.
type WebGLReport struct {
	OK       bool   `json:"ok"`
	WebGL2   bool   `json:"webgl2"`
	WebGL1   bool   `json:"webgl1"`
	Renderer string `json:"renderer"`
	Draw     string `json:"draw"`
	Error    string `json:"error"`
}

// webglProbeJS returns the page script that asks for a GL context and proves it
// can clear and read back pixels. Function form: rod's Eval calls it.
func webglProbeJS() string {
	return `() => {
  const out = {ok: false, webgl2: false, webgl1: false, renderer: "", draw: "", error: ""};
  try {
    const c = document.createElement("canvas");
    c.width = 64; c.height = 64;
    let gl = c.getContext("webgl2");
    if (gl) { out.webgl2 = true; } else { gl = c.getContext("webgl"); out.webgl1 = !!gl; }
    if (!gl) { out.error = "no gl context"; return JSON.stringify(out); }
    const dbg = gl.getExtension("WEBGL_debug_renderer_info");
    out.renderer = dbg ? String(gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL)) : String(gl.getParameter(gl.RENDERER));
    gl.clearColor(0.2, 0.4, 0.6, 1);
    gl.clear(gl.COLOR_BUFFER_BIT);
    const px = new Uint8Array(4);
    gl.readPixels(0, 0, 1, 1, gl.RGBA, gl.UNSIGNED_BYTE, px);
    out.draw = px[0] + "," + px[1] + "," + px[2];
    out.ok = out.draw !== "" && out.draw !== "0,0,0";
  } catch (e) { out.error = String(e); }
  return JSON.stringify(out);
}`
}

// ProbeWebGL launches Chromium with the flags a real Meet session uses and
// reports whether pages get a working WebGL context. Google Meet needs one to
// publish presentation video; --disable-gpu (browser.gpu: "off") removes it
// entirely, which is exactly the failure mode this probe exists to catch.
func ProbeWebGL(ctx context.Context, browserCfg BrowserConfig, meet MeetOptions, log Logger) (WebGLReport, error) {
	if log == nil {
		log = noopLogger{}
	}
	rep := WebGLReport{}
	b, closeFn, err := launchBrowser(launchOptions{browser: browserCfg, meet: meet, forMeet: true}, log)
	if err != nil {
		return rep, fmt.Errorf("launch chromium: %w", err)
	}
	defer closeFn()
	p, err := b.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return rep, fmt.Errorf("open probe page: %w", err)
	}
	defer func() { _ = p.Close() }()
	res, err := p.Timeout(15 * time.Second).Eval(webglProbeJS())
	if err != nil {
		return rep, fmt.Errorf("probe WebGL: %w", err)
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &rep); err != nil {
		return rep, fmt.Errorf("decode WebGL probe: %w", err)
	}
	return rep, nil
}
