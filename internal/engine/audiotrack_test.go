package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// htmlMultiAudioPlayer generates a mock HTML page that emulates YouTube's
// player object and caption response.
func htmlMultiAudioPlayer(script string) string {
	return fmt.Sprintf(`<!doctype html><html><head><title>Mock YouTube</title></head>
<body style="background:#000">
<div id="movie_player"></div>
<script>
%s
</script>
</body></html>`, script)
}

// servePage starts a one-page HTTP server and registers its cleanup.
func servePage(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestIntegrationAudioTrackLifecycle drives every deterministic audio-track
// scenario through one real Chromium: discovery/selection, already-original,
// single-track videos, late hydration retries, player reinitialization,
// unavailable originals, an ineffective switch, non-YouTube media and the UI
// settings-menu fallback.
//
// One browser serves all subtests because a Chromium launch + teardown costs
// ~25s on the media-proxy path, while each page load costs milliseconds.
func TestIntegrationAudioTrackLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	b, closeFn := newTestBrowser(t)
	defer closeFn()

	t.Run("DiscoveryAndSelection", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
const tracks = [
  {id: "ja.4", name: "Japanese (original)"},
  {id: "en-US.10", name: "English (US)"}
];
window.__active = tracks[1]; // default: dubbed
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => tracks.map(t => ({
  getId: () => t.id,
  getName: () => t.name,
  Vp: { id: t.id }
}));
p.getAudioTrack = () => ({
  getId: () => window.__active.id,
  getName: () => window.__active.name,
  Vp: { id: window.__active.id }
});
p.setAudioTrack = (track) => {
  const id = track.getId ? track.getId() : (track.Vp && track.Vp.id);
  const found = tracks.find(t => t.id === id);
  if (found) window.__active = found;
};
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [
        { audioTrackId: "ja.4", defaultCaptionTrackIndex: 0 },
        { audioTrackId: "en-US.10", defaultCaptionTrackIndex: 1 }
      ],
      defaultAudioTrackIndex: 0
    }
  }
});
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureOriginalAudioTrack(page, MediaYouTube, testLogger{t})
		if err != nil {
			t.Fatalf("ensureOriginalAudioTrack failed: %v", err)
		}
		if rep.Available != 2 {
			t.Fatalf("expected 2 available tracks, got %d", rep.Available)
		}
		if rep.Original == nil || rep.Original.ID != "ja.4" {
			t.Fatalf("expected original track ja.4, got %+v", rep.Original)
		}
		if !rep.Switched || !rep.Verified {
			t.Fatalf("expected switched+verified, got %+v", rep)
		}
		if rep.Method != "player-api" {
			t.Fatalf("expected method player-api, got %s", rep.Method)
		}
		if got := evaluate(t, page, `window.__active.id`); got != "ja.4" {
			t.Fatalf("page active track is %q, want ja.4", got)
		}
	})

	t.Run("AlreadyOriginal", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
const tracks = [
  {id: "fr.2", name: "Francais (original)"},
  {id: "en.1", name: "English"}
];
window.__active = tracks[0]; // already original
window.__switched = false;
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => tracks.map(t => ({ getId: () => t.id, getName: () => t.name, Vp: { id: t.id } }));
p.getAudioTrack = () => ({ getId: () => window.__active.id, getName: () => window.__active.name, Vp: { id: window.__active.id } });
p.setAudioTrack = () => { window.__switched = true; };
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [{ audioTrackId: "fr.2" }, { audioTrackId: "en.1" }],
      defaultAudioTrackIndex: 0
    }
  }
});
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureOriginalAudioTrack(page, MediaYouTube, testLogger{t})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rep.Switched {
			t.Fatalf("expected switched=false when already on original, got %+v", rep)
		}
		if !rep.Verified {
			t.Fatalf("expected verified=true, got %+v", rep)
		}
		if got := evaluate(t, page, `window.__switched`); got != "false" {
			t.Fatalf("setAudioTrack was unexpectedly invoked: %s", got)
		}
	})

	t.Run("SingleTrack", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => [{ getId: () => "en.1", getName: () => "English", Vp: { id: "en.1" } }];
p.getAudioTrack = () => ({ getId: () => "en.1", getName: () => "English", Vp: { id: "en.1" } });
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [{ audioTrackId: "en.1" }],
      defaultAudioTrackIndex: 0
    }
  }
});
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureOriginalAudioTrack(page, MediaYouTube, testLogger{t})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rep.Available != 1 || !rep.SingleTrack {
			t.Fatalf("expected available=1 single=true, got %+v", rep)
		}
	})

	t.Run("RetriesLateHydration", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
let polls = 0;
const tracks = [
  {id: "ko.1", name: "Korean (original)"},
  {id: "en.2", name: "English (dubbed)"}
];
window.__active = tracks[1];
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => {
  polls++;
  if (polls < 3) return []; // first 2 calls empty (simulating late hydration)
  return tracks.map(t => ({ getId: () => t.id, getName: () => t.name, Vp: { id: t.id } }));
};
p.getAudioTrack = () => ({ getId: () => window.__active.id, getName: () => window.__active.name, Vp: { id: window.__active.id } });
p.setAudioTrack = (track) => {
  const id = track.getId ? track.getId() : (track.Vp && track.Vp.id);
  const found = tracks.find(t => t.id === id);
  if (found) window.__active = found;
};
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [{ audioTrackId: "ko.1" }, { audioTrackId: "en.2" }],
      defaultAudioTrackIndex: 0
    }
  }
});
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureOriginalAudioTrack(page, MediaYouTube, testLogger{t})
		if err != nil {
			t.Fatalf("ensureOriginalAudioTrack failed with retries: %v", err)
		}
		if rep.Attempts < 3 {
			t.Fatalf("expected at least 3 discovery attempts, got %d", rep.Attempts)
		}
		if !rep.Verified || !rep.Switched {
			t.Fatalf("expected verified+switched, got %+v", rep)
		}
		if rep.Original == nil || rep.Original.ID != "ko.1" {
			t.Fatalf("expected original ko.1, got %+v", rep.Original)
		}
	})

	t.Run("PlayerReinitialization", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
const tracks = [
  {id: "de.1", name: "Deutsch (original)"},
  {id: "en.2", name: "English"}
];
window.__active = tracks[1];
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => tracks.map(t => ({ getId: () => t.id, getName: () => t.name, Vp: { id: t.id } }));
p.getAudioTrack = () => ({ getId: () => window.__active.id, getName: () => window.__active.name, Vp: { id: window.__active.id } });
p.setAudioTrack = (track) => {
  const id = track.getId ? track.getId() : (track.Vp && track.Vp.id);
  const found = tracks.find(t => t.id === id);
  if (found) window.__active = found;
};
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [{ audioTrackId: "de.1" }, { audioTrackId: "en.2" }],
      defaultAudioTrackIndex: 0
    }
  }
});
window.reinitPlayer = () => { window.__active = tracks[1]; };
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		// Phase 1: initial switch
		rep1, err := ensureOriginalAudioTrack(page, MediaYouTube, testLogger{t})
		if err != nil {
			t.Fatalf("phase 1 failed: %v", err)
		}
		if !rep1.Switched || rep1.Selected == nil || rep1.Selected.ID != "de.1" {
			t.Fatalf("phase 1 switch failed: %+v", rep1)
		}

		// Phase 2: simulate player reinitialization back to the dub
		evaluate(t, page, `window.reinitPlayer()`)
		if cur := evaluate(t, page, `window.__active.id`); cur != "en.2" {
			t.Fatalf("reinit setup failed: got %q, want en.2", cur)
		}

		// Phase 3: enforcement after reinitialization
		rep2, err := ensureOriginalAudioTrack(page, MediaYouTube, testLogger{t})
		if err != nil {
			t.Fatalf("phase 3 failed: %v", err)
		}
		if !rep2.Switched || rep2.Selected == nil || rep2.Selected.ID != "de.1" {
			t.Fatalf("phase 3 did not force the original track: %+v", rep2)
		}
		if cur := evaluate(t, page, `window.__active.id`); cur != "de.1" {
			t.Fatalf("after reinit active track is %q, want de.1", cur)
		}
	})

	t.Run("UnavailableOriginal", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
const tracks = [
  {id: "lang-a", name: "Language A"},
  {id: "lang-b", name: "Language B"}
];
window.__active = tracks[0];
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => tracks.map(t => ({ getId: () => t.id, getName: () => t.name, Vp: { id: t.id } }));
p.getAudioTrack = () => ({ getId: () => window.__active.id, getName: () => window.__active.name, Vp: { id: window.__active.id } });
p.setAudioTrack = () => {};
p.getPlayerResponse = () => ({}); // no captions or original markers
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureOriginalAudioTrack(page, MediaYouTube, testLogger{t})
		if err != nil {
			t.Fatalf("expected nil error on ambiguous original track, got %v", err)
		}
		if rep.Original != nil {
			t.Fatalf("expected nil Original track, got %+v", rep.Original)
		}
		if !strings.Contains(rep.Note, "no original audio marker") {
			t.Fatalf("expected note explaining retained track, got %q", rep.Note)
		}
	})

	t.Run("SwitchFailsDubbedRetained", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
const tracks = [
  {id: "es.1", name: "Espanol (original)"},
  {id: "en.2", name: "English (dubbed)"}
];
window.__active = tracks[1]; // dubbed
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => tracks.map(t => ({ getId: () => t.id, getName: () => t.name, Vp: { id: t.id } }));
p.getAudioTrack = () => ({ getId: () => window.__active.id, getName: () => window.__active.name, Vp: { id: window.__active.id } });
p.setAudioTrack = () => {}; // broken no-op: never switches
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [{ audioTrackId: "es.1" }, { audioTrackId: "en.2" }],
      defaultAudioTrackIndex: 0
    }
  }
});
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureOriginalAudioTrack(page, MediaYouTube, testLogger{t})
		if err == nil {
			t.Fatal("expected failure when a dubbed track cannot be switched, got nil")
		}
		if rep.Verified {
			t.Fatalf("expected verified=false on failed switch, got %+v", rep)
		}
		if !strings.Contains(err.Error(), "still on dubbed track") {
			t.Fatalf("error should cite the retained dubbed track: %v", err)
		}
	})

	t.Run("NonYouTubeUntouched", func(t *testing.T) {
		page := openPage(t, b, "data:text/html,<html><body>plain page</body></html>")
		defer func() { _ = page.Close() }()

		for _, kind := range []MediaKind{MediaDirect, MediaPage} {
			rep, err := ensureOriginalAudioTrack(page, kind, testLogger{t})
			if err != nil {
				t.Fatalf("kind %s failed: %v", kind, err)
			}
			if rep.Switched {
				t.Fatalf("kind %s had switched=true", kind)
			}
			if !strings.Contains(rep.Note, "non-YouTube media") {
				t.Fatalf("kind %s had unexpected note: %q", kind, rep.Note)
			}
		}
	})

	t.Run("UISettingsMenuFallback", func(t *testing.T) {
		url := servePage(t, `<!doctype html><html><head><title>Mock YouTube UI</title></head>
<body>
<div id="movie_player">
  <button class="ytp-settings-button">Settings</button>
  <div class="ytp-settings-menu" style="display:none">
    <div class="ytp-panel-menu">
      <div class="ytp-menuitem" id="audio-entry">Audio track</div>
    </div>
    <div class="ytp-audio-submenu" style="display:none">
      <div class="ytp-menuitem" role="menuitemradio" id="item-en" aria-checked="true">English</div>
      <div class="ytp-menuitem" role="menuitemradio" id="item-it" aria-checked="false">Italian (original)</div>
    </div>
  </div>
</div>
<script>
window.__selected = "English";
const btn = document.querySelector(".ytp-settings-button");
const menu = document.querySelector(".ytp-settings-menu");
const audioEntry = document.getElementById("audio-entry");
const panelMenu = document.querySelector(".ytp-panel-menu");
const subMenu = document.querySelector(".ytp-audio-submenu");
const itemEn = document.getElementById("item-en");
const itemIt = document.getElementById("item-it");

btn.addEventListener("click", () => {
  menu.style.display = menu.style.display === "none" ? "block" : "none";
});
audioEntry.addEventListener("click", () => {
  panelMenu.style.display = "none";
  subMenu.style.display = "block";
});
itemIt.addEventListener("click", () => {
  window.__selected = "Italian";
  itemIt.setAttribute("aria-checked", "true");
  itemEn.setAttribute("aria-checked", "false");
});
itemEn.addEventListener("click", () => {
  window.__selected = "English";
  itemEn.setAttribute("aria-checked", "true");
  itemIt.setAttribute("aria-checked", "false");
});

// No getAvailableAudioTracks / setAudioTrack API on this build.
const p = document.getElementById("movie_player");
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [{ audioTrackId: "it.1" }, { audioTrackId: "en.2" }],
      defaultAudioTrackIndex: 0
    }
  }
});
</script>
</body></html>`)
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureOriginalAudioTrack(page, MediaYouTube, testLogger{t})
		if err != nil {
			t.Fatalf("UI fallback failed: %v", err)
		}
		if !rep.Switched {
			t.Fatalf("expected switched=true via UI fallback, got %+v", rep)
		}
		if rep.Method != "ui-menu" {
			t.Fatalf("expected method ui-menu, got %s", rep.Method)
		}
		if got := evaluate(t, page, `window.__selected`); got != "Italian" {
			t.Fatalf("selected via UI = %q, want Italian", got)
		}
	})

	t.Run("PersianAudioSelectedWhenAvailable", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
const tracks = [
  {id: "fr.1", name: "French (original)", isOriginal: true},
  {id: "en.2", name: "English (dubbed)", isAutoDubbed: true},
  {id: "fa.3", name: "Persian (فارسی)", isAutoDubbed: false}
];
window.__active = tracks[1]; // default is dubbed English
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => tracks.map(t => ({
  getId: () => t.id,
  getName: () => t.name,
  Vp: { id: t.id },
  getLanguageInfo: () => ({ id: t.id.split(".")[0], name: t.name, isAutoDubbed: !!t.isAutoDubbed })
}));
p.getAudioTrack = () => ({
  getId: () => window.__active.id,
  getName: () => window.__active.name,
  Vp: { id: window.__active.id },
  getLanguageInfo: () => ({ id: window.__active.id.split(".")[0], name: window.__active.name, isAutoDubbed: !!window.__active.isAutoDubbed })
});
p.setAudioTrack = (track) => {
  const id = track.getId ? track.getId() : (track.Vp && track.Vp.id);
  const found = tracks.find(t => t.id === id);
  if (found) window.__active = found;
};
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [{ audioTrackId: "fr.1" }, { audioTrackId: "en.2" }, { audioTrackId: "fa.3" }],
      defaultAudioTrackIndex: 0
    }
  }
});
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureAudioTrack(page, MediaYouTube, "fa", testLogger{t})
		if err != nil {
			t.Fatalf("ensureAudioTrack failed for fa: %v", err)
		}
		if !rep.PersianAvailable {
			t.Fatal("expected PersianAvailable=true")
		}
		if rep.Selected == nil || rep.Selected.ID != "fa.3" {
			t.Fatalf("expected selected track fa.3, got %+v", rep.Selected)
		}
		if !rep.Switched || !rep.Verified {
			t.Fatalf("expected switched+verified, got %+v", rep)
		}
		if rep.Active.ID != "fa.3" {
			t.Fatalf("expected active track fa.3, got %+v", rep.Active)
		}
	})

	t.Run("PersianFallbackToOriginalWhenUnavailable", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
const tracks = [
  {id: "fr.1", name: "French (original)", isOriginal: true},
  {id: "en.2", name: "English (dubbed)", isAutoDubbed: true}
];
window.__active = tracks[1]; // default is dubbed English
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => tracks.map(t => ({
  getId: () => t.id,
  getName: () => t.name,
  Vp: { id: t.id },
  getLanguageInfo: () => ({ id: t.id.split(".")[0], name: t.name, isAutoDubbed: !!t.isAutoDubbed })
}));
p.getAudioTrack = () => ({
  getId: () => window.__active.id,
  getName: () => window.__active.name,
  Vp: { id: window.__active.id },
  getLanguageInfo: () => ({ id: window.__active.id.split(".")[0], name: window.__active.name, isAutoDubbed: !!window.__active.isAutoDubbed })
});
p.setAudioTrack = (track) => {
  const id = track.getId ? track.getId() : (track.Vp && track.Vp.id);
  const found = tracks.find(t => t.id === id);
  if (found) window.__active = found;
};
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [{ audioTrackId: "fr.1" }, { audioTrackId: "en.2" }],
      defaultAudioTrackIndex: 0
    }
  }
});
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureAudioTrack(page, MediaYouTube, "fa", testLogger{t})
		if err != nil {
			t.Fatalf("ensureAudioTrack failed for fa fallback: %v", err)
		}
		if rep.PersianAvailable {
			t.Fatal("expected PersianAvailable=false")
		}
		// Must fall back to French (original), NEVER remain on dubbed English!
		if rep.Selected == nil || rep.Selected.ID != "fr.1" {
			t.Fatalf("expected fallback to original track fr.1, got %+v", rep.Selected)
		}
		if rep.Active.ID != "fr.1" {
			t.Fatalf("expected active track switched to fr.1, got %+v", rep.Active)
		}
		if !rep.Switched || !rep.Verified {
			t.Fatalf("expected switched+verified to original, got %+v", rep)
		}
		if !strings.Contains(rep.FallbackReason, "Persian audio unavailable") {
			t.Fatalf("expected FallbackReason explaining fallback, got: %q", rep.FallbackReason)
		}
	})

	t.Run("EnglishOriginalSelected", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
const tracks = [
  {id: "en.1", name: "English (original)", isOriginal: true},
  {id: "es.2", name: "Spanish (dubbed)", isAutoDubbed: true}
];
window.__active = tracks[1];
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => tracks.map(t => ({
  getId: () => t.id,
  getName: () => t.name,
  Vp: { id: t.id },
  getLanguageInfo: () => ({ id: t.id.split(".")[0], name: t.name, isAutoDubbed: !!t.isAutoDubbed })
}));
p.getAudioTrack = () => ({
  getId: () => window.__active.id,
  getName: () => window.__active.name,
  Vp: { id: window.__active.id },
  getLanguageInfo: () => ({ id: window.__active.id.split(".")[0], name: window.__active.name, isAutoDubbed: !!window.__active.isAutoDubbed })
});
p.setAudioTrack = (track) => {
  const id = track.getId ? track.getId() : (track.Vp && track.Vp.id);
  const found = tracks.find(t => t.id === id);
  if (found) window.__active = found;
};
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [{ audioTrackId: "en.1" }, { audioTrackId: "es.2" }],
      defaultAudioTrackIndex: 0
    }
  }
});
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureAudioTrack(page, MediaYouTube, "en", testLogger{t})
		if err != nil {
			t.Fatalf("ensureAudioTrack failed for en: %v", err)
		}
		if rep.Selected == nil || rep.Selected.ID != "en.1" {
			t.Fatalf("expected en.1 selected, got %+v", rep.Selected)
		}
		if !rep.Switched || !rep.Verified {
			t.Fatalf("expected switched+verified, got %+v", rep)
		}
	})

	t.Run("AutoModePicksPersianThenOriginal", func(t *testing.T) {
		url := servePage(t, htmlMultiAudioPlayer(`
const tracks = [
  {id: "ja.1", name: "Japanese (original)", isOriginal: true},
  {id: "fa.2", name: "Persian", isAutoDubbed: false}
];
window.__active = tracks[0];
const p = document.getElementById("movie_player");
p.getAvailableAudioTracks = () => tracks.map(t => ({
  getId: () => t.id,
  getName: () => t.name,
  Vp: { id: t.id },
  getLanguageInfo: () => ({ id: t.id.split(".")[0], name: t.name, isAutoDubbed: false })
}));
p.getAudioTrack = () => ({
  getId: () => window.__active.id,
  getName: () => window.__active.name,
  Vp: { id: window.__active.id },
  getLanguageInfo: () => ({ id: window.__active.id.split(".")[0], name: window.__active.name, isAutoDubbed: false })
});
p.setAudioTrack = (track) => {
  const id = track.getId ? track.getId() : (track.Vp && track.Vp.id);
  const found = tracks.find(t => t.id === id);
  if (found) window.__active = found;
};
p.getPlayerResponse = () => ({
  captions: {
    playerCaptionsTracklistRenderer: {
      audioTracks: [{ audioTrackId: "ja.1" }, { audioTrackId: "fa.2" }],
      defaultAudioTrackIndex: 0
    }
  }
});
`))
		page := openPage(t, b, url)
		defer func() { _ = page.Close() }()

		rep, err := ensureAudioTrack(page, MediaYouTube, "auto", testLogger{t})
		if err != nil {
			t.Fatalf("ensureAudioTrack failed for auto: %v", err)
		}
		// Auto should prefer Persian when present
		if rep.Selected == nil || rep.Selected.ID != "fa.2" {
			t.Fatalf("expected auto to pick fa.2, got %+v", rep.Selected)
		}
	})
}

