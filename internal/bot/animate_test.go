package bot

import (
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

func TestRenderAnimatedBar(t *testing.T) {
	cases := []struct {
		pct  int
		want string
	}{
		{0, "▱▱▱▱▱▱▱▱▱▱"},
		{45, "▰▰▰◆▱▱▱▱▱▱"}, // head is part of the filled count (4/10 at 45%)
		{100, "▰▰▰▰▰▰▰▰▰◆"},
		{5, "◆▱▱▱▱▱▱▱▱▱"}, // started but no full block yet: head on slot 1
	}
	for _, tc := range cases {
		if got := renderAnimatedBar(tc.pct, 10); got != tc.want {
			t.Errorf("renderAnimatedBar(%d) = %q, want %q", tc.pct, got, tc.want)
		}
	}
}

func TestRenderCometSweeps(t *testing.T) {
	a := renderComet(10)
	animCounter.Add(1)
	b := renderComet(10)
	if a == b {
		t.Fatal("comet must move between frames")
	}
	if strings.Count(a, "▰") != 2 || strings.Count(a, "◆") != 1 {
		t.Errorf("comet shape wrong: %q", a)
	}
}

func TestSpinnerFramesCycle(t *testing.T) {
	a := spinner()
	b := spinner()
	if a == b || a == "" {
		t.Fatalf("spinner must advance: %q vs %q", a, b)
	}
}

func TestDownloadCardAnimatedLayout(t *testing.T) {
	st := engine.Status{
		ID: "s-x", State: "joining",
		Detail: "[1/6] Downloading media: 45%",
		DownloadProgress: engine.DownloadProgress{
			Active: true, Bytes: 45 << 20, Total: 100 << 20,
			Speed: 13107200, Percent: 45, Stage: "download", ETASeconds: 95,
		},
	}
	card := sessionCardText(st)
	for _, want := range []string{"Downloading:", "▰▰▰◆▱▱▱▱▱▱", "45%", "1m 35s", "12.5 MiB/s", "45.0", "100.0 MiB"} {
		if !strings.Contains(card, want) {
			t.Errorf("card missing %q:\n%s", want, card)
		}
	}
	// Spinner frame present on the headline.
	if !strings.Contains(card, "⬇️") {
		t.Errorf("card missing download headline:\n%s", card)
	}
}

func TestConnectStageCard(t *testing.T) {
	st := engine.Status{
		ID: "s-y", State: "starting",
		DownloadProgress: engine.DownloadProgress{
			Active: true, Stage: "connect", Percent: -1,
		},
	}
	card := sessionCardText(st)
	if !strings.Contains(card, "Connecting") || !strings.Contains(card, "▱") {
		t.Errorf("connect stage card wrong:\n%s", card)
	}
}
