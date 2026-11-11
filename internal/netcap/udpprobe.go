package netcap

// UDP egress probe — answers the question the HTTP probe cannot: can this
// host send and receive real UDP datagrams, which is what Google Meet's
// WebRTC media transport needs? Many datacenter networks (this deployment's
// included) pass every TCP check while silently blackholing outbound UDP,
// which forces all meeting media through TURN-over-TCP — or kills the media
// path entirely when Google's TURN edges are unreachable too. A probe that
// only fetches YouTube over HTTPS reports "ok" on exactly such a network,
// which is why this probe exists alongside it.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// UDP outcome values, following the HTTP probe's outcome vocabulary so the
// /healthz surfaces read uniformly.
const (
	UDPOutcomeUnknown = "unknown"
	UDPOutcomeOK      = "ok"
	UDPOutcomeBlocked = "blocked" // no reply: datagrams left but nothing answered (provider filter / silent UDP drop)
	UDPOutcomeError   = "error"   // local failure: no route, resolver, or socket error
)

// udpTTL bounds how long one UDP observation stays fresh, mirroring the HTTP
// probe's cache window; udpProbeCooldown keeps a fully filtered network from
// attracting a fresh probe on every /healthz read.
const (
	udpTTL           = 3 * time.Minute
	udpProbeCooldown = 30 * time.Second
)

// DefaultSTUNServers is the probe target list. Google's public STUN service
// is the same endpoint family Meet's own ICE config offers, so a reply here
// is meaningful evidence for WebRTC viability.
var DefaultSTUNServers = []string{"stun.l.google.com:19302", "stun1.l.google.com:19302"}

