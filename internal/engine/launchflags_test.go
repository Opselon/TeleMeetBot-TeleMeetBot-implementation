package engine

import (
	"strings"
	"testing"
)

// TestLaunchFlagsGPUDefault pins the CPU contract for headless runs: software
// compositing (--disable-gpu), never SwiftShader. SwiftShader rasterises every
// shared frame through a software GL pipeline and measured ~6x the host CPU of
// plain software compositing for identical frames on a 2-core VPS (83% -> 14%
// of one core for a 720p30 decode+share).
func TestLaunchFlagsGPUDefault(t *testing.T) {
	flags := launchFlags(BrowserConfig{Headless: true}, MeetOptions{})
	joined := strings.Join(flags, "\n")
	if !hasFlag(flags, "disable-gpu") {
		t.Fatalf("headless launch must disable the GPU process: %v", flags)
	}
	for _, banned := range []string{"use-angle=swiftshader", "use-gl=angle", "enable-unsafe-swiftshader"} {
		if hasFlag(flags, banned) {
			t.Fatalf("headless launch must not force SwiftShader (%s): %v", banned, flags)
		}
	}
	if !strings.Contains(joined, "disable-dev-shm-usage") {
		t.Fatal("disable-dev-shm-usage missing")
	}
}

// TestLaunchFlagsGPUModes pins the browser.gpu escape hatches: an operator on a
// real-GPU host can ask for Chromium's own choice, and the SwiftShader path must
// still be reachable without a rebuild.
func TestLaunchFlagsGPUModes(t *testing.T) {
	cases := []struct {
		name     string
		cfg      BrowserConfig
		want     []string
		mustNot  []string
		wantMode string
	}{
		{"headless default", BrowserConfig{Headless: true}, []string{"disable-gpu"}, []string{"use-angle=swiftshader"}, GPUModeOff},
		{"explicit off", BrowserConfig{Headless: true, GPU: GPUModeOff}, []string{"disable-gpu"}, nil, GPUModeOff},
		{"swiftshader", BrowserConfig{Headless: true, GPU: GPUModeSwiftShader}, []string{"use-angle=swiftshader", "use-gl=angle", "enable-unsafe-swiftshader"}, []string{"disable-gpu"}, GPUModeSwiftShader},
		{"auto headless", BrowserConfig{Headless: true, GPU: GPUModeAuto}, nil, []string{"disable-gpu", "use-angle=swiftshader"}, GPUModeAuto},
		// Non-headless (Xvfb / real display): leave the compositor alone.
		{"windowed default", BrowserConfig{Headless: false}, nil, []string{"disable-gpu"}, GPUModeAuto},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gpuMode(tc.cfg); got != tc.wantMode {
				t.Fatalf("gpuMode = %q, want %q", got, tc.wantMode)
			}
			flags := launchFlags(tc.cfg, MeetOptions{})
			for _, w := range tc.want {
				if !hasFlag(flags, w) {
					t.Fatalf("missing %q in %v", w, flags)
				}
			}
			for _, n := range tc.mustNot {
				if hasFlag(flags, n) {
					t.Fatalf("unexpected %q in %v", n, flags)
				}
			}
		})
	}
}

// TestLaunchFlagsNeverFakeCapture keeps the green-screen guard in place: the
// fake device/UI switches make getDisplayMedia return Chromium's test pattern.
func TestLaunchFlagsNeverFakeCapture(t *testing.T) {
	flags := launchFlags(BrowserConfig{Headless: true}, MeetOptions{PresentMode: "tab", CaptureSource: "TeleMeet Media"})
	for _, banned := range []string{"use-fake-device-for-media-stream", "use-fake-ui-for-media-stream"} {
		for _, f := range flags {
			if strings.HasPrefix(f, banned) {
				t.Fatalf("fake capture switch present: %q", f)
			}
		}
	}
	if !hasFlag(flags, "auto-select-tab-capture-source-by-title=TeleMeet Media") {
		t.Fatalf("tab capture auto-select missing: %v", flags)
	}
}

// TestLaunchFlagsExtraArgsLast keeps the documented override contract: extra_args
// are appended last so Chromium sees them after the defaults (last value wins for
// single-value switches).
func TestLaunchFlagsExtraArgsLast(t *testing.T) {
	flags := launchFlags(BrowserConfig{Headless: true, ExtraArgs: []string{"window-size=640,480"}}, MeetOptions{})
	if len(flags) == 0 || !strings.HasPrefix(flags[len(flags)-1], "window-size=640,480") {
		t.Fatalf("extra_args must be applied last: %v", flags)
	}
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}
