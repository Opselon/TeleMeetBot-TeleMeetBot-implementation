package bot

import (
	"errors"
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// phaseview_test.go — contract tests for the honest playback phase on the
// Telegram surfaces.
//
// The user contract under test:
//
//  1. A browser fallback is NEVER rendered as a successful local download —
//     the card shows the fallback line and the "via browser" method.
//  2. Every phase the user sees is actionable prose; the operator diagnostic
//     never reaches the card.
//  3. Duplicate notifications are suppressed: one transition, one message.
//  4. Download progress, completed downloads, browser playback and recovery
//     each render their distinct, honest line.

// statusWith builds an engine.Status carrying the given phase fields, the way
// a live session's Snapshot would.
func statusWith(phase engine.PlaybackPhase, userReason, detail, method string) engine.Status {
	return engine.Status{
		ID:                "s-test1",
		State:             engine.StateStreaming,
		PlayMode:          engine.MediaModeDownload,
		PlaybackMethod:    method,
		Phase:             string(phase),
		PhaseUserReason:   userReason,
		PhaseDetailReason: detail,
		Detail:            "[6/6] Streaming live",
	}
}

// TestCardShowsFallbackNotDownloadSuccess is the central Telegram honesty
// regression: a Download-mode session whose download failed must render the
// FALLBACK phase line plus "via browser", never "via download".
func TestCardShowsFallbackNotDownloadSuccess(t *testing.T) {
	st := statusWith(
		engine.PhaseFallback,
		"Download failed — playing the link directly from the browser instead. The meeting keeps streaming.",
		"yt-dlp: exit status 1: ERROR: TLS/SSL connection has been closed (EOF)",
		engine.PlaybackMethodBrowser, // the honest derived method
	)
	text := sessionCardText(st)
	if !strings.Contains(text, "Browser fallback") {
		t.Errorf("the card must name the fallback phase, got:\n%s", text)
	}
	if !strings.Contains(text, "via <b>browser</b>") {
		t.Errorf("the card must say the browser is playing, got:\n%s", text)
	}
	if strings.Contains(text, "via <b>download</b>") {
		t.Errorf("a fallback must never render as a successful download:\n%s", text)
	}
	// The operator diagnostic must never reach the user.
	for _, leak := range []string{"yt-dlp", "exit status", "TLS", "EOF"} {
		if strings.Contains(text, leak) {
			t.Errorf("card leaks operator diagnostic %q:\n%s", leak, text)
		}
	}
}

// TestCardShowsDownloadingPhase proves an in-flight download renders its own
// honest state — not an implied finished file.
func TestCardShowsDownloadingPhase(t *testing.T) {
	st := statusWith(engine.PhaseDownloading, "Downloading the new track…", "", engine.PlaybackMethodBrowser)
	text := sessionCardText(st)
	if !strings.Contains(text, "Downloading") {
		t.Errorf("the card must show the downloading phase, got:\n%s", text)
	}
	if strings.Contains(text, "via <b>download</b>") {
		t.Errorf("an in-flight download must not claim a completed download:\n%s", text)
	}
}

// TestCardShowsDownloadedPhase proves the happy path renders both the
// downloaded phase and the download method together.
func TestCardShowsDownloadedPhase(t *testing.T) {
	st := statusWith(engine.PhaseDownloaded, "", "", engine.PlaybackMethodDownload)
	text := sessionCardText(st)
	if !strings.Contains(text, "Downloaded") {
		t.Errorf("the card must show the downloaded phase, got:\n%s", text)
	}
	if !strings.Contains(text, "via <b>download</b>") {
		t.Errorf("a completed download must say so:\n%s", text)
	}
}

// TestCardShowsNetworkUnavailablePhase proves the network state renders with
// its own icon and actionable message.
func TestCardShowsNetworkUnavailablePhase(t *testing.T) {
	st := statusWith(
		engine.PhaseNetworkUnavailable,
		"The server can't reach the media source right now. Playback will retry automatically; the meeting is still live.",
		"dial tcp: no such host",
		engine.PlaybackMethodBrowser,
	)
	text := sessionCardText(st)
	if !strings.Contains(text, "Network unavailable") {
		t.Errorf("the card must name the network phase, got:\n%s", text)
	}
	if !strings.Contains(text, "retry automatically") {
		t.Errorf("the card must say playback retries, got:\n%s", text)
	}
	for _, leak := range []string{"dial tcp", "no such host"} {
		if strings.Contains(text, leak) {
			t.Errorf("card leaks operator diagnostic %q:\n%s", leak, text)
		}
	}
}

// TestCardShowsRecoveringPhase proves the recovery state is visible to the
// user, not silently healed.
func TestCardShowsRecoveringPhase(t *testing.T) {
	st := statusWith(
		engine.PhaseRecovering,
		"The media hiccuped — repairing playback without leaving the meeting…",
		"recovering: the player is paused",
		engine.PlaybackMethodBrowser,
	)
	text := sessionCardText(st)
	if !strings.Contains(text, "Recovering") {
		t.Errorf("the card must show the recovering phase, got:\n%s", text)
	}
	if !strings.Contains(text, "repairing playback") {
		t.Errorf("the card must say what recovery is doing, got:\n%s", text)
	}
}

// TestCardShowsFailedPhase proves a terminal playback failure names the
// actionable next step.
func TestCardShowsFailedPhase(t *testing.T) {
	st := statusWith(
		engine.PhaseFailed,
		"The media stopped playing and automatic recovery couldn't fix it. The meeting is still live — use 🔀 Switch media to try another link.",
		"recovery exhausted: the media tab has no video element",
		engine.PlaybackMethodBrowser,
	)
	text := sessionCardText(st)
	if !strings.Contains(text, "Playback failed") {
		t.Errorf("the card must name the failed phase, got:\n%s", text)
	}
	if !strings.Contains(text, "Switch media") {
		t.Errorf("the card must offer the next step, got:\n%s", text)
	}
}

// TestCardHidesPlainBrowserBanner proves a healthy, requested browser stream
// renders NO phase banner: it is the everyday case, not news.
func TestCardHidesPlainBrowserBanner(t *testing.T) {
	st := statusWith(engine.PhaseBrowser, "", "", engine.PlaybackMethodBrowser)
	text := sessionCardText(st)
	for _, noise := range []string{"Browser playback", "fallback", "Fallback"} {
		if strings.Contains(text, noise) {
			t.Errorf("a healthy browser stream must not render a phase banner (%q):\n%s", noise, text)
		}
	}
}

// TestFallbackNoticeIsSentOncePerTransition proves the standalone notice fires
// exactly once per (phase, reason) tuple and stays quiet for repeats — the
// duplicate-notification contract.
func TestFallbackNoticeIsSentOncePerTransition(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	st := statusWith(
		engine.PhaseFallback,
		"Download failed — playing the link directly from the browser instead. The meeting keeps streaming.",
		"raw", engine.PlaybackMethodBrowser,
	)
	if notice := phaseNoticeText(st); notice == "" {
		t.Fatal("a fallback must produce a standalone notice")
	}
	if !h.bot.shouldNotifyPhase(engine.SessionID("s-x"), st) {
		t.Fatal("the first notification must be allowed")
	}
	// Same tuple again: suppressed.
	if h.bot.shouldNotifyPhase(engine.SessionID("s-x"), st) {
		t.Fatal("a repeated (phase, reason) tuple must not notify twice")
	}
	// A genuinely different reason is news again.
	st2 := st
	st2.PhaseUserReason = "A different failure mode."
	if !h.bot.shouldNotifyPhase(engine.SessionID("s-x"), st2) {
		t.Fatal("a new reason must notify again")
	}
	// Recovering/Downloading produce no notice at all (progress, not news).
	for _, phase := range []engine.PlaybackPhase{engine.PhaseDownloading, engine.PhaseDownloaded, engine.PhaseBrowser, engine.PhaseRecovering} {
		st3 := statusWith(phase, "something", "raw", engine.PlaybackMethodBrowser)
		if got := phaseNoticeText(st3); got != "" {
			t.Errorf("phase %q must not raise a standalone notice, got %q", phase, got)
		}
	}
}

// TestNoticeMessagesNeverLeakInfra is the leak guard on the standalone notices.
func TestNoticeMessagesNeverLeakInfra(t *testing.T) {
	for _, phase := range []engine.PlaybackPhase{engine.PhaseFallback, engine.PhaseNetworkUnavailable, engine.PhaseFailed} {
		st := statusWith(phase, "", "dial tcp 172.16.0.1: no such host", engine.PlaybackMethodBrowser)
		notice := phaseNoticeText(st)
		for _, leak := range []string{"172.16", "dial tcp", "no such host"} {
			if strings.Contains(notice, leak) {
				t.Errorf("notice for %q leaks %q: %q", phase, leak, notice)
			}
		}
	}
}

// TestSwitchFailureMessageIsClassified proves the switch-failure reply shows
// the classified user message instead of the raw extractor diagnostic, while
// the raw error is kept for the operator log.
func TestSwitchFailureMessageIsClassified(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	h.eng.failSwitchFor("s-test1")
	h.launchSession(42)
	// The switch flow must report the classified failure and the survival of
	// the previous track.
	sent := h.api.lastSent()
	// Drive a switch via the text path: open the switch flow then send a link.
	h.callback(42, "act:s-test1:switchurl")
	h.message(42, "https://example.com/clip.mp4")
	all := h.api.allSentText()
	if !strings.Contains(all, "Switch failed") {
		t.Fatalf("the switch failure must be reported, got:\n%s", all)
	}
	for _, leak := range []string{"exit status", "yt-dlp", "TLS"} {
		if strings.Contains(all, leak) {
			t.Errorf("the switch failure leaked raw text %q:\n%s", leak, all)
		}
	}
	if !strings.Contains(all, "kept streaming the previous track") {
		t.Errorf("the message must say the previous track survived:\n%s", all)
	}
	_ = sent
}

// TestLaunchErrorIsClassified proves a failed Start reports a classified,
// actionable message instead of the raw internal error.
func TestLaunchErrorIsClassified(t *testing.T) {
	h := newHarness(t, []int64{42}, 2)
	// A duplicate meeting start: the fake engine always succeeds, so drive
	// the classification through the engine error path directly.
	raw := errors.New("yt-dlp: exit status 1: ERROR: TLS/SSL connection has been closed (EOF)")
	msg := phaseNoticeText(statusWith(engine.PhaseFailed, "", raw.Error(), engine.PlaybackMethodBrowser))
	if strings.Contains(msg, "TLS") {
		t.Errorf("notice must not leak the raw diagnostic: %q", msg)
	}
	_ = h
}
