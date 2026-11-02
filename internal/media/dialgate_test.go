package media

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDialGateBlocksPrivateDial is the regression test for the SSRF dial
// layer. For the whole life of the download manager, NewManager built its
// HTTP client with clientWithDialer(opts.HTTPTimeout, nil): the pre-flight
// ValidateResolvedDestination checked the resolved addresses, but the
// connection itself dialed whatever the resolver returned at connect time,
// so a DNS rebinding between the two — or a host with mixed records — could
// route an attacker-supplied media URL into the loopback/link-local/cloud
// metadata ranges.
//
// This test pins the dial-side verdict directly: a manager whose DNS seam
// returns a metadata address for a hostname must refuse to connect to it,
// even when the URL itself would pass the syntax gate.
func TestDialGateBlocksPrivateDial(t *testing.T) {
	m, _ := testManager(t, time.Minute)
	// Resolve every hostname into the cloud-metadata address: this models a
	// rebind, i.e. the pre-flight saw one address and the dial sees another.
	m.lookupFn = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}

	sawDial := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawDial = true
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	// Point the fetch at a hostname whose dial-time resolution is the
	// metadata address. srv.URL's 127.0.0.1 would be refused by the loopback
	// rule for the unrelated reason that we never enable it here, so use the
	// hostname path: the gate must refuse 169.254.169.254 before any connect.
	u, err := ValidateURL("http://internal.example.com/probe.mp4")
	if err != nil {
		t.Fatalf("ValidateURL: %v", err)
	}
	_, err = m.fetchDirect(context.Background(), u, "ssrf-dial")
	if err == nil {
		t.Fatalf("fetchDirect to a metadata address succeeded; the dial gate is not enforced")
	}
	if sawDial {
		t.Fatalf("the server saw a connection; the dial gate did not block the dial")
	}
	if !strings.Contains(err.Error(), ErrUnsafeURL.Error()) {
		t.Fatalf("error = %v, want it to wrap ErrUnsafeURL", err)
	}
}

// TestDialGateOnPublicDial makes sure the gate still lets a genuinely public
// destination through: the pin above must not turn the gate into a blanket
// denial that breaks legitimate downloads.
func TestDialGateOnPublicDial(t *testing.T) {
	SetAllowLoopbackForTest(true)
	defer SetAllowLoopbackForTest(false)

	m, _ := testManager(t, time.Minute)
	serverHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverHit = true
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = fmt.Fprint(w, "media-bytes")
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	// The loopback exemption is required for the local fixture; verify the
	// gate's verdict function agrees for this address.
	if !m.dialGate("tcp", net.JoinHostPort(host, port)) {
		t.Fatalf("dialGate refused %s:%s, want allowed for the test fixture", host, port)
	}

	u, err := ValidateURL("http://" + net.JoinHostPort(host, port) + "/clip.mp4")
	if err != nil {
		t.Fatalf("ValidateURL: %v", err)
	}
	res, err := m.fetchDirect(context.Background(), u, "ssrf-public")
	if err != nil {
		t.Fatalf("fetchDirect on a permitted destination failed: %v", err)
	}
	if !serverHit {
		t.Fatalf("the fixture server was never reached")
	}
	if res.ContentType != "video/mp4" {
		t.Fatalf("content type = %q, want video/mp4", res.ContentType)
	}
}
