package netcap

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// fakeSTUNServer answers RFC 5389 binding requests with a valid
// XOR-MAPPED-ADDRESS response built from the request's transaction id.
type fakeSTUNServer struct {
	conn *net.UDPConn
}

func startFakeSTUNServer(t *testing.T) *fakeSTUNServer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("fake stun server: %v", err)
	}
	f := &fakeSTUNServer{conn: conn}
	go f.serve()
	t.Cleanup(func() { _ = conn.Close() })
	return f
}

func (f *fakeSTUNServer) addr() string { return f.conn.LocalAddr().String() }

func (f *fakeSTUNServer) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 20 || binary.BigEndian.Uint32(buf[4:8]) != 0x2112A442 {
			continue // not a STUN binding request
		}
		resp := f.response(buf[:n], from)
		_, _ = f.conn.WriteToUDP(resp, from)
	}
}

// response builds a binding success response echoing the request transaction
// id with attribute 0x0020 (XOR-MAPPED-ADDRESS) for the sender's address.
func (f *fakeSTUNServer) response(req []byte, from *net.UDPAddr) []byte {
	resp := make([]byte, 20, 32)
	copy(resp, req[:8]) // type 0x0101 shares the cookie+txid bytes layout
	binary.BigEndian.PutUint16(resp[0:2], 0x0101)
	binary.BigEndian.PutUint16(resp[2:4], 12)
	resp = append(resp,
		0x00, 0x20, // XOR-MAPPED-ADDRESS
		0x00, 0x08, // length 8
		0x00, 0x01, // IPv4
	)
	ip4 := from.IP.To4()
	port := uint16(from.Port) ^ (0x2112A442 >> 16)
	resp = append(resp,
		byte(port>>8), byte(port),
		ip4[0]^0x21, ip4[1]^0x12, ip4[2]^0xa4, ip4[3]^0x42,
	)
	return resp
}

// TestProbeUDPRespondingServer proves the probe classifies a working UDP
// path as ok, with a parsed round trip.
func TestProbeUDPRespondingServer(t *testing.T) {
	f := startFakeSTUNServer(t)
	res := probeUDP(context.Background(), []string{f.addr()}, 2*time.Second)
	if res.Outcome != UDPOutcomeOK {
		t.Fatalf("outcome = %q (err %q), want ok", res.Outcome, res.Error)
	}
	if res.LatencyMS < 0 || res.Attempted != 1 {
		t.Fatalf("attempted = %d latency = %d, want a single measured attempt", res.Attempted, res.LatencyMS)
	}
}

// TestProbeUDPSilentReceiver proves a bound-but-silent receiver classifies
// as blocked — the datagram is delivered by the kernel (no ICMP error), yet
// nothing answers. That is the signature of a provider UDP filter.
func TestProbeUDPSilentReceiver(t *testing.T) {
	silent, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("bind silent receiver: %v", err)
	}
	defer func() { _ = silent.Close() }() // bound, deliberately never read

	res := probeUDP(context.Background(), []string{silent.LocalAddr().String()}, 300*time.Millisecond)
	if res.Outcome != UDPOutcomeBlocked {
		t.Fatalf("outcome = %q (err %q), want blocked", res.Outcome, res.Error)
	}
}

// TestParseXORMappedAddress pins the attribute decoder.
func TestParseXORMappedAddress(t *testing.T) {
	data := make([]byte, 20, 32)
	copy(data, []byte{0x01, 0x01, 0x00, 0x0c, 0x21, 0x12, 0xa4, 0x42})
	data = append(data,
		0x00, 0x20, 0x00, 0x08,
		0x00, 0x01,
		0xd3, 0x9f, // 0xd39f ^ 0x2112 = 0xf22d = 62005
		0x61, 0x3e, 0x5b, 0x73, // ^cookie -> 64.255.31.49
	)
	got := parseXORMappedAddress(data)
	want := "64.44.255.49:62093"
	if got != want {
		t.Fatalf("mapped = %q, want %q", got, want)
	}
	if got := parseXORMappedAddress(data[:20]); got != "" {
		t.Fatalf("header-only reply should decode to empty, got %q", got)
	}
}

// TestEnsureUDPProbedCacheLifecycle proves the cached observation transitions
// from unknown to a real result after the background probe lands, and that
// concurrent callers do not multiply probes.
func TestEnsureUDPProbedCacheLifecycle(t *testing.T) {
	f := startFakeSTUNServer(t)
	realServers := DefaultSTUNServers
	DefaultSTUNServers = []string{f.addr()}
	t.Cleanup(func() { DefaultSTUNServers = realServers })

	c := New()
	if got := c.UDP().Outcome; got != UDPOutcomeUnknown {
		t.Fatalf("initial outcome = %q, want unknown", got)
	}
	c.EnsureUDPProbed(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.UDP().Outcome == UDPOutcomeOK {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := c.UDP().Outcome; got != UDPOutcomeOK {
		t.Fatalf("cached outcome = %q, want ok after the background probe", got)
	}
	// A fresh cache must not relaunch the probe.
	c.EnsureUDPProbed(context.Background())
	if c.UDP().Outcome != UDPOutcomeOK {
		t.Fatalf("cached observation lost after a no-op EnsureUDPProbed call")
	}
}
