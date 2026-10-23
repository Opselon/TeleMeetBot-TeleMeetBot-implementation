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
	flags := launchFlags(BrowserConfig{Headless: true}, MeetOptions{}, false)
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

// TestLaunchFlagsGPUModes pins the browser.gpu contract, including the
// regression guard: a real meeting session must never launch GPU-less, because
// Chromium then hands Google Meet no WebGL context and Meet stops publishing
// presentation video while audio keeps flowing (s-39e3a90c: participants heard
// the clip but saw a blank share). Capture tests and probes may still use the
// cheapest mode.
func TestLaunchFlagsGPUModes(t *testing.T) {
	cases := []struct {
		name     string
		cfg      BrowserConfig
		forMeet  bool
		want     []string
		mustNot  []string
		wantMode string
	}{
		{"headless default (probe)", BrowserConfig{Headless: true}, false,
			[]string{"disable-gpu"}, []string{"use-angle=swiftshader"}, GPUModeOff},
		{"headless default (Meet)", BrowserConfig{Headless: true}, true,
			[]string{"use-angle=swiftshader", "use-gl=angle", "enable-unsafe-swiftshader", "disable-gpu-compositing", "disable-gpu-rasterization"},
			[]string{"disable-gpu"}, GPUModeCompatible},
		{"explicit off (probe)", BrowserConfig{Headless: true, GPU: GPUModeOff}, false,
			[]string{"disable-gpu"}, nil, GPUModeOff},
		// The documented upgrade: an operator asking for "off" still gets a
		// working Meet, because off cannot publish video.
		{"explicit off (Meet)", BrowserConfig{Headless: true, GPU: GPUModeOff}, true,
			[]string{"use-angle=swiftshader", "disable-gpu-compositing"}, []string{"disable-gpu"}, GPUModeCompatible},
		{"compatible", BrowserConfig{Headless: true, GPU: GPUModeCompatible}, true,
			[]string{"use-angle=swiftshader", "use-gl=angle", "enable-unsafe-swiftshader", "disable-gpu-compositing", "disable-gpu-rasterization"},
			[]string{"disable-gpu"}, GPUModeCompatible},
		{"swiftshader", BrowserConfig{Headless: true, GPU: GPUModeSwiftShader}, true,
			[]string{"use-angle=swiftshader", "use-gl=angle", "enable-unsafe-swiftshader"},
			[]string{"disable-gpu", "disable-gpu-compositing"}, GPUModeSwiftShader},
		{"auto headless (Meet)", BrowserConfig{Headless: true, GPU: GPUModeAuto}, true,
			[]string{"use-angle=swiftshader"}, []string{"disable-gpu"}, GPUModeCompatible},
		{"auto headless (probe)", BrowserConfig{Headless: true, GPU: GPUModeAuto}, false,
			nil, []string{"disable-gpu", "use-angle=swiftshader"}, GPUModeAuto},
		// Non-headless (Xvfb / real display): leave the compositor alone.
		{"windowed default", BrowserConfig{Headless: false}, false, nil, []string{"disable-gpu"}, GPUModeAuto},
		{"windowed Meet", BrowserConfig{Headless: false}, true, nil, []string{"disable-gpu"}, GPUModeAuto},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gpuMode(tc.cfg, tc.forMeet); got != tc.wantMode {
				t.Fatalf("gpuMode = %q, want %q", got, tc.wantMode)
			}
			flags := launchFlags(tc.cfg, MeetOptions{}, tc.forMeet)
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

// TestMeetNeverLaunchesGPUless is the narrow regression assertion for the
// production incident: no combination of browser.gpu and headlessness may send
// a meeting session into the GPU-less launch.
func TestMeetNeverLaunchesGPUless(t *testing.T) {
	values := []string{"", GPUModeOff, "disable", "disabled", "none", GPUModeAuto, "on", "hardware", "default",
		GPUModeCompatible, "meet", "webgl", GPUModeSwiftShader, "angle", "swangle", "bogus"}
	for _, headless := range []bool{true, false} {
		for _, v := range values {
			cfg := BrowserConfig{Headless: headless, GPU: v}
			if got := gpuMode(cfg, true); got == GPUModeOff {
				t.Fatalf("gpu=%q headless=%v: Meet session resolved to %q", v, headless, got)
			}
			if hasFlag(launchFlags(cfg, MeetOptions{}, true), "disable-gpu") {
				t.Fatalf("gpu=%q headless=%v: Meet launch carries --disable-gpu", v, headless)
			}
		}
	}
}

// TestLaunchFlagsNeverFakeCapture keeps the green-screen guard in place: the
// fake device/UI switches make getDisplayMedia return Chromium's test pattern.
func TestLaunchFlagsNeverFakeCapture(t *testing.T) {
	flags := launchFlags(BrowserConfig{Headless: true}, MeetOptions{PresentMode: "tab", CaptureSource: "TeleMeet Media"}, false)
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
	flags := launchFlags(BrowserConfig{Headless: true, ExtraArgs: []string{"window-size=640,480"}}, MeetOptions{}, false)
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
