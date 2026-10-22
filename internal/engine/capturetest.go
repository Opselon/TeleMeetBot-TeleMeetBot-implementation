package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

// captureProbeURL is the secure-context page that requests getDisplayMedia
// (mediaDevices is unavailable on about:blank/data: pages). Overridable in tests.
var captureProbeURL = "https://example.com/"

// CaptureReport is the outcome of a real tab-capture probe.
type CaptureReport struct {
	Video    bool    `json:"video"`
	Audio    bool    `json:"audio"`
	Frames   int     `json:"frames"`
	Variance float64 `json:"variance"`
	Label    string  `json:"label"`
	Px       string  `json:"px"`
	Error    string  `json:"error"`
}

// TestTabCapture launches Chromium exactly like a real session and verifies
// that getDisplayMedia is auto-granted to the named media tab. This is the same
// code path Google Meet uses when the bot clicks "Present now" → "A tab", so a
// passing report means the share will work without a screen picker.
func TestTabCapture(ctx context.Context, browserCfg BrowserConfig, meet MeetOptions, log Logger) (CaptureReport, error) {
	if log == nil {
		log = noopLogger{}
	}
	b, closeFn, err := launchBrowser(launchOptions{browser: browserCfg, meet: meet}, log)
	if err != nil {
		return CaptureReport{}, err
	}
	defer closeFn()

	// 1) The media tab: animated canvas + a web-audio oscillator + pinned title.
	mediaPage, err := newPage(b, browserCfg)
	if err != nil {
		return CaptureReport{}, err
	}
	if err := mediaPage.Timeout(30 * time.Second).Navigate(captureTestDoc()); err != nil {
		return CaptureReport{}, fmt.Errorf("open capture test page: %w", err)
	}
	if err := mediaPage.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: widthOf(browserCfg), Height: heightOf(browserCfg), DeviceScaleFactor: 1,
	}); err != nil {
		log.Warnf("capture test viewport: %v", err)
	}
	if _, err := mediaPage.Eval(titleLockJS(captureSource(browserCfg, meet))); err != nil {
		return CaptureReport{}, err
	}
	time.Sleep(1200 * time.Millisecond)

	// 2) The "Meet" side asks for the display stream.
	probe, err := newPage(b, browserCfg)
	if err != nil {
		return CaptureReport{}, err
	}
	defer func() { _ = probe.Close() }()
	if err := probe.Timeout(30 * time.Second).Navigate(captureProbeURL); err != nil {
		return CaptureReport{}, err
	}
	select {
	case <-ctx.Done():
		return CaptureReport{}, ctx.Err()
	default:
	}

	log.Debugf("capture test: media page url=%s", mediaPage.MustInfo().URL)
	pre := ""
	if r, e := probe.Eval("() => String(navigator.mediaDevices) + '|' + location.href"); e == nil {
		pre = r.Value.Str()
	}
	log.Debugf("capture test: probe page %s", pre)

	js := `() => new Promise(async (resolve) => {
  const out = {video: false, audio: false, frames: 0, variance: 0, label: "", error: ""};
  let stream;
  try {
    stream = await navigator.mediaDevices.getDisplayMedia({video: true, audio: true});
  } catch (e) {
    try {
      stream = await navigator.mediaDevices.getDisplayMedia({video: true});
      out.error = "video-only grant: " + String(e);
    } catch (e2) {
      out.error = String(e2);
      resolve(JSON.stringify(out));
      return;
    }
  }
  const v = stream.getVideoTracks()[0];
  out.video = !!v && v.readyState === "live";
  out.label = v ? (v.label || "") : "";
  try {
    const e0 = document.createElement("video");
    e0.srcObject = new MediaStream([v]);
    e0.muted = true;
    await e0.play();
    await new Promise(r => setTimeout(r, 700));
    const c0 = document.createElement("canvas");
    c0.width = 32; c0.height = 18;
    const x0 = c0.getContext("2d");
    x0.drawImage(e0, 0, 0, 32, 18);
    const d0 = x0.getImageData(16, 9, 1, 1).data;
    out.px = d0[0] + "," + d0[1] + "," + d0[2];
  } catch (e) { out.px = "err"; }
  out.audio = stream.getAudioTracks().length > 0;
  if (!v) { resolve(JSON.stringify(out)); return; }

  const el = document.createElement("video");
  el.srcObject = new MediaStream([v]);
  el.muted = true;
  el.playsInline = true;
  try { await el.play(); } catch (e) {}
  await new Promise(r => setTimeout(r, 1500));

  try {
    const c = document.createElement("canvas");
    c.width = 96; c.height = 54;
    const ctx = c.getContext("2d");
    let sum = 0, frames = 0;
    for (let i = 0; i < 4; i++) {
      ctx.drawImage(el, 0, 0, c.width, c.height);
      const d = ctx.getImageData(0, 0, c.width, c.height).data;
      let local = 0;
      for (let p = 0; p < d.length; p += 4) local += d[p] + d[p+1] + d[p+2];
      sum += local / (d.length / 4);
      frames++;
      await new Promise(r => setTimeout(r, 250));
    }
    out.frames = frames;
    out.variance = sum / frames;
  } catch (e) {
    out.error = "frame sampling failed: " + String(e);
  }
  stream.getTracks().forEach(t => t.stop());
  resolve(JSON.stringify(out));
})`
	res, err := probe.Timeout(40 * time.Second).Eval(js)
	if err != nil {
		return CaptureReport{}, fmt.Errorf("getDisplayMedia probe failed: %w", err)
	}
	var rep CaptureReport
	if err := json.Unmarshal([]byte(res.Value.Str()), &rep); err != nil {
		return CaptureReport{}, fmt.Errorf("decode capture probe: %w", err)
	}
	if !rep.Video {
		return rep, fmt.Errorf("display capture was not granted (%s)", rep.Error)
	}
	return rep, nil
}

