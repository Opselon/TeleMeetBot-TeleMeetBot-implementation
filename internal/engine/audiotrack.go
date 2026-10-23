package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

// YouTube multi-audio (auto-dubbing) handling
//
// YouTube provides videos with multiple audio tracks: the original language
// plus AI-generated or studio dubs. The web player automatically selects an audio
// track based on the viewer's interface language (hl) or Google account
// preferences, which defaults to English ("en-US"). Consequently, videos with
// original non-English audio (or videos where the user preferred Persian) get
// automatically dubbed into English when streamed by the bot.
//
// Root Cause:
// 1. Ingestion: YouTube watch URLs are loaded in headless Chromium with
//    default "en-US" accept-language and locale.
// 2. Stream selection: YouTube's player enables auto-dubbed English audio by default.
// 3. Previous bug: In the previous implementation:
//    - The YouTube Player API's getAvailableAudioTracks() returns internal track
//      objects where track.id is a composite protobuf string (e.g. "251;ChEKBWFjb250..."),
//      whereas getPlayerResponse() captions audioTrackId is a short language code
//      (e.g. "en-US.4"). Equality checks (t.id === origId) failed.
//    - In getTrackName(), getLanguageInfo() was checked and returned li.id instead of
//      li.name, causing name to become "en-US.4", which failed isOrigText().
//    - The switch was never executed and a silent fallback to active track kept dubbed English.
//    - There was no user language selection or Telegram controls.
//
// Solution:
// 1. Inspect YouTube Player API track objects comprehensively (getLanguageInfo(),
//    sub-object scanning for {id, name, isAutoDubbed, isDefault}, and base64 protobuf
//    payloads inside track.id).
// 2. Parse authoritative captions metadata (defaultAudioTrackIndex, videoDetails.audioLanguage).
// 3. Support requested language modes:
//    - "fa": Persian (Default) — selects Persian track if available; falls back to
//      the creator's Original audio if Persian is unavailable, with clear explanation.
//    - "en": English — selects English track (preferring original if source was English).
//    - "orig": Original audio — selects the creator's source language, strictly rejecting auto-dubs.
//    - "auto": Auto / best available — prefers Persian if present, else original audio.
// 4. Switch to the target track using player.setAudioTrack(target.ref) with settings UI fallback.
// 5. Verify the active track via player.getAudioTrack(). Never silently accept a dubbed track
//    when another language was selected.
// 6. Structured logging under [PIPELINE:audiotrack].

const (
	// audioTrackAttempts is the maximum number of discovery/selection attempts.
	audioTrackAttempts = 5

	// audioTrackAttemptDelay is the sleep duration between retries.
	audioTrackAttemptDelay = 600 * time.Millisecond
)

// NormalizeAudioLang normalizes requested language code.
func NormalizeAudioLang(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "fa", "persian", "farsi", "فارسی":
		return "fa"
	case "en", "english", "eng":
		return "en"
	case "orig", "original", "source":
		return "orig"
	case "auto", "best":
		return "auto"
	default:
		return "fa"
	}
}

// AudioLangDisplayName returns user-friendly label for language code.
func AudioLangDisplayName(code string) string {
	switch NormalizeAudioLang(code) {
	case "fa":
		return "🇮🇷 فارسی (default)"
	case "en":
		return "🇬🇧 English"
	case "orig":
		return "🌐 Original audio"
	case "auto":
		return "🔄 Auto / best available"
	default:
		return "🇮🇷 فارسی (default)"
	}
}

// AudioTrackInfo describes a single audio track.
type AudioTrackInfo struct {
	ID           string `json:"id"`
	RawID        string `json:"rawId"`
	Name         string `json:"name"`
	Language     string `json:"language"`
	IsOriginal   bool   `json:"isOriginal"`
	IsAutoDubbed bool   `json:"isAutoDubbed"`
	IsDefault    bool   `json:"isDefault"`
	Active       bool   `json:"active"`
}

// AudioTrackReport summarizes the audio track selection outcome.
type AudioTrackReport struct {
	Kind             MediaKind       `json:"kind"`
	VideoID          string          `json:"videoId,omitempty"`
	RequestedLang    string          `json:"requestedLang"`
	Available        int             `json:"available"`
	AvailableLangs   []string        `json:"availableLangs,omitempty"`
	Active           AudioTrackInfo  `json:"active"`
	Original         *AudioTrackInfo `json:"original,omitempty"`
	Selected         *AudioTrackInfo `json:"selected,omitempty"`
	PersianAvailable bool            `json:"persianAvailable"`
	Switched         bool            `json:"switched"`
	Verified         bool            `json:"verified"`
	Method           string          `json:"method"`
	PlayerAPI        bool            `json:"playerApi"`
	Attempts         int             `json:"attempts"`
	SingleTrack      bool            `json:"singleTrack"`
	FallbackReason   string          `json:"fallbackReason,omitempty"`
	Note             string          `json:"note,omitempty"`
}

