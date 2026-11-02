package media

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDirectDownloadOutlivesHTTPTimeout is the regression test for the
// 30-second download cliff. The HTTP client was built with
// Client.Timeout = opts.HTTPTimeout (default 30s). Client.Timeout covers the
// *entire response body*, so a direct media download slower than 30 seconds
// — a large file over a slow link, a throttled CDN — was always cancelled as
// a client timeout, no matter what download.timeout_seconds said. Nothing in
// production ever overrode HTTPTimeout, so the cliff was unconditional.
//
// The fix gives the whole fetch the download deadline (opts.Timeout, the same
// bound yt-dlp already runs under) and leaves the client without a body
// timeout. This test serves a body that takes longer than the old 30s
// ceiling would have allowed and asserts it still arrives, using a short
// client-side timeout for the negative control.
func TestDirectDownloadOutlivesHTTPTimeout(t *testing.T) {
	SetAllowLoopbackForTest(true)
	defer SetAllowLoopbackForTest(false)

	// 800ms of dribbled body: comfortably over the 200ms HTTPTimeout pinned
	// below, comfortably under the 10s download deadline.
	const dripEvery = 40 * time.Millisecond
	const drips = 20

	m, _ := testManager(t, time.Minute)
	m.opts.HTTPTimeout = 200 * time.Millisecond // the historical cliff
	m.opts.Timeout = 10 * time.Second           // the per-download bound
	m.opts.MaxBytes = 0                         // tiny ceiling off; size is fixed below
	m.opts.MaxBytes = 1024

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		for i := 0; i < drips; i++ {
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(dripEvery)
		}
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	u, err := ValidateURL("http://" + net.JoinHostPort(host, port) + "/slow.mp4")
	if err != nil {
		t.Fatalf("ValidateURL: %v", err)
	}

	start := time.Now()
	res, err := m.fetchDirect(context.Background(), u, "slow-dl")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("fetchDirect failed on a slow-but-healthy body after %s: %v", elapsed.Round(time.Millisecond), err)
	}
	if res.Bytes != drips {
		t.Fatalf("got %d bytes, want %d", res.Bytes, drips)
	}
	// The body must have taken longer than the old client-side ceiling: that
	// is the whole point — a transfer slower than HTTPTimeout still completes.
	if elapsed <= m.opts.HTTPTimeout {
		t.Fatalf("body completed in %s, expected it to outlast the %s HTTPTimeout",
			elapsed.Round(time.Millisecond), m.opts.HTTPTimeout)
	}
}

// TestDirectDownloadRespectsDownloadTimeout makes sure the new bound is a
// real deadline, not an unbounded one: a body that never finishes must die
// at the download deadline, not hang forever.
func TestDirectDownloadRespectsDownloadTimeout(t *testing.T) {
	SetAllowLoopbackForTest(true)
	defer SetAllowLoopbackForTest(false)

	m, _ := testManager(t, time.Minute)
	m.opts.Timeout = 300 * time.Millisecond
	m.opts.HTTPTimeout = 5 * time.Second // must NOT be the effective bound

	// A raw TCP listener stands in for a server that never finishes its
	// body: the deadline is under test, not the fixture's lifecycle, so the
	// handler accepts the connection, answers the request line and then
	// holds the socket open. Closing the listener afterwards drops the
	// parked connection without the graceful-drain wait that makes
	// httptest.Server.Close block on an active handler.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the request (best-effort) then emit headers that promise a
		// body that never arrives.
		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: video/mp4\r\nContent-Length: 1024\r\nConnection: close\r\n\r\n"))
		_, _ = conn.Write([]byte("x"))
		<-time.After(30 * time.Second)
	}()

	addr := ln.Addr().String()
	u, err := ValidateURL("http://" + addr + "/stuck.mp4")
	if err != nil {
		t.Fatalf("ValidateURL: %v", err)
	}

	start := time.Now()
	_, err = m.fetchDirect(context.Background(), u, "stuck-dl")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("fetchDirect on a never-finishing body succeeded after %s", elapsed.Round(time.Millisecond))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("download deadline not honoured: took %s, expected ~%s", elapsed.Round(time.Millisecond), m.opts.Timeout)
	}
}
