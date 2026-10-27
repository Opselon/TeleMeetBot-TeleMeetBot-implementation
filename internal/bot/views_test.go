package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

func TestSessionCardTextIncludesServerResources(t *testing.T) {
	st := engine.Status{
		ID:          "s-c645fee3",
		Title:       "TeleMeet Media",
		MeetURL:     "https://meet.google.com/amw-mtyr-opp?hl=en-US",
		Cur:         1812.0, // 30:12
		Dur:         3965.0, // 1:06:05
		Volume:      100,
		State:       engine.StatePaused,
		Detail:      "paused by operator",
		AudioTrack:  "Default",
		AudioLang:   "fa",
		PlayMode:    "download",
		QueueLength: 1,
		QueueIndex:  0,
		Created:     time.Now().Add(-45 * time.Minute),
	}

	card := sessionCardText(st)

	// Verify all existing fields remain present
	checks := []string{
		"⏸ <b>Session s-c645fee3</b>",
		"🎞 Media: TeleMeet Media",
		"🔗 Meet: <code>https://meet.google.com/amw-mtyr-opp?hl=en-US</code>",
		"⏱ Playback: 30:12 / 1:06:05",
		"🔊 Volume: 100%",
		"📶 State: <b>paused</b> — paused by operator",
		"🎵 Audio: <b>Default</b> (pref: fa)",
		"⬇️ Mode: <b>Download &amp; Stream</b>",
		"📋 Playlist: <b>1 / 1</b>",
		"⏳ Elapsed: 45m",
		"🖥 Server Resources",
		"⚙️ CPU:",
		"🧠 RAM:",
		"🔄 Swap:",
		"💽 Storage:",
	}

	for _, check := range checks {
		if !strings.Contains(card, check) {
			t.Errorf("missing expected field %q in sessionCardText output:\n%s", check, card)
		}
	}

	// Verify Server Resources appears immediately after Elapsed
	elapsedIdx := strings.Index(card, "⏳ Elapsed:")
	resourcesIdx := strings.Index(card, "🖥 Server Resources")
	if elapsedIdx == -1 || resourcesIdx == -1 || resourcesIdx < elapsedIdx {
		t.Fatalf("Server Resources should appear directly after Elapsed line: elapsedIdx=%d, resourcesIdx=%d", elapsedIdx, resourcesIdx)
	}
}

func TestSessionCardTextPreservesErrorAtEnd(t *testing.T) {
	st := engine.Status{
		ID:      "s-err001",
		State:   engine.StateError,
		Error:   "media failure occurred",
		Created: time.Now().Add(-5 * time.Minute),
	}

	card := sessionCardText(st)
	if !strings.Contains(card, "🖥 Server Resources") {
		t.Errorf("expected Server Resources section in error card:\n%s", card)
	}
	if !strings.HasSuffix(strings.TrimSpace(card), "media failure occurred") {
		t.Errorf("expected Error to remain at the very end of the card, got:\n%s", card)
	}
}
