package engine

import "testing"

// TestPublishProblemClass pins the recovery split: only problems Meet's own
// share-drops cause (missing capture stream, no video sender) count as
// share-dropped and may cost a re-present; media-side stalls belong to the
// media recovery path and must never trigger a Share Screen click.
func TestPublishProblemClass(t *testing.T) {
	cases := []struct {
		name    string
		problem string
		want    string
	}{
		{"healthy", "", ""},
		{"no capture stream", "Meet reports a live presentation but its capture stream is missing (getDisplayMedia was never granted)", "share-dropped"},
		{"no sender", "Meet has no video sender for the presentation: participants get no video track", "share-dropped"},
		{"frozen source", "Meet's capture track stopped delivering frames (0.0/s): the shared surface is frozen", "media-stalled"},
		{"dead encoder", "Meet's video encoder produced no frames (encoder=\"\" quality=\"\"): participants see a still/black share", "media-stalled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := publishProblemClass(tc.problem); got != tc.want {
				t.Fatalf("publishProblemClass(%q) = %q, want %q", tc.problem, got, tc.want)
			}
		})
	}
}
