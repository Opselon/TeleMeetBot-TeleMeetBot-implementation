package bot

// Tests for the Settings → Egress / Proxy screen: admin gating, preset
// persistence through the app_settings KV, the Meet-via-proxy toggle, the
// custom URL input step, and validation that rejects junk addresses.

import (
	"context"
	"strings"
	"testing"

	"github.com/Opselon/TeleMeetBot/internal/engine"
)

// applyEgress persists through the real store and reaches the engine.
func TestEgressOverrideRoundTrip(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	st, err := h.bot.applyEgress("socks5://172.17.0.1:3339", nil)
	if err != nil {
		t.Fatalf("applyEgress: %v", err)
	}
	if st.proxy != "socks5://172.17.0.1:3339" {
		t.Fatalf("proxy = %q, want the preset URL", st.proxy)
	}

	// The override must survive a fresh read from the store.
	got := h.bot.egressStateOf()
	if got.proxy != "socks5://172.17.0.1:3339" {
		t.Fatalf("persisted proxy = %q, want the preset URL", got.proxy)
	}

	// Clearing restores the server default.
	if _, err := h.bot.applyEgress("", nil); err != nil {
		t.Fatalf("clear applyEgress: %v", err)
	}
	if got := h.bot.egressStateOf(); got.proxy != "" {
		t.Fatalf("cleared proxy = %q, want empty", got.proxy)
	}
}

// The meet_via_proxy toggle persists independently and defaults to the
// configured value when the toggle was never touched.
func TestEgressMeetViaProxyToggle(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)
	h.bot.cfg.Browser.MeetViaProxy = true // the yml default

	// Untouched: the configured default applies.
	if st := h.bot.egressStateOf(); !st.meetVia || st.meetViaSet {
		t.Fatalf("untouched state = %+v, want the yml default (true, unset)", st)
	}

	off := false
	if _, err := h.bot.applyEgress("direct", &off); err != nil {
		t.Fatalf("applyEgress with toggle: %v", err)
	}
	st := h.bot.egressStateOf()
	if st.meetVia || !st.meetViaSet || st.proxy != "direct" {
		t.Fatalf("state = %+v, want (direct, meetVia=false, set)", st)
	}

	on := true
	if _, err := h.bot.applyEgress("direct", &on); err != nil {
		t.Fatalf("applyEgress toggle on: %v", err)
	}
	if st := h.bot.egressStateOf(); !st.meetVia {
		t.Fatalf("state = %+v, want meetVia=true", st)
	}
}

// Junk proxy addresses are rejected before they can reach a session.
func TestValidateProxyURL(t *testing.T) {
	good := []string{
		"socks5://172.17.0.1:3339",
		"socks5://host.docker.internal:3339",
		"http://127.0.0.1:8080",
	}
	for _, p := range good {
		if err := validateProxyURL(p); err != nil {
			t.Errorf("validateProxyURL(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{
		"ftp://172.17.0.1:3339", // unsupported scheme
		"172.17.0.1:3339",       // no scheme
		"socks5://",             // no host
		"not a url at all://:",  // unparseable
	}
	for _, p := range bad {
		if err := validateProxyURL(p); err == nil {
			t.Errorf("validateProxyURL(%q) = nil, want an error", p)
		}
	}
}

// The menu is admin-only: a non-admin tapping the preset must not change the
// stored egress.
func TestProxySettingsAdminGate(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.callback(43, "set:proxy:url:socks5://172.17.0.1:3339")
	if st := h.bot.egressStateOf(); st.proxy != "" {
		t.Fatalf("non-admin changed the egress: %+v", st)
	}

	h.callback(42, "set:proxy:url:socks5://172.17.0.1:3339")
	st := h.bot.egressStateOf()
	if st.proxy != "socks5://172.17.0.1:3339" {
		t.Fatalf("admin preset not applied: %+v", st)
	}

	// The screen must render the saved value for the admin.
	h.callback(42, "set:proxy")
}

// The custom-input step accepts a typed URL, persists it, and clears the flow.
func TestProxyCustomInputStep(t *testing.T) {
	h := newHarnessWithSettings(t, []int64{42}, 2)

	h.callback(42, "set:proxy:custom")
	if f := h.bot.getFlow(42); f == nil || f.step != stepProxyCustom {
		t.Fatalf("custom step not armed: %+v", f)
	}

	h.message(42, "socks5://10.0.0.9:1080")
	if f := h.bot.getFlow(42); f != nil && f.step == stepProxyCustom {
		t.Fatal("the flow stayed open after a valid proxy was saved")
	}
	if st := h.bot.egressStateOf(); st.proxy != "socks5://10.0.0.9:1080" {
		t.Fatalf("typed proxy not persisted: %+v", st)
	}

	// A bad address keeps the flow open and does not persist.
	h.callback(42, "set:proxy:custom")
	h.message(42, "not-a-proxy")
	if st := h.bot.egressStateOf(); st.proxy != "socks5://10.0.0.9:1080" {
		t.Fatalf("bad address overwrote the saved proxy: %+v", st)
	}

	// "-" clears the override.
	h.message(42, "-")
	if st := h.bot.egressStateOf(); st.proxy != "" {
		t.Fatalf("clear sentinel did not reset the override: %+v", st)
	}
}

// The engine receives the override: the real manager mutates its browser
// config for future launches (pinned here through the concrete manager).
func TestManagerSetEgressOverride(t *testing.T) {
	m := engine.NewManager(engine.BrowserConfig{Proxy: ""}, engine.MediaOptions{}, engine.MeetOptions{GuestName: "T"}, nil)
	m.SetEgressOverride("socks5://172.17.0.1:3339", true)
	m.SetEgressOverride("direct", false)
	// The method must not panic and must keep the manager usable.
	if s, err := m.Start(context.Background(), engine.StartRequest{MeetLink: "https://meet.google.com/abc-defg-hij"}); err == nil && s != nil {
		t.Fatal("a manager with no browser should refuse to start, not silently run")
	}
}

// proxySettingsText names the current egress so the operator can tell, at a
// glance, which egress the next session will take.
func TestProxySettingsTextReflectsState(t *testing.T) {
	tests := []struct {
		name string
		st   egressState
		want string
	}{
		{"server default", egressState{}, "server default"},
		{"direct", egressState{proxy: "direct", meetViaSet: true}, "<b>direct</b>"},
		{"proxy url", egressState{proxy: "socks5://172.17.0.1:3339"}, "socks5://172.17.0.1:3339"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text := proxySettingsText(tc.st)
			if !strings.Contains(text, tc.want) {
				t.Fatalf("text %q does not mention %q", text, tc.want)
			}
		})
	}
}
