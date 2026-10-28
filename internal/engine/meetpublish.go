package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Publication diagnostics for the Meet side of a session.
//
// The engine cannot see Meet's internals, so it installs a small hook before the
// Meet app boots: it records every RTCPeerConnection Meet creates and keeps the
// stream Meet got from getDisplayMedia. That makes the difference measurable
// between "the tab paints frames", "Meet captured a stream", "Meet's sender
// encoded frames" and "participants received video" — the four steps a share can
// break at, three of which the old diagnostics could not see at all.

// meetPeerHookJS runs at document start on the Meet tab. It must never throw:
// a broken hook would break the meeting join it is supposed to diagnose.
func meetPeerHookJS() string {
	return `(() => {
  try {
    if (window.__tmHook) return;
    window.__tmHook = true;
    window.__tmPCs = [];
    window.__tmDisplay = null;
    window.__tmConsole = [];
    window.__tmSrcFrames = 0;

    const push = (level, text) => {
      try {
        window.__tmConsole.push(level + ": " + String(text).slice(0, 200));
        if (window.__tmConsole.length > 40) window.__tmConsole.shift();
      } catch (e) {}
    };
    const cerr = console.error;
    console.error = function () {
      try { push("error", Array.from(arguments).join(" ")); } catch (e) {}
      return cerr.apply(this, arguments);
    };

    const Orig = window.RTCPeerConnection;
    if (Orig) {
      const Wrapped = function (...args) {
        const pc = new Orig(...args);
        try { window.__tmPCs.push(pc); } catch (e) {}
        return pc;
      };
      Wrapped.prototype = Orig.prototype;
      try { Object.setPrototypeOf(Wrapped, Orig); } catch (e) {}
      try {
        window.RTCPeerConnection = Wrapped;
        if (window.webkitRTCPeerConnection === Orig) window.webkitRTCPeerConnection = Wrapped;
      } catch (e) { push("error", "pc hook: " + e); }
    }

    const md = navigator.mediaDevices;
    if (md && md.getDisplayMedia) {
      const gdm = md.getDisplayMedia.bind(md);
      md.getDisplayMedia = async function (...args) {
        const stream = await gdm(...args);
        try {
          window.__tmDisplay = stream;
          window.__tmDisplayAt = Date.now();
          // Count the frames the capture track actually delivers inside Meet's
          // own renderer. A live track that never produces frames is exactly
          // what "participants see nothing, audio works" looks like.
          const track = stream.getVideoTracks()[0];
          if (track) {
            try { track.contentHint = "motion"; } catch (e) {}
          }
          if (track && window.MediaStreamTrackProcessor && !window.__tmSrcReader) {
            const proc = new MediaStreamTrackProcessor({track});
            const reader = proc.readable.getReader();
            window.__tmSrcReader = reader;
            (async () => {
              try {
                for (;;) {
                  const {value, done} = await reader.read();
                  if (done) break;
                  window.__tmSrcFrames++;
                  if (value && value.close) value.close();
                }
              } catch (e) { push("error", "src reader: " + e); }
            })();
          }
        } catch (e) { push("error", "gdm hook: " + e); }
        return stream;
      };
    }
  } catch (e) {
    try { window.__tmHookError = String(e); } catch (e2) {}
  }
})();`
}

// MeetPublishStats is one reading of Meet's own publication pipeline.
type MeetPublishStats struct {
	Present    bool    `json:"present"`    // Meet shows an active presentation
	PCs        int     `json:"pcs"`        // RTCPeerConnections Meet created
	Display    string  `json:"display"`    // "none" | "live 1280x800@30 | label=…"
	SrcFrames  float64 `json:"srcFrames"`  // frames the capture track delivered in Meet
	SrcFPS     float64 `json:"srcFps"`     // and its rate over the sample window
	Senders    int     `json:"senders"`    // video senders tracked by Meet
	TrackState string  `json:"trackState"` // state of the presentation track on the sender
	EncFrames  float64 `json:"encFrames"`  // frames Meet's encoder produced
	EncFPS     float64 `json:"encFps"`     //
	BytesSent  float64 `json:"bytesSent"`  // video bytes Meet pushed to the SFU
	BitsPerSec float64 `json:"bitsPerSec"`
	Quality    string  `json:"quality"` // qualityLimitationReason
	Encoder    string  `json:"encoder"`
	KeyFrames  float64 `json:"keyFrames"`
	EncWidth   int     `json:"encWidth"`
	EncHeight  int     `json:"encHeight"`
	InVideo    float64 `json:"inVideo"` // inbound video frames (other participants)
	InFPS      float64 `json:"inFps"`
	Console    string  `json:"console"`
	Error      string  `json:"error"`
}

