package engine

import "fmt"

// Consent + presentation fixtures.
//
// These documents reproduce the shapes the bot meets in the wild: a YouTube watch
// page whose "Before you continue to YouTube" lightbox is mounted inside
// ytd-consent-bump-v2-lightbox / tp-yt-paper-dialog, full-sentence aria-labels
// instead of the literal string "reject all", per-locale button text, and the
// decoy controls ("Customize", "More options", "Learn more") that must never be
// clicked in place of reject.

// consentFixture describes one consent wall variant.
type consentFixture struct {
	Name   string // test name
	Locale string // what the label language is, for diagnostics
	Reject string // reject-all label; empty means the wall has no reject control
	Accept string // accept-all label; empty means no accept control
	Aria   bool   // use long full-sentence aria-labels (the real YouTube shape)
	Shadow bool   // render the dialog inside a shadow root
	Inert  bool   // buttons exist but do nothing (a wall that cannot be cleared)
	Decoys bool   // add Customize / More options / Learn more decoys
	NoAria bool   // label only in innerText
}

// consentFixtureDoc renders a YouTube-shaped watch page carrying the wall.
func consentFixtureDoc(f consentFixture) string {
	aria := func(label string) string {
		if !f.Aria || f.NoAria {
			return ""
		}
		// Real walls carry full-sentence aria-labels in the UI language — the
		// shape that broke the old exact-match matcher.
		switch label {
		case f.Reject:
			if f.Locale == "" || f.Locale == "en" {
				return ` aria-label="Reject the use of cookies and other data for the purposes described"`
			}
			return fmt.Sprintf(` aria-label="%s"`, label)
		case f.Accept:
			if f.Locale == "" || f.Locale == "en" {
				return ` aria-label="Accept the use of cookies and other data for the purposes described"`
			}
			return fmt.Sprintf(` aria-label="%s"`, label)
		}
		return ""
	}
	btn := func(id, label, extra string) string {
		if label == "" {
			return ""
		}
		return fmt.Sprintf(`<button id="%s" class="yt-spec-button-shape-next" style="padding:12px 20px;margin:6px"%s>%s%s</button>`,
			id, aria(label), label, extra)
	}
	action := func(id string, reject bool) string {
		if f.Inert {
			return ""
		}
		kind := "accept"
		if reject {
			kind = "reject"
		}
		// Real walls dismiss on click; record what was clicked so the test can
		// prove decoys were not used.
		return fmt.Sprintf(`document.getElementById('%s').addEventListener('click', () => {
		  window.__clicks = window.__clicks || []; window.__clicks.push('%s');
		  const lb = document.querySelector('ytd-consent-bump-v2-lightbox'); if (lb) lb.remove();
		});`, id, kind)
	}
	decoys := ""
	if f.Decoys {
		decoys = `<button id="customize" style="padding:12px 20px;margin:6px">Customize</button>
<button id="more" aria-label="More options" style="padding:12px 20px;margin:6px">More options</button>
<a id="learn" href="#" style="padding:12px 20px;margin:6px">Learn more</a>`
	}

	dialog := fmt.Sprintf(`<tp-yt-paper-dialog role="dialog" aria-labelledby="cb-header" style="display:flex;flex-direction:column;width:700px;height:700px;background:#fff;color:#111;padding:24px;box-sizing:border-box">
  <h1 id="cb-header" style="font-size:22px">Before you continue to YouTube</h1>
  <p>We use cookies and data to deliver and maintain Google services.</p>
  <div id="actions">%s%s%s</div>
</tp-yt-paper-dialog>`, btn("reject", f.Reject, ""), btn("accept", f.Accept, ""), decoys)

	// A zero-height host with a visible dialog child is the real YouTube shape:
	// the wrapper reports 0x0 while the dialog inside is 700x700.
	mount := fmt.Sprintf(`<ytd-consent-bump-v2-lightbox id="bump" style="display:block;height:0">
<tp-yt-iron-overlay-backdrop style="position:fixed;inset:0;background:rgba(0,0,0,.5)"></tp-yt-iron-overlay-backdrop>
%s
</ytd-consent-bump-v2-lightbox>`, dialog)

	script := action("reject", true) + action("accept", false)
	if f.Shadow {
		script += `
const host = document.createElement('div');
host.id = 'shadow-host';
host.style.cssText = 'position:fixed;inset:0;z-index:1000';
document.getElementById('bump').remove();
document.body.appendChild(host);
const root = host.attachShadow({mode: 'open'});
root.innerHTML = ` + "`" + mount + "`" + `;
root.querySelector('#reject') && root.querySelector('#reject').addEventListener('click', () => {
  window.__clicks = window.__clicks || []; window.__clicks.push('reject'); host.remove();
});`
	}

	// A wall exists only when it actually offers a decision. An empty fixture is
	// a page with no consent surface at all.
	wall := ""
	if f.Reject != "" || f.Accept != "" {
		wall = mount
	}

	return fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>Me at the zoo - YouTube</title>
<style>
 html,body{margin:0;background:#0f0f0f;color:#fff;font-family:sans-serif}
 #masthead-container{height:56px;background:#202020}
 #primary{width:900px;height:600px;background:#111}
 #secondary{float:right;width:340px;height:600px;background:#181818}
 ytd-consent-bump-v2-lightbox tp-yt-paper-dialog{position:fixed;left:290px;top:0;z-index:999}
</style></head>
<body>
<div id="masthead-container">masthead</div>
<div id="secondary">related videos</div>
<div id="primary"><div id="player-container"><div id="movie_player"><video id="v" muted playsinline></video></div></div></div>
%s
<script>
window.__clicks = [];
%s
</script>
</body></html>`, wall, script)
}

// mediaPresentationDoc is a YouTube-shaped watch page with a genuinely playing
// video (canvas captureStream into a video element) and a wall that can be
// mounted later, so a test can prove recovery works after presentation started.
func mediaPresentationDoc() string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>TeleMeet Media</title>
<style>
 html,body{margin:0;background:#0f0f0f;overflow:hidden;color:#fff;font-family:sans-serif}
 #masthead-container{height:56px;background:#202020}
 #primary{width:900px}
 #secondary{float:right;width:340px;height:700px;background:#181818}
 #movie_player video{width:900px;height:506px;background:#000}
</style></head>
<body>
<div id="masthead-container">masthead</div>
<div id="secondary">related videos</div>
<div id="primary"><div id="player-container"><div id="movie_player"><video id="v" muted playsinline></video></div></div></div>
<script>
let frame = 0;
const c = document.createElement('canvas');
c.width = 640; c.height = 360;
const ctx = c.getContext('2d');
function draw() {
  frame++;
  const g = ctx.createLinearGradient(0, 0, c.width, c.height);
  const hue = (frame * 3) % 360;
  g.addColorStop(0, 'hsl(' + hue + ',70%,55%)');
  g.addColorStop(1, 'hsl(' + ((hue + 120) % 360) + ',70%,45%)');
  ctx.fillStyle = g; ctx.fillRect(0, 0, c.width, c.height);
  const x = (frame * 6) % c.width;
  ctx.fillStyle = '#fff'; ctx.fillRect(x, 120, 90, 90);
  ctx.fillStyle = '#000'; ctx.font = '48px sans-serif'; ctx.fillText('TeleMeet', 40, 60);
  requestAnimationFrame(draw);
}
draw();
const v = document.getElementById('v');
v.srcObject = c.captureStream(30);
v.play().catch(() => {});

// Mount the wall a few seconds late, exactly like YouTube does.
window.__showConsent = (labels) => {
  if (document.querySelector('ytd-consent-bump-v2-lightbox')) return true;
  const mount = document.createElement('ytd-consent-bump-v2-lightbox');
  mount.style.cssText = 'display:block;height:0';
  mount.innerHTML = '<tp-yt-paper-dialog role="dialog" style="position:fixed;left:290px;top:0;width:700px;height:700px;background:#fff;color:#111;z-index:999">'
    + '<h1>Before you continue to YouTube</h1>'
    + '<button id="reject" aria-label="Reject the use of cookies and other data for the purposes described" style="padding:12px 20px">' + labels[0] + '</button>'
    + '<button id="accept" aria-label="Accept the use of cookies and other data for the purposes described" style="padding:12px 20px">' + labels[1] + '</button>'
    + '</tp-yt-paper-dialog>';
  document.body.appendChild(mount);
  const r = mount.querySelector('#reject');
  if (r) r.addEventListener('click', () => mount.remove());
  return true;
};
</script>
</body></html>`
}

// captureMeetDoc requests a display capture (the Meet share) and samples the
// captured frames continuously, recording them for the test to inspect.
func captureMeetDoc() string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>meet probe</title></head>
<body style="margin:0;background:#000">
<script>
window.__cap = {calls: 0, id: "", label: "", error: "", samples: [], ready: false};
(async () => {
  let stream;
  try {
    stream = await navigator.mediaDevices.getDisplayMedia({video: true, audio: false});
  } catch (e) {
    window.__cap.error = String(e); return;
  }
  window.__cap.calls++;
  const t = stream.getVideoTracks()[0];
  window.__cap.id = t.id;
  window.__cap.label = t.label || "";
  const v = document.createElement('video');
  v.srcObject = stream; v.muted = true; v.playsInline = true;
  await v.play().catch(() => {});
  const c = document.createElement('canvas');
  c.width = 64; c.height = 36;
  const ctx = c.getContext('2d');
  const tick = () => {
    try {
      ctx.drawImage(v, 0, 0, c.width, c.height);
      const d = ctx.getImageData(0, 0, c.width, c.height).data;
      let sum = 0;
      for (let i = 0; i < d.length; i += 4) sum += d[i] + d[i + 1] + d[i + 2];
      window.__cap.samples.push({t: Date.now(), mean: sum / (d.length / 4), live: t.readyState, w: v.videoWidth, h: v.videoHeight});
      window.__cap.ready = true;
    } catch (e) {
      window.__cap.error = String(e);
    }
    setTimeout(tick, 250);
  };
  setTimeout(tick, 400);
})();
</script></body></html>`
}
