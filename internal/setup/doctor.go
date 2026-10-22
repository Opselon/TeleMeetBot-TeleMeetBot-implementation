// Package setup contains the easy-setup wizard and the doctor diagnostics.
package setup

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/bot"
	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/health"
)

// Check is one diagnostic result.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Report is the doctor result.
type Report struct {
	Checks []Check
}

// OK reports whether every check passed.
func (r Report) OK() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// DoctorOptions tunes the diagnostics.
type DoctorOptions struct {
	// CaptureTest runs a real getDisplayMedia probe (launches Chromium).
	CaptureTest bool
	// SkipNetwork skips outbound connectivity checks.
	SkipNetwork bool
	// Log is the engine logger.
	Log engine.Logger
	// Validate overrides the Telegram token validation (tests).
	Validate func(string) (string, error)
}

// Doctor runs all environment checks for a config.
func Doctor(cfg *config.Config, cfgPath string, opts DoctorOptions) Report {
	r := Report{}

	// 1. Config
	if cfg != nil {
		if err := cfg.Validate(); err != nil {
			r.Checks = append(r.Checks, Check{"config", false, err.Error()})
		} else {
			r.Checks = append(r.Checks, Check{"config", true, fmt.Sprintf("valid (%s)", cfgPath)})
		}
	} else {
		r.Checks = append(r.Checks, Check{"config", false, "not loaded"})
	}

	// 2. Token
	validate := opts.Validate
	if validate == nil {
		validate = validateToken
	}
	if cfg != nil && cfg.Telegram.Token != "" {
		uname, err := validate(cfg.Telegram.Token)
		if err != nil {
			r.Checks = append(r.Checks, Check{"telegram token", false, err.Error()})
		} else {
			r.Checks = append(r.Checks, Check{"telegram token", true, "@" + uname})
		}
	} else {
		r.Checks = append(r.Checks, Check{"telegram token", false, "missing"})
	}

	// 3. Chromium
	browserCfg, _, meetCfg := engine.FromConfig(cfg)
	bin, err := engine.ResolveBrowser(browserCfg, opts.Log)
	if err != nil {
		r.Checks = append(r.Checks, Check{"chromium", false, err.Error()})
	} else {
		version := chromiumVersion(bin)
		r.Checks = append(r.Checks, Check{"chromium", true, strings.TrimSpace(bin + " " + version)})
	}

	// 4. Disk
	if err := os.MkdirAll(cfg.Paths.DataDir, 0o700); err != nil {
		r.Checks = append(r.Checks, Check{"disk", false, fmt.Sprintf("cannot create data dir: %v", err)})
	} else {
		freeGB, totalGB := health.DiskUsage(cfg.Paths.DataDir)
		if freeGB >= 1.5 {
			r.Checks = append(r.Checks, Check{"disk", true, fmt.Sprintf("%.1f GB free of %.1f GB", freeGB, totalGB)})
		} else {
			r.Checks = append(r.Checks, Check{"disk", false, fmt.Sprintf("only %.1f GB free (need ≥ 1.5 GB)", freeGB)})
		}
	}

	// 5. Health port
	if err := portFree(cfg.Server.HealthAddr); err != nil {
		r.Checks = append(r.Checks, Check{"health port", false, err.Error()})
	} else {
		r.Checks = append(r.Checks, Check{"health port", true, cfg.Server.HealthAddr + " is free"})
	}

	// 6. Network
	if opts.SkipNetwork {
		r.Checks = append(r.Checks, Check{"network", true, "skipped"})
	} else {
		netOK, netDetail := checkNetwork()
		r.Checks = append(r.Checks, Check{"network", netOK, netDetail})
	}

	// 7. Real capture probe
	if opts.CaptureTest {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		rep, err := engine.TestTabCapture(ctx, browserCfg, meetCfg, opts.Log)
		if err != nil {
			hint := "if the picker was blocked, try present_mode: screen with Xvfb"
			r.Checks = append(r.Checks, Check{"tab capture", false, fmt.Sprintf("%v (%s)", err, hint)})
		} else {
			detail := fmt.Sprintf("video=%t audio=%t label=%q frames=%d", rep.Video, rep.Audio, rep.Label, rep.Frames)
			if !rep.Audio {
				detail += " — no audio track captured (Meet tab shares usually include tab audio; check --enable-usermedia-screen-capturing)"
			}
			r.Checks = append(r.Checks, Check{"tab capture", true, detail})
		}
	}

	return r
}

// Print renders the report to stdout.
func (r Report) Print() {
	failed := 0
	for _, c := range r.Checks {
		mark := "✅"
		if !c.OK {
			mark = "❌"
			failed++
		}
		fmt.Printf("%s %-16s %s\n", mark, c.Name, c.Detail)
	}
	if failed == 0 {
		fmt.Println("\nall checks passed")
	} else {
		fmt.Printf("\n%d check(s) need attention\n", failed)
	}
}

// validateToken asks Telegram for the bot identity.
func validateToken(token string) (string, error) {
	type result struct {
		username string
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		u, _, err := bot.BotInfo(token)
		ch <- result{u, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return "", fmt.Errorf("telegram rejected the token: %v", res.err)
		}
		return res.username, nil
	case <-time.After(20 * time.Second):
		return "", fmt.Errorf("telegram API timeout (network blocked?)")
	}
}

// chromiumVersion returns `--version` output, best effort.
func chromiumVersion(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// portFree checks that addr can be bound.
func portFree(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("cannot bind %s: %v", addr, err)
	}
	return ln.Close()
}

// checkNetwork verifies outbound TLS to Telegram and Meet.
func checkNetwork() (bool, string) {
	targets := []string{"api.telegram.org:443", "meet.google.com:443"}
	var parts []string
	ok := true
	for _, t := range targets {
		conn, err := net.DialTimeout("tcp", t, 6*time.Second)
		if err != nil {
			ok = false
			parts = append(parts, fmt.Sprintf("%s unreachable (%v)", t, err))
			continue
		}
		_ = conn.Close()
		parts = append(parts, t+" ok")
	}
	return ok, strings.Join(parts, ", ")
}

// runCmd runs an installer command showing its output.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// which finds an executable on PATH.
func which(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}