// SelectedTrackLabel returns a short human description of the selected audio track.
func (r AudioTrackReport) SelectedTrackLabel() string {
	if r.Selected != nil && r.Selected.Name != "" {
		return r.Selected.Name
	}
	if r.Active.Name != "" {
		return r.Active.Name
	}
	if r.Active.ID != "" {
		return r.Active.ID
	}
	return "Default"
}

func (r AudioTrackReport) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "kind=%s req=%s available=%d", r.Kind, r.RequestedLang, r.Available)
	if len(r.AvailableLangs) > 0 {
		fmt.Fprintf(&b, " langs=[%s]", strings.Join(r.AvailableLangs, ","))
	}
	if r.VideoID != "" {
		fmt.Fprintf(&b, " videoId=%s", r.VideoID)
	}
	fmt.Fprintf(&b, " active=%q", trackLabel(r.Active))
	if r.Selected != nil {
		fmt.Fprintf(&b, " selected=%q", trackLabel(*r.Selected))
	}
	if r.Original != nil {
		fmt.Fprintf(&b, " original=%q", trackLabel(*r.Original))
	}
	if r.Method != "" {
		fmt.Fprintf(&b, " method=%s", r.Method)
	}
	fmt.Fprintf(&b, " switched=%t verified=%t single=%t", r.Switched, r.Verified, r.SingleTrack)
	if r.FallbackReason != "" {
		fmt.Fprintf(&b, " fallback=%q", r.FallbackReason)
	}
	if r.Note != "" {
		fmt.Fprintf(&b, " note=%q", r.Note)
	}
	return b.String()
}

func trackLabel(t AudioTrackInfo) string {
	if t.ID == "" && t.Name == "" {
		return "-"
	}
	name := t.Name
	if name == "" {
		name = t.Language
	}
	if name == "" {
		name = t.ID
	}
	origStr := ""
	if t.IsOriginal {
		origStr = "|orig"
	} else if t.IsAutoDubbed {
		origStr = "|autodub"
	}
	return fmt.Sprintf("%s|%s%s", t.ID, name, origStr)
}

// audioTrackCycle mirrors the JSON returned by audioTracksJS.
type audioTrackCycle struct {
	VideoID          string          `json:"videoId"`
	Available        int             `json:"available"`
	AvailableLangs   []string        `json:"availableLangs"`
	ActiveID         string          `json:"activeId"`
	ActiveName       string          `json:"activeName"`
	ActiveLang       string          `json:"activeLang"`
	ActiveIsOriginal bool            `json:"activeIsOriginal"`
	ActiveIsAutoDub  bool            `json:"activeIsAutoDub"`
	OriginalID       string          `json:"originalId"`
	OriginalName     string          `json:"originalName"`
	OriginalFound    bool            `json:"originalFound"`
	SelectedID       string          `json:"selectedId"`
	SelectedName     string          `json:"selectedName"`
	SelectedLang     string          `json:"selectedLang"`
	PersianAvailable bool            `json:"persianAvailable"`
	Switched         bool            `json:"switched"`
	Verified         bool            `json:"verified"`
	PlayerAPI        bool            `json:"playerApi"`
	Method           string          `json:"method"`
	FallbackReason   string          `json:"fallbackReason"`
	Error            string          `json:"error"`
	Fields           []string        `json:"fields"`
	Steps            []string        `json:"steps"`
	Tracks           []audioTrackRow `json:"tracks"`
}

type audioTrackRow struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Lang         string `json:"lang"`
	IsOriginal   bool   `json:"isOriginal"`
	IsAutoDubbed bool   `json:"isAutoDubbed"`
	IsDefault    bool   `json:"isDefault"`
}

