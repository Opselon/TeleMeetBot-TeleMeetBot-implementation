package engine

// publishMeasureJS is the single instrument for the publication half of a share:
// getDisplayMedia -> sender -> encoder -> RTP -> decoder -> rendered pixels. It
// reports the last stage reached, so a hang/timeout names the broken step, and
// every counter comes from real RTCStats. seconds sets the encoder measurement
// window during generation (remote pixel sampling derives from it).
func publishMeasureJS() string {
	return `() => new Promise(async (resolve) => {
  const WIN_MS = 6000;
  const SAMPLES = Math.max(6, Math.round(WIN_MS / 400));
  const out = {stage: "start", label: "", trackState: "", width: 0, height: 0, frameRate: 0, captureFrames: 0, captureFps: 0,
    encFrames: 0, encFps: 0, bytesSent: 0, bytesPerSec: 0, keyFrames: 0, encoderImpl: "", qualityLimitation: "",
    mediaSrcFrames: 0, mediaSrcFps: 0, mediaSrcWidth: 0, mediaSrcHeight: 0,
    recvFrames: 0, recvDecoded: 0, transceiverDirection: "", sendCodec: "",
    remoteMean: 0, remoteDelta: 0, remoteBlackRatio: 0, remoteFrames: 0, error: "", console: "",
    iceGathering: "", iceState: "", connState: "", candidates: 0, offer: ""};
  // Watchdog: whatever happens, resolve with the state we have so a hang is
  // diagnosable instead of a silent timeout.
  const done = new Promise(r => setTimeout(() => r("watchdog"), WIN_MS*4 + 15000));
  const race = (p) => Promise.race([p, done]);
  const fail = (m) => { if (!out.error) out.error = m; resolve(JSON.stringify(out)); };
  const finish = () => resolve(JSON.stringify(out));
  window.__report = out;
  try {
    out.stage = "getDisplayMedia";
    const stream = await race(navigator.mediaDevices.getDisplayMedia({video: true, audio: false}));
    if (!stream) return fail("getDisplayMedia returned no stream");
    const track = stream.getVideoTracks()[0];
    if (!track) return fail("no video track");
    out.label = track.label || "";
    out.trackState = track.readyState;
    const s0 = track.getSettings() || {};
    out.width = s0.width || 0; out.height = s0.height || 0; out.frameRate = s0.frameRate || 0;

    out.stage = "track-render";
    const el = document.createElement("video");
    el.muted = true; el.playsInline = true; el.srcObject = new MediaStream([track]);
    try { await race(el.play()); } catch (e) { out.console += "local play: " + String(e) + " | "; }
    await new Promise(r => setTimeout(r, 800));
    let cf = 0;
    if (el.requestVideoFrameCallback) {
      await race(new Promise(r => {
        const t0 = performance.now();
        const tick = () => {
          cf++;
          if (performance.now() - t0 < 2000) el.requestVideoFrameCallback(tick); else r();
        };
        el.requestVideoFrameCallback(tick);
      }));
    }
    out.captureFrames = cf;
    out.captureFps = Math.round(cf / 2 * 10) / 10;

    out.stage = "pc-setup";
    const pc1 = new RTCPeerConnection();
    const pc2 = new RTCPeerConnection();
    pc1.onicecandidate = e => { if (e.candidate) { out.candidates++; pc2.addIceCandidate(e.candidate).catch(() => {}); } };
    pc2.onicecandidate = e => { if (e.candidate) { out.candidates++; pc1.addIceCandidate(e.candidate).catch(() => {}); } };
    pc1.onicegatheringstatechange = () => { out.iceGathering = pc1.iceGatheringState; };
    pc1.oniceconnectionstatechange = () => { out.iceState = pc1.iceConnectionState; };
    pc1.onconnectionstatechange = () => { out.connState = pc1.connectionState; };
    pc1.addTrack(track, stream);
    const remote = new MediaStream();
    let remoteTrack = null;
    pc2.ontrack = e => { remoteTrack = e.track; remote.addTrack(e.track); };
    const offer = await pc1.createOffer();
    out.offer = offer.sdp.split("\n").filter(l => /m=video|a=send|a=recv|m=audio/.test(l)).join(" ;; ").slice(0, 300);
    await pc1.setLocalDescription(offer);
    await pc2.setRemoteDescription(offer);
    const answer = await pc2.createAnswer();
    await pc2.setLocalDescription(answer);
    await pc1.setRemoteDescription(answer);
    const tr = pc1.getTransceivers()[0];
    out.transceiverDirection = (tr && tr.currentDirection) || "";
    try {
      const codec = RTCRtpSender.getCapabilities("video").codecs[0];
      out.sendCodec = codec ? codec.mimeType : "";
    } catch (e) {}
    out.stage = "connect";
    const tconn = performance.now();
    while (pc1.connectionState !== "connected" && performance.now() - tconn < 8000) {
      await new Promise(r => setTimeout(r, 100));
    }
    if (pc1.connectionState !== "connected") return fail("sender connectionState=" + pc1.connectionState + " ice=" + pc1.iceConnectionState + " gathering=" + pc1.iceGatheringState + " candidates=" + out.candidates);
    if (!remoteTrack) return fail("receiver never fired ontrack (no video m-line negotiated)");

    out.stage = "remote-render";
    const rv = document.createElement("video");
    rv.muted = true; rv.playsInline = true; rv.srcObject = remote;
    try { await race(rv.play()); } catch (e) { out.console += "remote play: " + String(e) + " | "; }
    await new Promise(r => setTimeout(r, 1000));

    const snap = async () => {
      const res = {};
      for (const pc of [pc1, pc2]) {
        const stats = await pc.getStats();
        stats.forEach(r => {
          if (r.type === "outbound-rtp" && r.kind === "video") {
            res.encFrames = Math.max(res.encFrames || 0, r.framesEncoded || 0);
            res.bytesSent = Math.max(res.bytesSent || 0, r.bytesSent || 0);
            res.keyFrames = Math.max(res.keyFrames || 0, r.keyFramesEncoded || 0);
            if (r.encoderImplementation) res.encoderImpl = r.encoderImplementation;
            if (r.qualityLimitationReason) res.qualityLimitation = r.qualityLimitationReason;
            if (r.frameWidth) res.encWidth = r.frameWidth;
            if (r.frameHeight) res.encHeight = r.frameHeight;
          }
          if (r.type === "media-source" && r.kind === "video") {
            res.mediaSrcFrames = Math.max(res.mediaSrcFrames || 0, r.frames || 0);
            res.mediaSrcFps = Math.max(res.mediaSrcFps || 0, r.framesPerSecond || 0);
            res.mediaSrcWidth = Math.max(res.mediaSrcWidth || 0, r.width || 0);
            res.mediaSrcHeight = Math.max(res.mediaSrcHeight || 0, r.height || 0);
          }
          if (r.type === "inbound-rtp" && r.kind === "video") {
            res.recvFrames = Math.max(res.recvFrames || 0, r.framesReceived || 0);
            res.recvDecoded = Math.max(res.recvDecoded || 0, r.framesDecoded || 0);
          }
        });
      }
      return res;
    };
    out.stage = "measure";
    const a = await snap();
    const ta = performance.now();
    await new Promise(r => setTimeout(r, WIN_MS));
    const b = await snap();
    const dt = (performance.now() - ta) / 1000;
    out.encFrames = Math.round((b.encFrames || 0) - (a.encFrames || 0));
    out.encFps = Math.round(out.encFrames / dt * 10) / 10;
    out.bytesSent = Math.round((b.bytesSent || 0) - (a.bytesSent || 0));
    out.bytesPerSec = Math.round(out.bytesSent / dt);
    out.keyFrames = b.keyFrames || 0;
    out.encoderImpl = b.encoderImpl || "";
    out.qualityLimitation = b.qualityLimitation || "";
    out.mediaSrcFrames = Math.round((b.mediaSrcFrames || 0) - (a.mediaSrcFrames || 0));
    out.mediaSrcFps = Math.round((b.mediaSrcFps || 0) * 10) / 10;
    out.mediaSrcWidth = b.mediaSrcWidth || 0;
    out.mediaSrcHeight = b.mediaSrcHeight || 0;

    out.stage = "sample-remote";
    const c = document.createElement("canvas");
    c.width = 96; c.height = 54;
    const ctx = c.getContext("2d");
    let prev = null, meanSum = 0, deltaSum = 0, n = 0, black = 0;
    for (let i = 0; i < SAMPLES; i++) {
      ctx.drawImage(rv, 0, 0, c.width, c.height);
      const d = ctx.getImageData(0, 0, c.width, c.height).data;
      let sum = 0, dark = 0;
      const lum = new Array(d.length / 4);
      for (let p = 0; p < d.length; p += 4) {
        const l = (d[p] + d[p + 1] + d[p + 2]) / 3;
        lum[p / 4] = l; sum += l;
        if (l < 8) dark++;
      }
      const m = sum / lum.length;
      meanSum += m;
      if (m < 8) black++;
      if (prev) {
        let acc = 0;
        for (let k = 0; k < lum.length; k++) acc += Math.abs(lum[k] - prev[k]);
        deltaSum += acc / lum.length; n++;
      }
      prev = lum;
      await new Promise(r => setTimeout(r, 120));
    }
    out.remoteFrames = SAMPLES;
    out.remoteMean = Math.round(meanSum / SAMPLES * 10) / 10;
    out.remoteDelta = Math.round((n ? deltaSum / n : 0) * 100) / 100;
    out.remoteBlackRatio = Math.round(black / SAMPLES * 100) / 100;
    const c2 = await snap();
    out.recvFrames = c2.recvFrames || 0;
    out.recvDecoded = c2.recvDecoded || 0;
    stream.getTracks().forEach(t => t.stop());
    out.stage = "done";
    finish();
  } catch (e) {
    fail(String(e));
  }
})
`
}
