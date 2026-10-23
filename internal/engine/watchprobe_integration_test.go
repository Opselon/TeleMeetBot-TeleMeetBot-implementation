package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestWatchMeetDetectsPresentingWebRTC verifies the remote-viewer tool:
// it navigates to a meeting, waits for the call, and detects inbound presentation.
func TestWatchMeetDetectsPresentingWebRTC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<html><head><meta charset="utf-8"><title>Meet Viewer Test</title></head>
<body>
  <div id="prejoin">
    <input placeholder="Your name" value="">
    <button aria-label="Join now" onclick="join()">Join now</button>
  </div>
  <div id="meeting" style="display:none">
    <button aria-label="Leave call">Leave</button>
    <div id="presenting-banner">Someone is presenting</div>
    <canvas id="c" width="320" height="180" style="display:block;margin:auto"></canvas>
  </div>
<script>
  let t = 0;
  function join() {
    document.getElementById('prejoin').style.display = 'none';
    document.getElementById('meeting').style.display = 'block';
    const c = document.getElementById('c');
    const ctx = c.getContext('2d');
    function draw() {
      t += 0.05;
      ctx.fillStyle = 'hsl(' + ((t*60)%360) + ', 70%, 50%)';
      ctx.fillRect(0, 0, 320, 180);
      ctx.fillStyle = '#fff';
      ctx.font = '24px sans-serif';
      ctx.fillText('Frame ' + Math.floor(t*20), 40, 90);
      requestAnimationFrame(draw);
    }
    draw();
  }
</script>
</body></html>`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rep, err := WatchMeet(ctx, BrowserConfig{Executable: browserBin(t), Headless: true}, MeetOptions{}, WatchOptions{
		MeetURL: srv.URL,
		Name:    "Viewer",
		Seconds: 5,
	}, testLogger{t})
	if err != nil {
		t.Fatalf("WatchMeet: %v", err)
	}
	t.Logf("WatchReport: %+v", rep)
	if !rep.Joined {
		t.Fatal("viewer never joined")
	}
	if !rep.Presenting {
		t.Fatal("viewer did not detect presentation banner")
	}
	if !rep.Moves() {
		t.Fatalf("viewer reported no moving video: center motion=%.2f mean=%.1f", rep.CenterMove, rep.CenterMean)
	}
}