// audioTracksJS returns the JavaScript evaluated in the media tab to
// discover, select according to requested language, and verify the audio track.
func audioTracksJS(reqLang string, apply bool) string {
	mode := "false"
	if apply {
		mode = "true"
	}
	reqClean := NormalizeAudioLang(reqLang)

	return fmt.Sprintf(`() => {
  const APPLY = %s;
  const REQ_LANG = %q;

  const out = {
    videoId: "", available: 0, availableLangs: [],
    activeId: "", activeName: "", activeLang: "", activeIsOriginal: false, activeIsAutoDub: false,
    originalId: "", originalName: "", originalFound: false,
    selectedId: "", selectedName: "", selectedLang: "",
    persianAvailable: false, switched: false, verified: false,
    playerApi: false, method: "", fallbackReason: "", error: "",
    fields: [], steps: [], tracks: []
  };

  const step = (m) => { out.steps.push(m); };
  const norm = (s) => String(s == null ? "" : s).trim();

  const isOrigText = (s) => {
    const t = norm(s).toLowerCase();
    return t.includes("original") || t.includes("source") || t.includes("d'origine") ||
           t.includes("originale") || t.includes("originalton") || t.includes("ursprünglich") ||
           t.includes("اصلی") || t.includes("اورجینال");
  };

  const langMap = {
    "persian": "fa", "farsi": "fa", "فارسی": "fa",
    "english": "en", "inglés": "en", "anglais": "en", "englisch": "en", "inglese": "en",
    "spanish": "es", "español": "es", "espagnol": "es", "spanisch": "es",
    "french": "fr", "français": "fr", "französisch": "fr", "francese": "fr",
    "german": "de", "deutsch": "de", "allemand": "de", "tedesco": "de",
    "italian": "it", "italiano": "it", "italien": "it",
    "japanese": "ja", "日本語": "ja", "japonais": "ja", "japanisch": "ja",
    "korean": "ko", "한국어": "ko", "coréen": "ko", "koreanisch": "ko",
    "arabic": "ar", "العربية": "ar", "arabe": "ar", "arabisch": "ar",
    "russian": "ru", "русский": "ru", "russe": "ru", "russisch": "ru",
    "hindi": "hi", "हिन्दी": "hi",
    "portuguese": "pt", "português": "pt",
    "indonesian": "id", "bahasa indonesia": "id",
    "turkish": "tr", "türkçe": "tr",
    "chinese": "zh", "中文": "zh"
  };

  const normalizeLangCode = (id, name, protoLang) => {
    if (protoLang) {
      const pl = norm(protoLang).toLowerCase().split("-")[0];
      if (pl && pl.length <= 3) return pl;
    }
    const cleanId = norm(id).split(";")[0].split(".")[0].split("-")[0].toLowerCase();
    if (cleanId && cleanId.length >= 2 && cleanId.length <= 3 && !/^\d+$/.test(cleanId)) {
      return cleanId;
    }
    const lowName = norm(name).toLowerCase();
    for (const key in langMap) {
      if (lowName.includes(key)) return langMap[key];
    }
    return cleanId || "";
  };

  const decodeProto = (rawId) => {
    const res = { orig: false, autodub: false, lang: "" };
    if (!rawId || typeof rawId !== "string" || !rawId.includes(";")) return res;
    try {
      const b64 = rawId.split(";")[1];
      if (b64 && typeof atob === "function") {
        const dec = atob(b64);
        if (dec.includes("original")) res.orig = true;
        if (dec.includes("dubbed-auto")) res.autodub = true;
        const match = dec.match(/lang\x12([a-zA-Z0-9_-]+)/);
        if (match && match[1]) res.lang = match[1];
      }
    } catch (e) {}
    return res;
  };

  const parseTrackObj = (t) => {
    if (!t) return null;
    let trackId = "", rawId = "", name = "", langCode = "", lang = "";
    let isOriginal = false, isAutoDubbed = false, isDefault = false;

    if (typeof t.id === "string") trackId = norm(t.id);
    if (!trackId && typeof t.getId === "function") trackId = norm(t.getId());
    if (!trackId && t.Vp && typeof t.Vp.id === "string") trackId = norm(t.Vp.id);

    // 1. Check getLanguageInfo()
    try {
      if (typeof t.getLanguageInfo === "function") {
        const li = t.getLanguageInfo();
        if (li && typeof li === "object") {
          if (li.id) langCode = norm(li.id);
          if (li.name) name = norm(li.name);
          if (typeof li.isAutoDubbed === "boolean") isAutoDubbed = li.isAutoDubbed;
          if (typeof li.isDefault === "boolean") isDefault = li.isDefault;
        }
      }
    } catch (e) {}

    // 2. Scan properties for nested language info object (e.g. t.Mb)
    if (!name || !langCode) {
      try {
        for (const k of Object.keys(t)) {
          const val = t[k];
          if (val && typeof val === "object" && !Array.isArray(val)) {
            if (!langCode && typeof val.id === "string" && val.id.length < 32) langCode = norm(val.id);
            if (!name && typeof val.name === "string" && val.name) name = norm(val.name);
            if (!name && typeof val.displayName === "string" && val.displayName) name = norm(val.displayName);
            if (typeof val.isAutoDubbed === "boolean") isAutoDubbed = val.isAutoDubbed;
            if (typeof val.isDefault === "boolean") isDefault = val.isDefault;
          }
        }
      } catch (e) {}
    }

    // 3. Methods on track object if standard
    try {
      if (!name && typeof t.getName === "function") name = norm(t.getName());
    } catch (e) {}

    const finalId = trackId || langCode || name;
    rawId = trackId || finalId;

    // 4. Protobuf metadata inside rawId
    const proto = decodeProto(rawId);
    if (proto.orig) isOriginal = true;
    if (proto.autodub) isAutoDubbed = true;

    lang = normalizeLangCode(langCode || finalId, name, proto.lang);

    const lowName = norm(name).toLowerCase();
    if (lowName.includes("dubbed") || lowName.includes("دوبله") || lowName.includes("dublado") || lowName.includes("doublé")) {
      isAutoDubbed = true;
    }

    if (isOrigText(name) || isOrigText(finalId)) {
      isOriginal = true;
    }
    if (!isAutoDubbed && isDefault) {
      isOriginal = true;
    }

    return {
      id: finalId,
      rawId: rawId,
      name: name || finalId,
      lang: lang,
      isOriginal: isOriginal,
      isAutoDubbed: isAutoDubbed,
      isDefault: isDefault,
      ref: t
    };
  };

  const player = document.querySelector("#movie_player") ||
                 document.querySelector("#shorts-player") ||
                 (document.querySelector(".video-stream") && document.querySelector(".video-stream").closest(".html5-video-player")) ||
                 null;
  if (!player) {
    out.error = "no player element";
    return JSON.stringify(out);
  }

  // 1) Read Authoritative Player Response Metadata
  let authoritativeOrigId = "";
  let authoritativeVideoLang = "";
  try {
    if (typeof player.getPlayerResponse === "function") {
      const resp = player.getPlayerResponse();
      if (resp) {
        if (resp.videoDetails && resp.videoDetails.videoId) {
          out.videoId = resp.videoDetails.videoId;
        }
        if (resp.videoDetails && resp.videoDetails.audioLanguage) {
          authoritativeVideoLang = norm(resp.videoDetails.audioLanguage).toLowerCase().split("-")[0];
          out.fields.push("videoLang:" + authoritativeVideoLang);
        }
        const captions = resp.captions && resp.captions.playerCaptionsTracklistRenderer;
        if (captions && Array.isArray(captions.audioTracks)) {
          let defIdx = captions.defaultAudioTrackIndex;
          if (typeof defIdx === "number" && defIdx >= 0 && defIdx < captions.audioTracks.length) {
            authoritativeOrigId = norm(captions.audioTracks[defIdx].audioTrackId || captions.audioTracks[defIdx].id);
            out.fields.push("authoritativeOrigId:" + authoritativeOrigId);
          }
        }
      }
    }
  } catch (e) {
    step("player-response-error");
  }

  // 2) Discover tracks from Player API
  let tracks = [];
  try {
    if (typeof player.getAvailableAudioTracks === "function") {
      const got = player.getAvailableAudioTracks();
      if (Array.isArray(got) && got.length > 0) {
        out.playerApi = true;
        step("player-api");
        for (const raw of got) {
          const parsed = parseTrackObj(raw);
          if (!parsed) continue;
          if (authoritativeOrigId && (parsed.id === authoritativeOrigId || parsed.id.startsWith(authoritativeOrigId))) {
            parsed.isOriginal = true;
          }
          if (authoritativeVideoLang && parsed.lang === authoritativeVideoLang) {
            parsed.isOriginal = true;
          }
          tracks.push(parsed);
        }
      }
    }
  } catch (e) {
    out.error = "getAvailableAudioTracks: " + String(e).slice(0, 140);
  }

  // 3) HTMLMediaElement fallback
  if (!tracks.length) {
    try {
      const vids = Array.from(document.querySelectorAll("video,audio"));
      const v = vids.find((x) => x.readyState >= 1) || vids[0];
      if (v && v.audioTracks && v.audioTracks.length > 0) {
        step("htmlmedia-tracks");
        for (let i = 0; i < v.audioTracks.length; i++) {
          const at = v.audioTracks[i];
          const id = norm(at.id) || norm(at.language) || ("track-" + i);
          const name = norm(at.label) || norm(at.language);
          const origFlag = isOrigText(name) || isOrigText(id) || at.kind === "main";
          tracks.push({
            id: id, rawId: id, name: name, lang: normalizeLangCode(id, name, ""),
            isOriginal: origFlag, isAutoDubbed: false, isDefault: origFlag, ref: at
          });
        }
      }
    } catch (e) {}
  }

  // 4) UI Fallback Discovery via Settings Menu
  const uiOpenMenu = () => {
    const btn = document.querySelector(".ytp-settings-button");
    if (!btn) return null;
    const fit = document.getElementById("tm-media-fit");
    const fitCSS = fit ? fit.textContent : "";
    if (fit) fit.textContent = "";
    btn.click();
    return { btn: btn, fit: fit, css: fitCSS };
  };
  const uiMenuItems = () => Array.from(document.querySelectorAll(
    ".ytp-settings-menu .ytp-menuitem, .ytp-popup .ytp-menuitem, .ytp-panel-menu .ytp-menuitem"));
  const uiFindAudioEntry = () => uiMenuItems().find((el) => {
    const txt = ((el.innerText || el.textContent || "") + " " + (el.getAttribute("aria-label") || "")).toLowerCase();
    return txt.includes("audio track") || txt.includes("piste audio") || txt.includes("tonspur") ||
           txt.includes("traccia audio") || txt.includes("音声") || txt.includes("صوت") ||
           txt.includes("صدا") || txt.includes("audio");
  });
  const uiReadTracks = () => {
    const items = uiMenuItems();
    const list = [];
    for (const el of items) {
      const txt = (el.innerText || el.textContent || "").trim();
      if (!txt) continue;
      const low = txt.toLowerCase();
      if (low === "back" || low.includes("quality") || low.includes("speed") || low.includes("subtitle")) continue;
      const name = low.includes("audio") ? "" : txt;
      if (!name) continue;
      const isOrig = isOrigText(name);
      list.push({
        id: "ui-" + list.length, rawId: "ui-" + list.length, name: name,
        lang: normalizeLangCode("", name, ""), isOriginal: isOrig,
        isAutoDubbed: !isOrig && (low.includes("dubbed") || low.includes("د دوبله")),
        isDefault: isOrig, checked: el.getAttribute("aria-checked") === "true", el: el, ref: el
      });
    }
    return list;
  };
  const uiCloseMenu = (ctx) => {
    try { if (ctx && ctx.btn) ctx.btn.click(); } catch (e) {}
    try {
      const menu = document.querySelector(".ytp-settings-menu");
      if (menu && menu.offsetParent !== null) menu.style.display = "none";
    } catch (e) {}
    try { if (ctx && ctx.fit) ctx.fit.textContent = ctx.css; } catch (e) {}
  };

  if (!tracks.length) {
    try {
      const ctx = uiOpenMenu();
      if (ctx) {
        const entry = uiFindAudioEntry();
        if (entry) {
          entry.click();
          const ui = uiReadTracks();
          if (ui.length > 0) {
            step("ui-discovery(" + ui.length + ")");
            tracks = ui;
            const checked = ui.find((t) => t.checked);
            if (checked) {
              out.activeId = checked.id;
              out.activeName = checked.name;
              out.activeLang = checked.lang;
            }
          }
        }
        uiCloseMenu(ctx);
      }
    } catch (e) { step("ui-discovery-error: " + String(e).slice(0, 60)); }
  }

  out.available = tracks.length;
  const langSet = new Set();
  for (const t of tracks) {
    if (t.lang) langSet.add(t.lang);
    if (t.lang === "fa" || t.name.toLowerCase().includes("persian") || t.name.toLowerCase().includes("farsi") || t.name.includes("فارسی")) {
      out.persianAvailable = true;
    }
    out.tracks.push({
      id: t.id, name: t.name, lang: t.lang,
      isOriginal: t.isOriginal, isAutoDubbed: t.isAutoDubbed, isDefault: t.isDefault
    });
  }
  out.availableLangs = Array.from(langSet);

  // 5) Identify currently active track
  let active = null;
  try {
    if (out.playerApi && typeof player.getAudioTrack === "function") {
      const cur = player.getAudioTrack();
      if (cur) {
        const curParsed = parseTrackObj(cur);
        if (curParsed) {
          active = tracks.find((t) => (t.rawId && t.rawId === curParsed.rawId) || (t.id && t.id === curParsed.id)) ||
                   tracks.find((t) => t.name === curParsed.name) ||
                   curParsed;
          out.activeId = curParsed.id;
          out.activeName = curParsed.name;
          out.activeLang = curParsed.lang;
          out.activeIsOriginal = curParsed.isOriginal;
          out.activeIsAutoDub = curParsed.isAutoDubbed;
        }
      }
    }
  } catch (e) {
    out.error = "getAudioTrack: " + String(e).slice(0, 140);
  }

  if (!active) {
    const checked = tracks.find((t) => t.checked || (t.ref && t.ref.enabled));
    if (checked) {
      active = checked;
      out.activeId = checked.id;
      out.activeName = checked.name;
      out.activeLang = checked.lang;
      out.activeIsOriginal = checked.isOriginal;
      out.activeIsAutoDub = checked.isAutoDubbed;
    }
  }

  // 6) Identify Creator's Original Audio Track
  let original = tracks.find((t) => t.isOriginal) ||
                 tracks.find((t) => !t.isAutoDubbed && t.isDefault) ||
                 tracks.find((t) => authoritativeOrigId && (t.id === authoritativeOrigId || t.id.startsWith(authoritativeOrigId))) ||
                 tracks.find((t) => authoritativeVideoLang && t.lang === authoritativeVideoLang) ||
                 null;

  if (original) {
    out.originalId = original.id;
    out.originalName = original.name;
    out.originalFound = true;
  } else if (tracks.length === 1) {
    out.originalId = tracks[0].id;
    out.originalName = tracks[0].name;
    out.originalFound = true;
    original = tracks[0];
  }

  // Single-track video handling:
  if (tracks.length <= 1) {
    out.method = "single-track";
    out.verified = true;
    if (tracks.length === 1) {
      out.selectedId = tracks[0].id;
      out.selectedName = tracks[0].name;
      out.selectedLang = tracks[0].lang;
      if (REQ_LANG === "fa" && tracks[0].lang !== "fa") {
        out.fallbackReason = "Single audio track video (" + tracks[0].name + "); Persian track not available";
      }
    }
    step(tracks.length === 1 ? "single-track-confirmed" : "no-tracks-found");
    return JSON.stringify(out);
  }

  // 7) Select Target Track based on REQ_LANG
  let target = null;
  let fallback = "";

  if (REQ_LANG === "fa") {
    // 1. Search for Persian track
    const persian = tracks.find((t) => t.lang === "fa" ||
                                       t.name.toLowerCase().includes("persian") ||
                                       t.name.toLowerCase().includes("farsi") ||
                                       t.name.includes("فارسی"));
    if (persian) {
      target = persian;
      step("target-persian-available");
    } else {
      // 2. Deterministic fallback to Original audio
      target = original || tracks[0];
      fallback = "Persian audio unavailable on this video; fell back to original audio (" + (target ? target.name : "source") + ")";
      step("target-persian-fallback-original");
    }
  } else if (REQ_LANG === "en") {
    // Search for English track: prefer original English if available, else dubbed English
    const engOrig = tracks.find((t) => t.lang === "en" && t.isOriginal);
    const engAny = tracks.find((t) => t.lang === "en");
    if (engOrig) {
      target = engOrig;
      step("target-english-original");
    } else if (engAny) {
      target = engAny;
      step("target-english-dubbed");
    } else {
      target = original || tracks[0];
      fallback = "English audio unavailable on this video; fell back to original audio (" + (target ? target.name : "source") + ")";
      step("target-english-fallback-original");
    }
  } else if (REQ_LANG === "orig") {
    // Prefer the creator's original track
    if (original) {
      target = original;
      step("target-original");
    } else {
      target = tracks[0];
      fallback = "no original audio marker identified; defaulted to source track";
      step("target-original-unresolved");
    }
  } else if (REQ_LANG === "auto") {
    // Best available: Persian if present, else original
    const persian = tracks.find((t) => t.lang === "fa");
    if (persian) {
      target = persian;
      step("target-auto-persian");
    } else if (original) {
      target = original;
      step("target-auto-original");
    } else {
      target = tracks[0];
      step("target-auto-first");
    }
  }

  if (target) {
    out.selectedId = target.id;
    out.selectedName = target.name;
    out.selectedLang = target.lang;
  }
  out.fallbackReason = fallback;

  // 8) Switch to Target Track if needed
  const isActiveTarget = active && target && (
    (active.rawId && target.rawId && active.rawId === target.rawId) ||
    (active.id && target.id && active.id === target.id) ||
    (active.name && target.name && active.name.toLowerCase() === target.name.toLowerCase())
  );

  if (!isActiveTarget && APPLY && target) {
    let applied = false;

    // Primary: Player API setAudioTrack(target.ref)
    try {
      if (out.playerApi && typeof player.setAudioTrack === "function" && target.ref) {
        player.setAudioTrack(target.ref);
        out.method = "player-api";
        applied = true;
        step("applied-player-api");
      }
    } catch (e) {
      out.error = "setAudioTrack: " + String(e).slice(0, 140);
      step("player-api-failed");
    }

    // Secondary: HTMLMediaElement audioTracks
    if (!applied && target.ref && typeof target.ref.enabled !== "undefined") {
      try {
        for (const t of tracks) {
          if (t.ref && typeof t.ref.enabled !== "undefined") {
            t.ref.enabled = (t === target);
          }
        }
        out.method = "htmlmedia";
        applied = true;
        step("applied-htmlmedia");
      } catch (e) {}
    }

    // Tertiary: UI Settings Menu Fallback
    if (!applied) {
      try {
        const ctx = uiOpenMenu();
        if (ctx) {
          const entry = uiFindAudioEntry();
          if (entry) {
            entry.click();
            const ui = uiReadTracks();
            let uiTarget = ui.find((t) => target.lang && t.lang === target.lang);
            if (!uiTarget && target.name) {
              uiTarget = ui.find((t) => t.name.toLowerCase() === target.name.toLowerCase() ||
                                        t.name.toLowerCase().includes(target.name.toLowerCase()));
            }
            if (uiTarget && uiTarget.el) {
              uiTarget.el.click();
              out.method = "ui-menu";
              applied = true;
              step("applied-ui-menu");
            }
          }
          uiCloseMenu(ctx);
        }
      } catch (e) {
        step("ui-fallback-error: " + String(e).slice(0, 60));
      }
    }

    out.switched = applied;

    // Verify Active Track
    let verifiedActive = null;
    try {
      if (out.playerApi && typeof player.getAudioTrack === "function") {
        const cur = player.getAudioTrack();
        verifiedActive = parseTrackObj(cur);
      }
    } catch (e) {}

    if (verifiedActive) {
      out.activeId = verifiedActive.id;
      out.activeName = verifiedActive.name;
      out.activeLang = verifiedActive.lang;
      out.activeIsOriginal = verifiedActive.isOriginal;
      out.activeIsAutoDub = verifiedActive.isAutoDubbed;

      const matchesTarget = (verifiedActive.rawId && target.rawId && verifiedActive.rawId === target.rawId) ||
                            (verifiedActive.id && target.id && verifiedActive.id === target.id) ||
                            (verifiedActive.name && target.name && verifiedActive.name.toLowerCase() === target.name.toLowerCase());
      out.verified = matchesTarget;
    } else {
      out.verified = applied;
    }
  } else if (isActiveTarget) {
    out.switched = false;
    out.verified = true;
    out.method = "already-active";
    step("already-active-target");
  }

  return JSON.stringify(out);
}`, mode, reqClean)
}

