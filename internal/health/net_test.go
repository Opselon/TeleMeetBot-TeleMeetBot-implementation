package health

import (
	"strings"
	"testing"
	"time"
)

func TestFormatRate(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0 B/s"},
		{512, "512 B/s"},
		{1024 * 1024 * 12.5, "12.5 MiB/s"},
		{1024 * 1024 * 1024 * 1.5, "1.5 GiB/s"},
	}
	for _, tc := range cases {
		if got := formatRate(tc.in); got != tc.want {
			t.Errorf("formatRate(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatTotalGiB(t *testing.T) {
	if got := formatTotalGiB(3 * 1 << 30); got != "3.0G" {
		t.Errorf("formatTotalGiB = %q", got)
	}
	if got := formatTotalGiB(500 << 20); got != "500M" {
		t.Errorf("formatTotalGiB MiB = %q", got)
	}
}

func TestFormatNetLine(t *testing.T) {
	line := formatNetLine(NetStats{
		Available: true, Scope: "container",
		RxRateBps: 1024 * 1024 * 12.5, TxRateBps: 1024 * 512,
		RxTotal: 3 << 30, TxTotal: 500 << 20,
	})
	for _, want := range []string{"Net (container)", "⬇️ 12.5 MiB/s", "⬆️ 512.0 KiB/s", "Σ 3.0G/500M"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
	if formatNetLine(NetStats{}) != "" {
		t.Error("unavailable net stats must render nothing")
	}
}

func TestNetSamplerRates(t *testing.T) {
	origDev, origScope := netDevFn, netScopeFn
	defer func() { netDevFn, netScopeFn = origDev, origScope }()

	fake := struct{ rx, tx uint64 }{0, 0}
	netDevFn = func() (uint64, uint64, bool) { return fake.rx, fake.tx, true }
	netScopeFn = func() string { return "container" }

	var n netSampler
	t0 := time.Unix(1000, 0)

	first := n.sample(t0)
	if first.RxRateBps != 0 || first.TxRateBps != 0 {
		t.Fatalf("first sample = %+v, want zero rates", first)
	}

	// +50 MiB down / +10 MiB up over exactly 2 seconds.
	fake.rx = 50 << 20
	fake.tx = 10 << 20
	second := n.sample(t0.Add(2 * time.Second))
	if second.RxRateBps != 50<<20/2 || second.TxRateBps != 10<<20/2 {
		t.Fatalf("rates = %+v", second)
	}
	if second.Scope != "container" || second.RxTotal != 50<<20 {
		t.Fatalf("totals/scope = %+v", second)
	}

	// Counter reset (container restart): rates clamp to zero, totals follow.
	fake.rx = 1 << 20
	fake.tx = 0
	third := n.sample(t0.Add(4 * time.Second))
	if third.RxRateBps != 0 || third.TxRateBps != 0 {
		t.Fatalf("reset must not produce negative rates: %+v", third)
	}
}

func TestNetSamplerCache(t *testing.T) {
	origDev := netDevFn
	defer func() { netDevFn = origDev }()
	calls := 0
	netDevFn = func() (uint64, uint64, bool) { calls++; return 1, 2, true }

	var n netSampler
	t0 := time.Unix(1000, 0)
	n.sample(t0)
	n.sample(t0.Add(100 * time.Millisecond)) // inside 1.5s cache
	if calls != 1 {
		t.Fatalf("cache missed: calls = %d", calls)
	}
	n.sample(t0.Add(2 * time.Second))
	if calls != 2 {
		t.Fatalf("cache never expired: calls = %d", calls)
	}
}

func TestReadNetDevTotalsParsesProc(t *testing.T) {
	// Live environment: /proc/self/net/dev exists on Linux CI and locally.
	rx, tx, ok := readNetDevTotals()
	if !ok {
		t.Skip("no /proc/self/net/dev")
	}
	if rx == 0 && tx == 0 {
		t.Log("warning: counters are zero (fresh namespace?)")
	}
}
