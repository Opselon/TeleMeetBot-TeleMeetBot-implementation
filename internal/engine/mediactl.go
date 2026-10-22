package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// mediaStatus mirrors the JSON returned by mediaControlJS.
type mediaStatus struct {
	OK           bool    `json:"ok"`
	Error        string  `json:"error"`
	Count        int     `json:"count"`
	Paused       bool    `json:"paused"`
	Muted        bool    `json:"muted"`
	Volume       float64 `json:"volume"`
	PlaybackRate float64 `json:"playbackRate"`
	CurrentTime  float64 `json:"currentTime"`
	Duration     float64 `json:"duration"`
	Title        string  `json:"title"`
	Src          string  `json:"src"`
}

// mediaControlJS builds a self-contained script that finds the best media
// element across same-origin frames, applies an action and returns status JSON.
// Actions: status, play, pause, mute, unmute, volume (0..100), rate (0.25..4),
// seek (delta seconds), restart.
func mediaControlJS(action string, arg float64) string {
	act, _ := json.Marshal(action)
	argJS := strconv.FormatFloat(arg, 'f', -1, 64)
	return fmt.Sprintf(`() => {
  const out = {ok: false, error: "", count: 0, paused: true, muted: false, volume: 1,
               playbackRate: 1, currentTime: 0, duration: 0, title: "", src: ""};
  const items = [];
  const walk = (doc, depth) => {
    if (!doc || depth > 6) return;
    let vids = [];
    try { vids = Array.from(doc.querySelectorAll('video,audio')); } catch (e) { return; }
    for (const v of vids) {
      let r = {width: 0, height: 0};
      try { r = v.getBoundingClientRect(); } catch (e) {}
      const area = Math.max(0, r.width | 0) * Math.max(0, r.height | 0);
      const dur = isFinite(v.duration) ? v.duration : 0;
      let score = area + (dur > 0 ? 500000 : 0) + (v.paused ? 0 : 250000);
      if (v.ended) score -= 1000;
      items.push({v, score});
    }
    let frames = [];
    try { frames = Array.from(doc.querySelectorAll('iframe')); } catch (e) {}
    for (const f of frames) {
      try { walk(f.contentDocument, depth + 1); } catch (e) {}
    }
  };
  walk(document, 0);
  out.count = items.length;
  if (!items.length) { out.error = "no media element"; return JSON.stringify(out); }
  items.sort((a, b) => b.score - a.score);
  const m = items[0].v;
  const arg = %s;
  const action = %s;
  try {
    switch (action) {
      case "play": {
        const mp = document.querySelector('#movie_player');
        if (mp && mp.playVideo) { try { mp.unMute(); mp.playVideo(); } catch (e) {} }
        const unmute = document.querySelector('.ytp-unmute-inner, button[aria-label*="unmute"]');
        if (unmute) { try { unmute.click(); } catch (e) {} }
        const p = m.play(); if (p && p.catch) p.catch(() => {});
        break;
      }
      case "pause": {
        const mp = document.querySelector('#movie_player');
        if (mp && mp.pauseVideo) { try { mp.pauseVideo(); } catch (e) {} }
        window.__allowPause = true;
        m.pause();
        break;
      }
      case "mute": {
        const mp = document.querySelector('#movie_player');
        if (mp && mp.mute) { try { mp.mute(); } catch (e) {} }
        m.muted = true;
        break;
      }
      case "unmute": {
        const mp = document.querySelector('#movie_player');
        if (mp && mp.unMute) { try { mp.unMute(); } catch (e) {} }
        const unmute = document.querySelector('.ytp-unmute-inner, button[aria-label*="unmute"]');
        if (unmute) { try { unmute.click(); } catch (e) {} }
        m.muted = false;
        break;
      }
      case "volume": {
        const mp = document.querySelector('#movie_player');
        if (mp && mp.setVolume) { try { mp.setVolume(Math.round(arg)); } catch (e) {} }
        m.volume = Math.min(1, Math.max(0, arg / 100));
        break;
      }
      case "rate": {
        const mp = document.querySelector('#movie_player');
        if (mp && mp.setPlaybackRate) { try { mp.setPlaybackRate(Math.min(4, Math.max(0.25, arg))); } catch (e) {} }
        m.playbackRate = Math.min(4, Math.max(0.25, arg));
        break;
      }
      case "seek": {
        const d = isFinite(m.duration) ? m.duration : 0;
        m.currentTime = Math.min(Math.max(0, m.currentTime + arg), d > 0 ? d : 1e9);
        break;
      }
      case "restart": {
        m.currentTime = 0;
        const p = m.play(); if (p && p.catch) p.catch(() => {});
        break;
      }
      case "status": break;
    }
  } catch (e) { out.error = String(e); }
  if (m.error) {
    out.error = "media error " + (m.error.code || "?") + " " + (m.error.message || "");
  }
  out.ok = !out.error;
  out.paused = !!m.paused;
  out.muted = !!m.muted;
  out.volume = typeof m.volume === "number" ? m.volume : 0;
  out.playbackRate = m.playbackRate || 1;
  out.currentTime = m.currentTime || 0;
  out.duration = isFinite(m.duration) ? m.duration : 0;
  out.src = String(m.currentSrc || m.src || "").slice(0, 300);
  let real = "";
  try {
    const og = document.querySelector('meta[property="og:title"], meta[name="og:title"]');
    if (og && og.content) real = og.content;
  } catch (e) {}
  if (!real) {
    try {
      const mp = document.querySelector('#movie_player');
      if (mp && mp.getVideoData) {
        const vd = mp.getVideoData();
        if (vd && vd.title) real = vd.title;
      }
    } catch (e) {}
  }
  if (!real) real = window.__tmOrigTitle || document.title || "";
  out.title = String(real).slice(0, 200);
  return JSON.stringify(out);
}`, argJS, act)
}

// titleLockJS keeps document.title pinned so Chromium's
// auto-select-desktop-capture-source always finds the media tab by name.
func titleLockJS(title string) string {
	t, _ := json.Marshal(title)
	return fmt.Sprintf(`() => {
  const T = %s;
  try {
    if (!window.__tmOrigTitle) window.__tmOrigTitle = document.title;
  } catch (e) {}
  const lock = () => {
    try {
      if (document.title !== T) document.title = T;
    } catch (e) {}
  };
  lock();
  try { if (window.__tmTitleLock) clearInterval(window.__tmTitleLock); } catch (e) {}
  window.__tmTitleLock = setInterval(lock, 800);
  return document.title;
}`, t)
}

// tabAliveJS is the watchdog liveness probe for the meet tab. rod requires
// function-form JS: a bare expression ("1") makes rod evaluate (1).apply and
// report a healthy browser as disconnected.
func tabAliveJS() string {
	return `() => { return 1; }`
}

// playerProblemJS scans visible text for known fatal player errors.
func playerProblemJS() string {
	return `() => {
  const txt = (((document.body || {}).innerText) || "").toLowerCase();
  const pats = [
    ["this video is unavailable", "video unavailable in this region"],
    ["video unavailable", "video unavailable"],
    ["sign in to confirm", "YouTube wants a sign-in (bot check)"],
    ["confirm you're not a bot", "YouTube bot check"],
    ["this video isn't available anymore", "video removed"],
    ["private video", "private video"],
    ["age-restricted", "age restricted"],
    ["playback error", "playback error"],
    ["an error occurred", "player error"],
    ["media could not be loaded", "media could not be loaded"],
    ["no supported source", "unsupported media format"]
  ];
  for (const [needle, msg] of pats) if (txt.includes(needle)) return msg;
  return "";
}`
}