// captureTestDoc is a self-contained page with animated pixels and audio.
func captureTestDoc() string {
	doc := `<!doctype html><html><head><meta charset="utf-8"><title>TeleMeet Media</title>
<style>html,body{margin:0;background:#000;overflow:hidden}canvas{display:block;width:100vw;height:100vh}</style>
</head><body><canvas id="c" width="1280" height="720"></canvas>
<script>
(() => {
  const c = document.getElementById('c');
  const ctx = c.getContext('2d');
  let t = 0;
  const draw = () => {
    t += 0.03;
    const g = ctx.createLinearGradient(0, 0, c.width, c.height);
    g.addColorStop(0, 'hsl(' + ((t * 40) % 360) + ',70%,45%)');
    g.addColorStop(1, 'hsl(' + ((t * 40 + 120) % 360) + ',70%,25%)');
    ctx.fillStyle = g;
    ctx.fillRect(0, 0, c.width, c.height);
    ctx.fillStyle = '#fff';
    ctx.font = 'bold 96px system-ui, sans-serif';
    ctx.textAlign = 'center';
    ctx.fillText('TeleMeetBot capture test', c.width / 2, c.height / 2);
    ctx.font = '48px system-ui, sans-serif';
    ctx.fillText(new Date().toISOString(), c.width / 2, c.height / 2 + 90);
    requestAnimationFrame(draw);
  };
  draw();
  // A quiet oscillator so the captured tab has an audio track.
  try {
    const ac = new (window.AudioContext || window.webkitAudioContext)();
    const osc = ac.createOscillator();
    const gain = ac.createGain();
    gain.gain.value = 0.02;
    osc.frequency.value = 220;
    osc.connect(gain).connect(ac.destination);
    osc.start();
  } catch (e) {}
  const lock = () => { if (document.title !== 'TeleMeet Media') document.title = 'TeleMeet Media'; };
  lock(); setInterval(lock, 700);
})();
</script></body></html>`
	return "data:text/html;charset=utf-8," + escapeDataURL(doc)
}

// escapeDataURL percent-encodes a document for a data: URL.
func escapeDataURL(s string) string {
	const hex = "0123456789ABCDEF"
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '!', c == '~', c == '*', c == '\'', c == '(', c == ')':
			b = append(b, c)
		default:
			b = append(b, '%', hex[c>>4], hex[c&15])
		}
	}
	return string(b)
}
