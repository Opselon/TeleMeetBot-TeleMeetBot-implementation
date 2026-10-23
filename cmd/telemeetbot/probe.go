package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/engine"
	"github.com/Opselon/TeleMeetBot/internal/health"
	"github.com/Opselon/TeleMeetBot/internal/web"
)

// runMediaProbe is the media-side diagnosis mode (--media-probe <link>): it
// opens the link through the same pipeline the player tab uses and samples
// playback + rendered pixels, without touching a meeting.
func runMediaProbe(mediaLink string, seconds int, cfg *config.Config, log *slogLogger) error {
	bCfg, _, _ := engine.FromConfig(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds+75)*time.Second)
	defer cancel()

	rep, err := engine.ProbeMedia(ctx, bCfg, mediaLink, seconds, log)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(out))
	if rep.Progressed && !rep.GreenFrames && !rep.BlackFrames && !rep.ConsentPresent &&
		rep.PageProblem == "" && rep.PlayerErr == "" && rep.Verification == "" {
		return nil
	}
	return fmt.Errorf("media verdict: %s", rep.Verdict)
}

// runProbe is the live diagnosis mode (--probe <meet-link>): one session, the
// same engine the bot runs, no Telegram, no dashboard — events flow straight
// to the log so a failing meeting can be watched in real time. The probe
// overrides the configured admit/join wait to keep the one-shot bounded.
func runProbe(meetLink, mediaLink string, probeSeconds int, cfg *config.Config, log *slogLogger) error {
	stopHealth, err := startProbeHealth(cfg.Server.HealthAddr, log)
	if err != nil {
		log.Warnf("probe: health endpoint unavailable (%v) — continuing without it", err)
	}
	defer func() {
		if stopHealth != nil {
			stopHealth()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bCfg, mCfg, meetCfg := engine.FromConfig(cfg)
	meetCfg.AdmitWaitSeconds = probeSeconds
	if time.Duration(meetCfg.JoinTimeoutSeconds)*time.Second < time.Duration(probeSeconds)*time.Second {
		meetCfg.JoinTimeoutSeconds = probeSeconds
	}

	path, browserErr := engine.ResolveBrowser(bCfg, log)
	if browserErr != nil {
		log.Warnf("probe: browser problem: %v — the probe will fail fast", browserErr)
	} else {
		log.Infof("probe: chromium: %s", path)
	}
	log.Infof("probe: meet=%s media=%s admit_wait=%ds", meetLink, mediaLink, probeSeconds)

	cancelCtx, stopProbe := context.WithCancel(context.Background())
	defer stopProbe()

	eng := engine.NewManager(bCfg, mCfg, meetCfg, log)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case ev, ok := <-eng.Events():
				if !ok {
					return
				}
				switch ev.Kind {
				case engine.EventError:
					log.Errorf("probe event: session=%s kind=%s text=%s", ev.Session, ev.Kind, ev.Text)
				default:
					log.Infof("probe event: session=%s kind=%s text=%s", ev.Session, ev.Kind, ev.Text)
				}
			case <-cancelCtx.Done():
				return
			}
		}
	}()

	s, err := eng.Start(ctx, engine.StartRequest{
		MeetLink:  meetLink,
		MediaLink: mediaLink,
		TabTitle:  meetCfg.CaptureSource,
	})
	if err != nil {
		stopProbe()
		eng.StopAll()
		<-done
		log.Errorf("probe start failed: %v", err)
		return err
	}

	select {
	case <-s.Done():
		stopProbe()
		eng.StopAll()
		<-done
		st := s.Snapshot()
		log.Infof("probe finished: state=%s detail=%q error=%q", st.State, st.Detail, st.Error)
		if st.State == engine.StateError {
			return errors.New(st.Error)
		}
		return nil
	case <-time.After(time.Duration(probeSeconds+45) * time.Second):
		stopProbe()
		eng.StopAll()
		<-done
		return fmt.Errorf("probe did not finish within %ds — killing it", probeSeconds+45)
	}
}

// startProbeHealth serves /healthz and /livez on the probe address so the
// one-shot run is observable the same way a real bot is.
func startProbeHealth(addr string, log *slogLogger) (func(), error) {
	binding, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	snap := func() health.Snapshot {
		return health.Snapshot{
			Version:   "probe-" + config.Version,
			StartedAt: time.Now(),
			Uptime:    time.Since(startedAt),
		}
	}
	srv := web.New(addr, snap, log)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeOn(binding, ctx) }()
	go func() {
		select {
		case e := <-errCh:
			if e != nil {
				log.Warnf("probe health server: %v", e)
			}
		case <-ctx.Done():
		}
	}()
	return cancel, nil
}

// startedAt anchors the probe's uptime readout.
var startedAt = time.Now()
