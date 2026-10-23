package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// PublishReport measures the second half of a share: the part between "the
// captured tab paints pixels" and "a remote participant's decoder shows moving
// video". A live track plus a painting local <video> element proves nothing
// about publication — Meet hands the getDisplayMedia track to a WebRTC sender,
// and that sender can still emit zero frames (accepting the track, negotiating a
// sendonly m-line and then producing no RTP) while the DOM says "you are
// presenting".
//
// Every field is read from a real Chromium running the session's own launch
// flags; nothing here is simulated.
type PublishReport struct {
	Stage     string `json:"stage"`     // last stage reached (diagnosable on failure)
	Media     string `json:"media"`     // media URL, or "canvas-fixture"
	MediaKind string `json:"mediaKind"` // direct | youtube | page

	Label      string  `json:"label"` // capture source label (must be a tab stream)
	TrackState string  `json:"trackState"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	FrameRate  float64 `json:"frameRate"`

	CaptureFrames int     `json:"captureFrames"` // frames the track delivered to a sink
	CaptureFPS    float64 `json:"captureFps"`

	// Encoder side: outbound-rtp, i.e. what actually left the browser.
	EncFrames       float64 `json:"encFrames"`
	EncFPS          float64 `json:"encFps"`
	BytesSent       float64 `json:"bytesSent"`
	BytesPerSec     float64 `json:"bytesPerSec"`
	KeyFrames       float64 `json:"keyFrames"`
	EncoderImpl     string  `json:"encoderImpl"`
	QualityLimit    string  `json:"qualityLimitation"`
	MediaSrcFrames  float64 `json:"mediaSrcFrames"`
	MediaSrcFPS     float64 `json:"mediaSrcFps"`
	MediaSrcWidth   int     `json:"mediaSrcWidth"`
	MediaSrcHeight  int     `json:"mediaSrcHeight"`
	ReceiverFrames  float64 `json:"recvFrames"`
	ReceiverDecoded float64 `json:"recvDecoded"`

	TrDirection string `json:"transceiverDirection"`
	SendCodec   string `json:"sendCodec"`
	OfferVideo  string `json:"offerVideo"` // m=video line + direction from the SDP

	// Remote side: pixels produced by the receiving decoder.
	RemoteMean       float64 `json:"remoteMean"`
	RemoteDelta      float64 `json:"remoteDelta"`
	RemoteBlackRatio float64 `json:"remoteBlackRatio"`
	RemoteFrames     int     `json:"remoteFrames"`

	// Cost of the measured window (whole container cgroup, so it includes the
	// browser processes): CPU seconds, % of one core, and anonymous RSS.
	CPUSeconds float64 `json:"cpuSeconds"`
	CPUPercent float64 `json:"cpuPercent"`
	MemAnonMB  float64 `json:"memAnonMB"`

	ICEState  string `json:"iceState"`
	ConnState string `json:"connState"`
	Error     string `json:"error"`
	Console   string `json:"console"`
}

// Pass reports whether the capture was genuinely published as moving video.
func (r PublishReport) Pass() bool {
	return strings.HasPrefix(r.Label, "web-contents-media-stream") &&
		r.TrackState == "live" &&
		r.EncFrames >= 10 &&
		r.BytesPerSec >= 5000 &&
		r.MediaSrcFPS >= 5 &&
		r.RemoteBlackRatio <= 0.5 &&
		r.RemoteDelta >= 0.5
}

// describe renders the report as one structured log line.
func (r PublishReport) Describe() string {
	return fmt.Sprintf("stage=%s label=%q track=%s %dx%d@%.0f capture=%.1ffps enc=%.0ff(%0.1ffps) bytes=%0.0fB/s key=%0.0f encoder=%q qlim=%q src=%.0ff/%0.1ffps %dx%d recv=%.0f/%0.0f remote=mean%.1f delta%.2f black%.0f%% tr=%s codec=%s ice=%s conn=%s err=%q",
		r.Stage, r.Label, r.TrackState, r.Width, r.Height, r.FrameRate,
		r.CaptureFPS, r.EncFrames, r.EncFPS, r.BytesPerSec, r.KeyFrames,
		r.EncoderImpl, r.QualityLimit, r.MediaSrcFrames, r.MediaSrcFPS,
		r.MediaSrcWidth, r.MediaSrcHeight, r.ReceiverFrames, r.ReceiverDecoded,
		r.RemoteMean, r.RemoteDelta, r.RemoteBlackRatio*100,
		r.TrDirection, r.SendCodec, r.ICEState, r.ConnState, r.Error)
}

// PublishProbeOptions configures PublishProbe.
type PublishProbeOptions struct {
	// MediaURL opens a real media page as the capture target (the same flow a
	// session uses). Empty means the self-contained animated canvas fixture —
	// useful to separate "the pipeline is broken" from "this page is broken".
	MediaURL string
	// Seconds is the measurement window for the encoder/RTP counters.
	Seconds int
}

// PublishProbe launches Chromium with the session's launch flags, opens the
// capture target (a real media link or the canvas fixture), captures it with
// getDisplayMedia, publishes it through an RTCPeerConnection loopback and
// measures the encoded RTP and the decoded remote frames.
func PublishProbe(ctx context.Context, browserCfg BrowserConfig, meet MeetOptions, opt PublishProbeOptions, log Logger) (PublishReport, error) {
	if log == nil {
		log = noopLogger{}
	}
	if opt.Seconds < 3 {
		opt.Seconds = 3
	}
	rep := PublishReport{Media: "canvas-fixture"}
	b, closeFn, err := launchBrowser(launchOptions{browser: browserCfg, meet: meet}, log)
	if err != nil {
		return rep, err
	}
	defer closeFn()

	// --- capture target -----------------------------------------------------
	mediaPage, err := newPage(b, browserCfg)
	if err != nil {
		return rep, err
	}
	targetURL := ""
	if strings.TrimSpace(opt.MediaURL) == "" {
		targetURL, err = players.playerURL(captureTestDoc())
		if err != nil {
			return rep, err
		}
	} else {
		mtURL, mt, err := mediaTargetFor(opt.MediaURL)
		if err != nil {
			return rep, err
		}
		rep.Media = mtURL
		rep.MediaKind = string(mt.Kind)
		targetURL = mtURL
		// Same anti-throttle prelude a session installs before navigating.
		_, _ = mediaPage.EvalOnNewDocument(`
			Object.defineProperty(document, 'hidden', { value: false, writable: false });
			Object.defineProperty(document, 'visibilityState', { value: 'visible', writable: false });
			window.addEventListener('visibilitychange', e => e.stopImmediatePropagation(), true);
			window.__allowPause = false;
			const origPause = HTMLMediaElement.prototype.pause;
			HTMLMediaElement.prototype.pause = function() {
				if (!window.__allowPause) return;
				return origPause.apply(this, arguments);
			};
		`)
	}
	if err := mediaPage.Context(ctx).Timeout(45 * time.Second).Navigate(targetURL); err != nil {
		return rep, fmt.Errorf("open capture target: %w", err)
	}
	_ = mediaPage.WaitLoad()
	_ = mediaPage.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: widthOf(browserCfg), Height: heightOf(browserCfg), DeviceScaleFactor: 1,
	})

	if rep.Media != "canvas-fixture" {
		if res, cerr := dismissConsent(mediaPage, log, 20*time.Second); cerr != nil {
			log.Warnf("[PIPELINE:publish] consent check: %v", cerr)
		} else if res.Present {
			log.Infof("[PIPELINE:publish] consent: %s", res.describe())
		}
		if fit, ferr := fitMediaTab(mediaPage, browserCfg, log); ferr != nil {
			log.Warnf("[PIPELINE:publish] fit capture target: %v", ferr)
		} else {
			log.Infof("[PIPELINE:publish] capture target surface: %s", fit.describe())
		}
		if _, perr := mediaPage.Timeout(6 * time.Second).Eval(mediaControlJS("unmute", 0)); perr != nil {
			log.Debugf("[PIPELINE:publish] unmute: %v", perr)
		}
		if ver, perr := ensurePlayback(mediaPage, log, 1.0, 20*time.Second); perr != nil {
			log.Warnf("[PIPELINE:publish] playback not verified on the capture target: %v", perr)
		} else {
			log.Infof("[PIPELINE:publish] capture target playing: %s", ver.describe())
		}
	}

	// The auto-select matcher works on Chromium's own tab title.
	if _, err := mediaPage.Eval(titleLockJS(captureSource(browserCfg, meet))); err != nil {
		return rep, err
	}
	time.Sleep(1200 * time.Millisecond)
	tabs := pageTitles(b)
	log.Infof("[PIPELINE:publish] chromium tabs=%v wanted=%q", tabs, captureSource(browserCfg, meet))

	// --- publisher ----------------------------------------------------------
	probe, err := newPage(b, browserCfg)
	if err != nil {
		return rep, err
	}
	defer func() { _ = probe.Close() }()
	var console []string
	go probe.EachEvent(
		func(e *proto.RuntimeConsoleAPICalled) {
			var parts []string
			for _, a := range e.Args {
				parts = append(parts, a.Value.Str())
			}
			console = append(console, strings.Join(parts, " "))
		},
		func(e *proto.LogEntryAdded) {
			if e.Entry.Level == proto.LogLogEntryLevelError || e.Entry.Level == proto.LogLogEntryLevelWarning {
				console = append(console, string(e.Entry.Level)+": "+e.Entry.Text)
			}
		},
	)()
	probeTarget, err := players.playerURL(`<!doctype html><html><head><meta charset="utf-8"><title>meet probe</title></head><body>probe</body></html>`)
	if err != nil {
		return rep, err
	}
	if err := probe.Timeout(30 * time.Second).Navigate(probeTarget); err != nil {
		return rep, err
	}
	cpu0, _ := cgroupCPUSeconds()
	t0 := time.Now()
	res, err := probe.Timeout(60*time.Second + 4*time.Duration(opt.Seconds)*4*time.Second).Eval(publishMeasureJS())
	if err != nil {
		return rep, fmt.Errorf("publish measurement: %w", err)
	}
	if err := json.Unmarshal([]byte(res.Value.Str()), &rep); err != nil {
		return rep, fmt.Errorf("decode publish report: %w", err)
	}
	if cpu1, cerr := cgroupCPUSeconds(); cerr == nil && cpu0 >= 0 {
		rep.CPUSeconds = math.Round((cpu1-cpu0)*100) / 100
		if el := time.Since(t0).Seconds(); el > 0 {
			rep.CPUPercent = math.Round(rep.CPUSeconds/el*1000) / 10
		}
	}
	rep.MemAnonMB = cgroupAnonMB()
	if len(console) > 0 {
		rep.Console = strings.Join(console, " | ")
	}
	log.Infof("[PIPELINE:publish] %s", rep.Describe())
	if rep.Error != "" {
		log.Warnf("[PIPELINE:publish] capture/encode/publication failure at stage %q: %s", rep.Stage, rep.Error)
	}
	return rep, nil
}

// pageTitles lists the browser-side (WebContents) titles, which is what
// --auto-select-tab-capture-source-by-title matches.
func pageTitles(b *rod.Browser) []string {
	infos, err := proto.TargetGetTargets{}.Call(b)
	if err != nil {
		return nil
	}
	var out []string
	for _, ti := range infos.TargetInfos {
		if ti.Type == proto.TargetTargetInfoTypePage {
			out = append(out, ti.Title)
		}
	}
	return out
}

// cgroupCPUSeconds reads the container's accumulated CPU time (cgroup v1 or v2).
// It returns -1 when no accounting file is readable (non-Linux dev hosts).
func cgroupCPUSeconds() (float64, error) {
	for _, path := range []string{"/sys/fs/cgroup/cpu.stat", "/sys/fs/cgroup/cpuacct/cpuacct.usage"} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "usage_usec" {
				if v, err := strconv.ParseFloat(f[1], 64); err == nil {
					return v / 1e6, nil
				}
			}
		}
		// cgroup v1: raw nanoseconds on the first line.
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			return v / 1e9, nil
		}
	}
	return -1, fmt.Errorf("no cgroup cpu accounting")
}

// cgroupAnonMB reports the container's anonymous memory (what the browser
// processes actually cost), in MiB.
func cgroupAnonMB() float64 {
	for _, path := range []string{"/sys/fs/cgroup/memory.stat", "/sys/fs/cgroup/memory/memory.stat"} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "anon" {
				if v, err := strconv.ParseFloat(f[1], 64); err == nil {
					return math.Round(v/1024/1024*10) / 10
				}
			}
		}
	}
	return 0
}