// TestIntegrationRealYouTubeAudioTrackLive is an opt-in live regression test
// against a real YouTube video with multiple audio tracks (original + dubs).
//
//	TELEMEETBOT_YOUTUBE_MULTI_AUDIO_URL=https://www.youtube.com/watch?v=... \
//	TELEMEETBOT_BROWSER=/opt/chrome-for-testing/chrome-linux64/chrome \
//	TELEMEETBOT_PROXY=http://172.17.0.1:3339 \
//	go test ./internal/engine -run TestIntegrationRealYouTubeAudioTrackLive -v -count=1
func TestIntegrationRealYouTubeAudioTrackLive(t *testing.T) {
	link := os.Getenv("TELEMEETBOT_YOUTUBE_MULTI_AUDIO_URL")
	if link == "" {
		link = os.Getenv("TELEMEETBOT_YOUTUBE_URL")
	}
	if link == "" {
		t.Skip("set TELEMEETBOT_YOUTUBE_MULTI_AUDIO_URL=https://www.youtube.com/watch?v=... to run the live multi-audio test")
	}

	bin := browserBin(t)
	cfg := BrowserConfig{
		Executable:   bin,
		Headless:     true,
		WindowWidth:  1280,
		WindowHeight: 800,
		Lang:         envOr("TELEMEETBOT_LANG", "en-US"),
		Proxy:        os.Getenv("TELEMEETBOT_PROXY"),
	}

	b, closeFn, err := launchBrowser(launchOptions{browser: cfg, forMeet: true}, testLogger{t})
	if err != nil {
		t.Fatalf("launch browser: %v", err)
	}
	defer closeFn()

	page, err := newPage(b, cfg)
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer func() { _ = page.Close() }()

	target, _, err := mediaTargetFor(link)
	if err != nil {
		t.Fatalf("target for %s: %v", link, err)
	}

	if err := page.Timeout(45 * time.Second).Navigate(target); err != nil {
		t.Fatalf("load media: %v", err)
	}
	_ = page.WaitLoad()

	st, err := waitMediaReady(page, testLogger{t}, 35*time.Second)
	if err != nil {
		t.Fatalf("media ready failed: %v", err)
	}
	t.Logf("media ready: %s", st.describe())

	rep, err := ensureOriginalAudioTrack(page, MediaYouTube, testLogger{t})
	if err != nil {
		t.Fatalf("ensureOriginalAudioTrack failed on real video: %v", err)
	}
	t.Logf("audio track report: %s", rep.describe())

	if rep.Available > 1 {
		if rep.Original == nil {
			t.Fatalf("multi-audio video must identify the original track: %+v", rep)
		}
		if !rep.Verified {
			t.Fatalf("original track selection was not verified: %+v", rep)
		}
		t.Logf("SUCCESS: multi-audio video verified on original track %s", rep.Original.ID)

		// Test requesting Persian ("fa") on real multi-audio video
		repFa, err := ensureAudioTrack(page, MediaYouTube, "fa", testLogger{t})
		if err != nil {
			t.Fatalf("ensureAudioTrack(fa) failed on real video: %v", err)
		}
		t.Logf("audio track fa report: %s", repFa.describe())
		if !repFa.Verified {
			t.Fatalf("fa track selection was not verified: %+v", repFa)
		}
		if !repFa.PersianAvailable {
			if repFa.Selected == nil || repFa.Selected.ID != rep.Original.ID {
				t.Fatalf("expected fallback to original track %s, got %+v", rep.Original.ID, repFa.Selected)
			}
			if !strings.Contains(repFa.FallbackReason, "Persian audio unavailable") {
				t.Fatalf("expected fallback reason explaining fallback, got: %q", repFa.FallbackReason)
			}
			t.Logf("SUCCESS: Persian unavailable fallback correctly selected original audio %s", repFa.Selected.ID)
		}
	} else {
		t.Logf("single-audio video (%d tracks); audio-track handling verified cleanly", rep.Available)
	}
}