// meetPublishStatsJS reads the hook state plus fresh RTCStats from every
// connection Meet created. Function form (rod Eval), async because getStats is.
func meetPublishStatsJS() string {
	return `() => (async () => {
  const out = {present: false, pcs: 0, display: "none", srcFrames: 0, senders: 0, trackState: "",
    encFrames: 0, bytesSent: 0, quality: "", encoder: "", keyFrames: 0, encWidth: 0, encHeight: 0,
    inVideo: 0, console: (window.__tmConsole || []).slice(-6).join(" | "), error: window.__tmHookError || ""};
  try {
    const body = (((document.body || {}).innerText) || "").toLowerCase();
    out.present = /you are presenting|you're presenting|stop presenting|you are sharing|stop sharing|is presenting|presentation/.test(body) ||
      !!document.querySelector('[aria-label*="Stop presenting" i], [aria-label*="Stop sharing" i]');
    out.srcFrames = window.__tmSrcFrames || 0;
    const disp = window.__tmDisplay;
    if (disp) {
      const track = disp.getVideoTracks()[0];
      if (track) {
        const s = track.getSettings ? (track.getSettings() || {}) : {};
        out.display = "live " + (s.width || 0) + "x" + (s.height || 0) + "@" + (s.frameRate || 0) + " state=" + track.readyState + " muted=" + !!track.muted + " label=" + (track.label || "");
      } else {
        out.display = "no video track";
      }
    }
    const pcs = window.__tmPCs || [];
    out.pcs = pcs.length;
    for (const pc of pcs) {
      let senders = [];
      try { senders = pc.getSenders ? pc.getSenders() : []; } catch (e) {}
      const videoSenders = senders.filter(s => s.track && s.track.kind === "video");
      out.senders += videoSenders.length;
      // Prefer the presentation track when Meet publishes one.
      let sender = videoSenders.find(s => (s.track.label || "").indexOf("web-contents-media-stream") === 0) || videoSenders[0];
      if (!sender) continue;
      if (!out.trackState) out.trackState = sender.track.readyState + " muted=" + !!sender.track.muted + " enabled=" + sender.track.enabled + " label=" + (sender.track.label || "");
      const senderId = sender.mid || "";
      let report = null;
      try { report = await pc.getStats(); } catch (e) { continue; }
      report.forEach(r => {
        if (r.type === "outbound-rtp" && r.kind === "video") {
          // Match the section to our sender when the stats expose a link.
          if (r.mid && senderId && r.mid !== senderId) return;
          out.encFrames += r.framesEncoded || 0;
          out.bytesSent += r.bytesSent || 0;
          out.keyFrames += r.keyFramesEncoded || 0;
          if (r.qualityLimitationReason) out.quality = r.qualityLimitationReason;
          if (r.encoderImplementation) out.encoder = r.encoderImplementation;
          if (r.frameWidth) out.encWidth = r.frameWidth;
          if (r.frameHeight) out.encHeight = r.frameHeight;
        }
        if (r.type === "inbound-rtp" && r.kind === "video") {
          out.inVideo += r.framesReceived || 0;
        }
      });
    }
  } catch (e) {
    out.error = String(e);
  }
  return JSON.stringify(out);
})()`
}

// publishTracker keeps the previous sample so the log can show rates instead of
// monotonic counters (a stalled counter is invisible in a single reading).
type publishTracker struct {
	mu  sync.Mutex
	at  time.Time
	src float64
	enc float64
	byt float64
	in  float64
}

