package engine

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Stream-pipeline stage tracking.
//
// A session's run() walks six pipeline steps, each tagged "[N/6]" in the
// state detail lines that already exist. The tracker reuses those tags so
// every step gets a start time and duration with zero call-site churn —
// the Telegram card, the web dashboard and support dumps can then show
// WHERE time went ("join took 22s — the host gate was slow") instead of a
// single opaque state string.

// stageCount is the number of pipeline steps in the stream flow.
const stageCount = 6

// stageNames is the machine-readable name of each pipeline step (index 1..6).
var stageNames = [stageCount + 1]string{
	"",
	"download", // [1/6] fetch media to disk (Download & Stream modes)
	"media",    // [2/6] open and prepare the media tab
	"meet",     // [3/6] open the Google Meet tab
	"join",     // [4/6] prejoin + admission gate
	"present",  // [5/6] start presenting the tab
	"play",     // [6/6] unmute + playback (stream live)
}

// stageStepOf extracts the pipeline step from a "[N/6] ..." detail line,
// returning 0 when the detail carries no step tag.
func stageStepOf(detail string) int {
	if len(detail) < 5 || detail[0] != '[' || detail[2] != '/' || detail[3] != '6' || detail[4] != ']' {
		return 0
	}
	if c := detail[1]; c >= '1' && c <= '0'+stageCount {
		return int(c - '0')
	}
	return 0
}

// StageTiming is one pipeline step as shown on cards and dashboards.
type StageTiming struct {
	Step int    `json:"step"`
	Name string `json:"name"`
	// Seconds is the completed duration; -1 while the step is still active.
	Seconds float64 `json:"seconds"`
	// Active marks the step currently running.
	Active bool `json:"active,omitempty"`
}

// trackStageLocked advances the stage tracker to step. The caller must hold
// s.mu (setState/setDetail do). Passing 0 (an untagged detail) is a no-op:
// untagged details never end a stage, they only annotate the current one.
func (s *Session) trackStageLocked(step int) {
	if step == 0 || step == s.stageStep || step > stageCount {
		return
	}
	now := time.Now()
	s.closeStageLocked(now)
	s.stageStep = step
	s.stageStarted = now
}

// closeStageLocked records the elapsed time of the active stage, if any.
func (s *Session) closeStageLocked(now time.Time) {
	if s.stageStep == 0 || s.stageStarted.IsZero() {
		return
	}
	elapsed := now.Sub(s.stageStarted).Seconds()
	if prev := s.stageTimes[s.stageStep]; prev == 0 || elapsed > prev {
		// Keep the longest observation: repeated details within one step
		// (progress ticks) must not shrink a recorded duration.
		s.stageTimes[s.stageStep] = elapsed
	}
	s.stageStarted = time.Time{}
}

// StageTimings snapshots the pipeline timeline: completed steps carry their
// duration, the active step carries -1 with Active set, unstarted steps are
// omitted. Terminal sessions report every reached step as completed.
func (s *Session) StageTimings() []StageTiming {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stageStep == 0 && s.stageTimes == ([stageCount + 1]float64{}) {
		return nil
	}
	now := time.Now()
	out := make([]StageTiming, 0, stageCount)
	for step := 1; step <= s.stageStep; step++ {
		active := step == s.stageStep && !s.stageStarted.IsZero()
		sec := s.stageTimes[step]
		if !active && sec == 0 {
			// The step was never reached (tests can jump the tracker ahead);
			// it must not appear as a timeline entry.
			continue
		}
		st := StageTiming{Step: step, Name: stageNames[step], Seconds: sec}
		if active {
			st.Active = true
			// Report live elapsed too: cards show "join: 12s…" while waiting.
			st.Seconds = now.Sub(s.stageStarted).Seconds()
		}
		out = append(out, st)
	}
	return out
}

// stageTimingString renders the timeline as one compact line for logs and
// cards: "download 8.2s · meet 1.1s · join 12.4s*" (* = active).
func stageTimingString(timings []StageTiming) string {
	if len(timings) == 0 {
		return ""
	}
	var b strings.Builder
	for i, st := range timings {
		if i > 0 {
			b.WriteString(" · ")
		}
		if st.Active {
			b.WriteString(stageNames[st.Step] + ": " + formatSecs(st.Seconds) + "*")
		} else {
			b.WriteString(stageNames[st.Step] + ": " + formatSecs(st.Seconds))
		}
	}
	return b.String()
}

func formatSecs(sec float64) string {
	switch {
	case sec < 0:
		return "?"
	case sec < 100:
		return trimFloat(math.Round(sec*10)/10, 1) + "s"
	default:
		return trimFloat(math.Round(sec), 0) + "s"
	}
}

func trimFloat(f float64, prec int) string {
	s := strconv.FormatFloat(f, 'f', prec, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}