// UDPResult is one UDP egress observation. It deliberately does not carry the
// STUN-mapped address: /healthz is publicly readable, and the NAT mapping
// would publish this host's public transport address to anyone who can read
// the health endpoint.
type UDPResult struct {
	Outcome   string `json:"outcome"`
	Attempted int    `json:"attempted"`
	// LatencyMS is the first successful round trip, in milliseconds.
	LatencyMS int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"-"`
}

// probeSTUN sends one RFC 5389 binding request and returns the reply's
// XOR-MAPPED-ADDRESS (for tests and diagnostics only — never surfaced via
// HTTP) and the round-trip latency.
func probeSTUN(conn *net.UDPConn, timeout time.Duration) (latencyMS int64, mapped string, err error) {
	// RFC 5389 §6: 20-byte header — type 0x0001, length 0, magic cookie,
	// 96-bit transaction id. The magic cookie is required so the server
	// returns a XOR-MAPPED-ADDRESS we can decode.
	txid := make([]byte, 12)
	if _, err := rand.Read(txid); err != nil {
		return 0, "", fmt.Errorf("transaction id: %w", err)
	}
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:2], 0x0001)
	binary.BigEndian.PutUint32(req[4:8], 0x2112A442)
	copy(req[8:20], txid)

	if _, err := conn.Write(req); err != nil {
		return 0, "", fmt.Errorf("send: %w", err)
	}
	start := time.Now()
	buf := make([]byte, 1024)
	if err := conn.SetReadDeadline(start.Add(timeout)); err != nil {
		return 0, "", err
	}
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return 0, "", err
	}
	latencyMS = time.Since(start).Milliseconds()
	if n < 20 || binary.BigEndian.Uint32(buf[4:8]) != 0x2112A442 {
		return latencyMS, "", fmt.Errorf("not a STUN reply (%d bytes)", n)
	}
	return latencyMS, parseXORMappedAddress(buf[:n]), nil
}

// parseXORMappedAddress decodes attribute 0x0020 (XOR-MAPPED-ADDRESS) for an
// IPv4 mapping. Empty string when absent or malformed.
func parseXORMappedAddress(data []byte) string {
	i := 20
	for i+4 <= len(data) {
		at, al := binary.BigEndian.Uint16(data[i:i+2]), binary.BigEndian.Uint16(data[i+2:i+4])
		if at == 0x0020 && al >= 8 && i+4+8 <= len(data) && binary.BigEndian.Uint16(data[i+4:i+6]) == 0x01 {
			port := binary.BigEndian.Uint16(data[i+6:i+8]) ^ (0x2112A442 >> 16)
			ip := binary.BigEndian.Uint32(data[i+8:i+12]) ^ 0x2112A442
			return fmt.Sprintf("%d.%d.%d.%d:%d", ip>>24&0xff, ip>>16&0xff, ip>>8&0xff, ip&0xff, port)
		}
		i += 4 + int(al)
	}
	return ""
}

// probeUDP walks the server list with two attempts each and classifies the
// result. A silent timeout is "blocked" — datagrams left but nothing answered,
// the signature of a provider-level UDP filter. "error" is reserved for local
// failures (no route, resolver, socket), and only when every attempt failed
// that way.
func probeUDP(ctx context.Context, servers []string, timeout time.Duration) UDPResult {
	out := UDPResult{Outcome: UDPOutcomeBlocked, CheckedAt: time.Now()}
	localErrors := 0
	attempts := 0
	for _, server := range servers {
		if ctx.Err() != nil {
			out.Error = ctx.Err().Error()
			out.Outcome = UDPOutcomeError
			return out
		}
		raddr, err := net.ResolveUDPAddr("udp4", server)
		if err != nil {
			localErrors++
			out.Error = err.Error()
			continue
		}
		for try := 0; try < 2; try++ {
			if ctx.Err() != nil {
				out.Error = ctx.Err().Error()
				out.Outcome = UDPOutcomeError
				return out
			}
			attempts++
			conn, err := net.DialUDP("udp4", nil, raddr)
			if err != nil {
				localErrors++
				out.Error = err.Error()
				break
			}
			latency, mapped, perr := probeSTUN(conn, timeout)
			_ = conn.Close()
			if perr == nil {
				out.Outcome = UDPOutcomeOK
				out.LatencyMS = latency
				out.Attempted = attempts
				_ = mapped // diagnostics only; never surfaced via HTTP
				return out
			}
			if !isTimeoutErr(perr) {
				localErrors++
				out.Error = perr.Error()
			}
		}
	}
	out.Attempted = attempts
	if attempts == 0 || localErrors == attempts {
		out.Outcome = UDPOutcomeError
	}
	return out
}

// isTimeoutErr reports whether err is (or wraps) a deadline/timeout failure —
// the "sent but nothing answered" signature that classifies as blocked.
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

// EnsureUDPProbed launches a background UDP probe when the cached observation
// is stale. It never blocks the caller: the observation lands in the cache
// and the next Summary read picks it up. The cooldown keeps a fully filtered
// network from attracting a fresh probe on every /healthz read.
func (c *Capability) EnsureUDPProbed(ctx context.Context) {
	c.udpMu.Lock()
	stale := time.Since(c.udp.lastTry) >= udpTTL
	running := c.udp.running
	cooldownActive := c.udp.result.Outcome == UDPOutcomeBlocked && time.Since(c.udp.lastTry) < udpProbeCooldown
	if stale && !running && !cooldownActive {
		c.udp.running = true
	}
	c.udpMu.Unlock()
	if !stale || running || cooldownActive {
		return
	}
	go func() {
		res := probeUDP(ctx, DefaultSTUNServers, 3*time.Second)
		c.udpMu.Lock()
		c.udp.result = res
		c.udp.lastTry = time.Now()
		c.udp.running = false
		c.udpMu.Unlock()
	}()
}

// UDP returns the cached UDP egress observation. A never-probed capability
// reports an explicit unknown outcome instead of the zero value.
func (c *Capability) UDP() UDPResult {
	c.udpMu.Lock()
	defer c.udpMu.Unlock()
	if c.udp.result.Outcome == "" {
		r := c.udp.result
		r.Outcome = UDPOutcomeUnknown
		return r
	}
	return c.udp.result
}
