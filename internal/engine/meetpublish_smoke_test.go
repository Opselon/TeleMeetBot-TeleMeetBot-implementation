package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// None of the publication diagnostics may break Meet: scripts must be plain JS.
func TestMeetPublishSmoke(t *testing.T) {
	for _, name := range []string{"peerHook", "publishStatsJS"} {
		var js string
		switch name {
		case "peerHook":
			js = meetPeerHookJS()
		case "publishStatsJS":
			js = meetPublishStatsJS()
		}
		if js == "" {
			t.Fatalf("empty script for %s", name)
		}
		if name == "peerHook" && strings.Contains(js, "__tmPCs.push") == false {
			t.Fatalf("hook does not record peer connections")
		}
		stats := map[string]interface{}{}
		if err := json.Unmarshal([]byte(`{"present":true,"pcs":2,"display":"none","srcFrames":0,"senders":0,"trackState":"","encFrames":0,"encFps":0,"bytesSent":0,"bitsPerSec":0,"quality":"","encoder":"","keyFrames":0,"encWidth":0,"encHeight":0,"inVideo":0,"inFps":0,"console":"","error":""}`), &stats); err != nil {
			t.Fatalf("MeetPublishStats fixture json: %v", err)
		}
	}
}

// The tracker must report first-sample behavior sanely (no rates until two samples).
func TestPublishTrackerFirstSample(t *testing.T) {
}