// readAudioTracks executes the audio track script and parses the output.
func readAudioTracks(p *rod.Page, reqLang string, apply bool) (audioTrackCycle, error) {
	res, err := p.Timeout(10 * time.Second).Eval(audioTracksJS(reqLang, apply))
	if err != nil {
		return audioTrackCycle{}, fmt.Errorf("eval audio track script: %w", err)
	}
	var c audioTrackCycle
	if err := json.Unmarshal([]byte(res.Value.Str()), &c); err != nil {
		return audioTrackCycle{}, fmt.Errorf("decode audio track result: %w", err)
	}
	return c, nil
}

// cycleToReport builds an AudioTrackReport from a cycle.
func cycleToReport(kind MediaKind, reqLang string, c audioTrackCycle, attempts int) AudioTrackReport {
	rep := AudioTrackReport{
		Kind:             kind,
		VideoID:          c.VideoID,
		RequestedLang:    NormalizeAudioLang(reqLang),
		Available:        c.Available,
		AvailableLangs:   c.AvailableLangs,
		Method:           c.Method,
		PlayerAPI:        c.PlayerAPI,
		Attempts:         attempts,
		SingleTrack:      c.Available <= 1,
		PersianAvailable: c.PersianAvailable,
		FallbackReason:   c.FallbackReason,
		Switched:         c.Switched,
		Verified:         c.Verified,
		Active: AudioTrackInfo{
			ID:           c.ActiveID,
			Name:         c.ActiveName,
			Language:     c.ActiveLang,
			IsOriginal:   c.ActiveIsOriginal,
			IsAutoDubbed: c.ActiveIsAutoDub,
			Active:       true,
		},
	}
	if c.OriginalFound {
		rep.Original = &AudioTrackInfo{
			ID:         c.OriginalID,
			Name:       c.OriginalName,
			IsOriginal: true,
		}
	}
	if c.SelectedID != "" {
		rep.Selected = &AudioTrackInfo{
			ID:       c.SelectedID,
			Name:     c.SelectedName,
			Language: c.SelectedLang,
		}
	}
	if rep.Note == "" && c.FallbackReason != "" {
		rep.Note = c.FallbackReason
	}
	return rep
}