// logMeetPublish samples Meet's publication pipeline and logs a structured line.
// It returns a non-empty problem string when the share is broken in a way the
// engine can name, so the session can surface it instead of reporting success
// from the DOM alone.
func (s *Session) logMeetPublish() string {
	meet := s.meetTab()
	if meet == nil {
		return ""
	}
	res, err := meet.Timeout(6 * time.Second).Eval(meetPublishStatsJS())
	if err != nil {
		s.log.Warnf("[PIPELINE:publish] session %s: read Meet publisher stats: %v", s.ID, err)
		return ""
	}
	var st MeetPublishStats
	if err := json.Unmarshal([]byte(res.Value.Str()), &st); err != nil {
		s.log.Warnf("[PIPELINE:publish] session %s: decode Meet publisher stats: %v", s.ID, err)
		return ""
	}

	now := time.Now()
	s.pub.mu.Lock()
	var dSrc, dEnc, dByt, dIn float64
	first := s.pub.at.IsZero()
	if !first {
		el := now.Sub(s.pub.at).Seconds()
		if el > 0 {
			dSrc = (st.SrcFrames - s.pub.src) / el
			dEnc = (st.EncFrames - s.pub.enc) / el
			dByt = (st.BytesSent - s.pub.byt) / el
			dIn = (st.InVideo - s.pub.in) / el
		}
	}
	s.pub.at, s.pub.src, s.pub.enc, s.pub.byt, s.pub.in = now, st.SrcFrames, st.EncFrames, st.BytesSent, st.InVideo
	s.pub.mu.Unlock()

	s.log.Infof("[PIPELINE:publish] session %s: meet pcs=%d present=%t display=%q srcFrames=%.0f(%.1f/s) senders=%d track=%q encFrames=%.0f(%.1f/s) sentKBps=%.1f key=%0.0f enc=%s quality=%s inVideo=%.1f/s",
		s.ID, st.PCs, st.Present, st.Display, st.SrcFrames, dSrc, st.Senders, st.TrackState,
		st.EncFrames, dEnc, dByt/1024, st.KeyFrames, st.Encoder, st.Quality, dIn)
	if st.Console != "" {
		s.log.Debugf("[PIPELINE:publish] session %s: meet console: %s", s.ID, st.Console)
	}
	if st.Error != "" {
		s.log.Warnf("[PIPELINE:publish] session %s: publisher hook error: %s", s.ID, st.Error)
	}

	// Name the failure stage. Only claim a problem when a sample window exists
	// (rates need two readings) and Meet says it is presenting.
	if first || !st.Present {
		return ""
	}
	if st.Display == "none" {
		return "Meet reports a live presentation but its capture stream is missing (getDisplayMedia was never granted)"
	}
	if st.SrcFrames > 0 && dSrc < 1 {
		return fmt.Sprintf("Meet's capture track stopped delivering frames (%.1f/s): the shared surface is frozen", dSrc)
	}
	if st.Senders == 0 {
		return "Meet has no video sender for the presentation: participants get no video track"
	}
	if st.EncFrames > 0 && dEnc < 1 {
		return fmt.Sprintf("Meet's video encoder produced no frames (encoder=%q quality=%q): participants see a still/black share", st.Encoder, st.Quality)
	}
	return ""
}

// publishProblemClass maps a logMeetPublish problem to the recovery family it
// belongs to. Only classes that a re-present can fix count: Meet dropping the
// share (its senders vanish or the capture stream is torn down). A frozen
// media tab or a dead encoder is a media-side fault and is healed by the media
// recovery path instead, so the share is not needlessly re-clicked.
func publishProblemClass(problem string) string {
	switch {
	case problem == "":
		return ""
	case strings.Contains(problem, "capture stream is missing"),
		strings.Contains(problem, "no video sender"),
		strings.Contains(problem, "no video track"):
		return "share-dropped"
	case strings.Contains(problem, "stopped delivering frames"),
		strings.Contains(problem, "encoder produced no frames"):
		return "media-stalled"
	}
	return "unknown"
}
