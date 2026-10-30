package engine

import (
	"testing"
	"time"
)

func TestStageStepOf(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"[1/6] Downloading media: 45%", 1},
		{"[6/6] Streaming live", 6},
		{"loading meet", 0},
		{"[9/6] bogus", 0},
		{"", 0},
		{"[3/7] wrong total", 0},
	}
	for _, tc := range cases {
		if got := stageStepOf(tc.in); got != tc.want {
			t.Errorf("stageStepOf(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func newTrackedSession() *Session {
	return &Session{}
}

func TestTrackStageAdvancesAndTimes(t *testing.T) {
	s := newTrackedSession()
	s.mu.Lock()
	s.trackStageLocked(1)
	time.Sleep(5 * time.Millisecond)
	s.trackStageLocked(2) // closes 1
	s.trackStageLocked(2) // idempotent
	s.mu.Unlock()

	tims := s.StageTimings()
	if len(tims) != 2 {
		t.Fatalf("timings = %+v", tims)
	}
	if tims[0].Name != "download" || tims[0].Active || tims[0].Seconds <= 0 {
		t.Errorf("stage1 = %+v", tims[0])
	}
	if tims[1].Name != "media" || !tims[1].Active || tims[1].Seconds < 0 {
		t.Errorf("stage2 = %+v", tims[1])
	}
}

func TestTrackStageIgnoresUntagged(t *testing.T) {
	s := newTrackedSession()
	s.mu.Lock()
	s.trackStageLocked(3)
	s.trackStageLocked(0)
	s.mu.Unlock()
	if s.StageTimings()[0].Step != 3 {
		t.Fatalf("untagged detail must not change the step")
	}
}

func TestStageTimingString(t *testing.T) {
	got := stageTimingString([]StageTiming{
		{Step: 1, Name: "download", Seconds: 8.25},
		{Step: 2, Name: "media", Seconds: 1.02},
		{Step: 4, Name: "join", Seconds: 12.9, Active: true},
	})
	want := "download: 8.3s · media: 1s · join: 12.9s*"
	if got != want {
		t.Fatalf("stageTimingString = %q, want %q", got, want)
	}
}