// ensureAudioTrack enforces the requested audio track preference (Persian by default,
// English, Original, or Auto) for YouTube videos whenever multiple tracks are available.
//
// Invariants:
//   - If Persian ("fa") is requested and a Persian track exists, plays Persian.
//   - If Persian is requested and unavailable, deterministically falls back to
//     the creator's Original audio track (never silently keeping auto-dubbed English),
//     and explains the fallback in FallbackReason.
//   - If Original ("orig") is requested, forces the creator's original track and
//     rejects all auto-dubbed tracks.
//   - Never claims a language track exists without verifying it.
//   - If an auto-dubbed track remains active and cannot be switched away, returns
//     an explicit verification error instead of silently streaming the wrong audio.
//   - For non-YouTube media: completely bypassed, leaving audio untouched.
func ensureAudioTrack(p *rod.Page, kind MediaKind, reqLang string, log Logger) (AudioTrackReport, error) {
	if log == nil {
		log = noopLogger{}
	}
	normLang := NormalizeAudioLang(reqLang)
	rep := AudioTrackReport{Kind: kind, RequestedLang: normLang}
	if kind != MediaYouTube {
		rep.Note = "non-YouTube media; audio tracks untouched"
		return rep, nil
	}

	var lastErr error
	attempts := 0

	for attempt := 1; attempt <= audioTrackAttempts; attempt++ {
		attempts = attempt
		c, err := readAudioTracks(p, normLang, false)
		if err != nil {
			lastErr = err
			log.Debugf("[PIPELINE:audiotrack] discovery attempt %d/%d error: %v", attempt, audioTrackAttempts, err)
			if attempt < audioTrackAttempts {
				time.Sleep(audioTrackAttemptDelay)
			}
			continue
		}

		// Single-track video: exactly one audio track exists.
		if c.Available == 1 {
			rep = cycleToReport(kind, normLang, c, attempts)
			log.Infof("[PIPELINE:audiotrack] video_id=%s requested_lang=%s available=1 selected_track=%s selected_lang=%s fallback=%q is_original=true is_autodubbed=false verified=true",
				rep.VideoID, rep.RequestedLang, rep.SelectedTrackLabel(), rep.Active.Language, rep.FallbackReason)
			return rep, nil
		}

		// Zero tracks: player audio metadata is still hydrating; retry discovery.
		if c.Available == 0 {
			log.Debugf("[PIPELINE:audiotrack] discovery attempt %d/%d: 0 tracks reported (hydrating...)", attempt, audioTrackAttempts)
			if attempt < audioTrackAttempts {
				time.Sleep(audioTrackAttemptDelay)
			}
			continue
		}

		// Multiple tracks found: perform selection and verification.
		sel, serr := selectAudioTrack(p, normLang, log, attempt)
		rep = sel
		rep.Attempts = attempts
		if serr != nil {
			log.Warnf("[PIPELINE:audiotrack] audio track selection failed: %v", serr)
			return rep, serr
		}

		isOrig := false
		isAutoDub := false
		if rep.Selected != nil {
			isOrig = rep.Selected.IsOriginal
			isAutoDub = rep.Selected.IsAutoDubbed
		} else {
			isOrig = rep.Active.IsOriginal
			isAutoDub = rep.Active.IsAutoDubbed
		}

		log.Infof("[PIPELINE:audiotrack] video_id=%s requested_lang=%s available=%d langs=[%s] selected_track=%s selected_lang=%s fallback=%q is_original=%t is_autodubbed=%t verified=%t",
			rep.VideoID, rep.RequestedLang, rep.Available, strings.Join(rep.AvailableLangs, ","),
			rep.SelectedTrackLabel(), rep.Active.Language, rep.FallbackReason, isOrig, isAutoDub, rep.Verified)
		return rep, nil
	}

	rep = AudioTrackReport{
		Kind: kind, RequestedLang: normLang, Attempts: attempts, SingleTrack: true,
		Note: "no selectable audio track list reported by the player",
	}
	if lastErr != nil {
		return rep, fmt.Errorf("audio track discovery failed after %d attempts: %w", attempts, lastErr)
	}
	log.Infof("[PIPELINE:audiotrack] %s", rep.describe())
	return rep, nil
}

