package engine

// Tests for out-of-meeting detection: the decision table pins every branch
// of the recovery path so a kicked-out bot fails fast instead of looping.

import (
	"strings"
	"testing"
)

func TestDecideMeetingExit(t *testing.T) {
	cases := []struct {
		name    string
		st      *leftMeetingState
		probeOK bool
		wantErr bool
		wantMsg string
	}{
		{
			name: "probe failed: normal recovery applies",
			st:   nil, probeOK: false,
			wantErr: false,
		},
		{
			name: "genuinely in a meeting: keep recovering",
			st:   &leftMeetingState{InMeeting: true, URL: "https://meet.google.com/abc-defg-hij"}, probeOK: true,
			wantErr: false,
		},
		{
			name: "kicked to marketing page: fail fast",
			st:   &leftMeetingState{Marketing: true, Landing: true, URL: "https://meet.google.com/"}, probeOK: true,
			wantErr: true, wantMsg: "marketing/landing page",
		},
		{
			name: "left the meeting URL: fail fast",
			st:   &leftMeetingState{Landing: true, URL: "https://meet.google.com/landing"}, probeOK: true,
			wantErr: true, wantMsg: "left the meeting URL",
		},
		{
			name: "probe returned unknown page: fail fast",
			st:   &leftMeetingState{URL: "https://meet.google.com/xyz"}, probeOK: true,
			wantErr: true, wantMsg: "no longer shows a meeting",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := decideMeetingExit(tc.st, tc.probeOK)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error %q missing %q", err, tc.wantMsg)
			}
		})
	}
}

// TestLeftMeetingJSContract pins the probe's JSON shape so the Go decode and
// the JS producer cannot drift apart silently.
func TestLeftMeetingJSContract(t *testing.T) {
	for _, field := range []string{"inMeeting", "marketing", "landing", "url"} {
		if !strings.Contains(leftMeetingJS, field+":") {
			t.Fatalf("leftMeetingJS no longer emits %q", field)
		}
	}
	if !strings.Contains(leftMeetingJS, "meet\\.google\\.com\\/([a-z]{3}-[a-z]{4}-[a-z]{3})") {
		t.Fatal("probe lost the meeting-code URL check")
	}
	if !strings.Contains(leftMeetingJS, "Open the (Sign Up|Continue) page") {
		t.Fatal("probe lost the landing-page marker check")
	}
}

// TestSessionFailsFastWithHonestReason drives the fail-fast path through a
// session whose media tab is a fake: the error message must name the real
// cause, not the old misleading "screen share was dropped".
func TestSessionFailsFastWithHonestReason(t *testing.T) {
	err := decideMeetingExit(&leftMeetingState{Marketing: true, Landing: true, URL: "https://meet.google.com/"}, true)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "screen share was dropped") {
		t.Fatalf("error still uses the misleading old message: %q", err)
	}
	if !strings.Contains(err.Error(), "no longer in the meeting") {
		t.Fatalf("error missing the honest reason: %q", err)
	}
}
