package engine

import (
	"strings"
	"testing"
)

// TestMediaProbeScriptShape guards the rod contract: page scripts are function
// form, never IIFE-wrapped (an IIFE breaks rod's Eval).
func TestMediaProbeScriptShape(t *testing.T) {
	for name, js := range map[string]string{
		"pixelSampleJS":   pixelSampleJS(),
		"codecSupportJS":  codecSupportJS(),
		"playerProblemJS": playerProblemJS(),
	} {
		if !strings.HasPrefix(js, "() => {") {
			t.Errorf("%s must be function form for rod Eval", name)
		}
		if strings.HasPrefix(js, "(() =>") {
			t.Errorf("%s must not be IIFE-wrapped", name)
		}
		if strings.Contains(js, "})()") {
			t.Errorf("%s must not self-invoke", name)
		}
	}
}

// TestClassifyDecodeFailureFrames pins the pixel signature the classifier must
// catch: green means the compositor is running but the codec cannot decode.
func TestClassifyDecodeFailureFrames(t *testing.T) {
	green := MediaProbeSample{Current: 0.32, Duration: 212, MeanR: 3, MeanG: 251, MeanB: 2, Painted: true}
	black := MediaProbeSample{Current: 0, Painted: true}
	healthy := MediaProbeSample{Current: 4.7, Duration: 212, MeanR: 128, MeanG: 96, MeanB: 140, Painted: true}

	rep := MediaProbeReport{}
	classifyProbe(&rep, []MediaProbeSample{green, green, green})
	if !rep.GreenFrames {
		t.Fatalf("green frames not detected: %+v", rep)
	}
	if rep.Verdict == "" {
		t.Fatal("verdict empty")
	}

	rep = MediaProbeReport{}
	classifyProbe(&rep, []MediaProbeSample{black, black, black})
	if !rep.BlackFrames || rep.GreenFrames {
		t.Fatalf("black frames not detected (or green false-positive): %+v", rep)
	}

	rep = MediaProbeReport{}
	classifyProbe(&rep, []MediaProbeSample{healthy, healthy, healthy})
	if rep.GreenFrames || rep.BlackFrames || !rep.Progressed || !rep.Painted {
		t.Fatalf("healthy playback misclassified: %+v", rep)
	}
}

// TestClassifyStalledPlaybackReports keeps "frozen at position" distinct from
// hardware/render complaints.
func TestClassifyStalledPlaybackReports(t *testing.T) {
	rep := MediaProbeReport{}
	classifyProbe(&rep, []MediaProbeSample{
		{Current: 0.1, Painted: true, MeanR: 40, MeanG: 40, MeanB: 40},
		{Current: 0.1, Painted: true, MeanR: 40, MeanG: 40, MeanB: 40},
		{Current: 0.1, Painted: true, MeanR: 40, MeanG: 40, MeanB: 40},
	})
	if rep.Progressed {
		t.Fatalf("stalled playback classified as progressed: %+v", rep)
	}
	if !strings.Contains(rep.Verdict, "not advancing") {
		t.Fatalf("verdict = %q, want a frozen-position explanation", rep.Verdict)
	}
}