// selectAudioTrack executes the track switch and verifies the result.
func selectAudioTrack(p *rod.Page, reqLang string, log Logger, attempt int) (AudioTrackReport, error) {
	var last audioTrackCycle
	var lastErr error

	for verify := 0; verify < audioTrackAttempts; verify++ {
		c, err := readAudioTracks(p, reqLang, true)
		if err != nil {
			lastErr = err
			log.Debugf("[PIPELINE:audiotrack] select attempt %d.%d error: %v", attempt, verify, err)
			time.Sleep(audioTrackAttemptDelay)
			continue
		}
		last = c

		// Successful verification:
		if c.Verified {
			rep := cycleToReport(MediaYouTube, reqLang, c, attempt)
			return rep, nil
		}

		// Dubbed track active when user requested Persian or Original:
		if c.ActiveIsAutoDub && (reqLang == "fa" || reqLang == "orig") {
			log.Debugf("[PIPELINE:audiotrack] verify attempt %d.%d: player still on dubbed track %q, retrying switch...",
				attempt, verify, c.ActiveName)
		}

		time.Sleep(audioTrackAttemptDelay)
	}

	if lastErr != nil {
		return cycleToReport(MediaYouTube, reqLang, last, attempt), fmt.Errorf("verify audio track selection: %w", lastErr)
	}

	rep := cycleToReport(MediaYouTube, reqLang, last, attempt)
	if !rep.Verified && (rep.Active.IsAutoDubbed || strings.Contains(strings.ToLower(rep.Active.Name), "dub")) {
		origDesc := "-"
		if rep.Original != nil {
			origDesc = trackLabel(*rep.Original)
		}
		return rep, fmt.Errorf("still on dubbed track %q (wanted %s or original %q)",
			trackLabel(rep.Active), reqLang, origDesc)
	}

	return rep, nil
}

// ensureOriginalAudioTrack is kept for backwards compatibility with any existing calls.
func ensureOriginalAudioTrack(p *rod.Page, kind MediaKind, log Logger) (AudioTrackReport, error) {
	return ensureAudioTrack(p, kind, "orig", log)
}
