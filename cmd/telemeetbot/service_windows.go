//go:build windows

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Opselon/TeleMeetBot/internal/config"
	"github.com/Opselon/TeleMeetBot/internal/svcinst"
)

// shouldAutoEnterServiceMode reports whether the SCM started this process.
// Only true on Windows; every other platform returns false and the flag is
// a plain no-op.
func shouldAutoEnterServiceMode() bool {
	return svcinst.IsWindowsService()
}

// runAsService connects to the SCM and blocks in the service loop until the
// SCM sends Stop or the bot itself fails. The service name comes from the
// environment when the registration used a custom name.
func runAsService() error {
	name := os.Getenv(svcinst.ServiceNameEnv)
	if name == "" {
		name = "telemeetbot"
	}
	return svcinst.RunService(name, func(ctx context.Context) error {
		cfgPath := config.ResolvePath("")
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return fmt.Errorf("load config %s: %w", cfgPath, err)
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		log := newLogger(cfg.Logging.Level, cfg.Logging.File)
		return run(cfg, cfgPath, log)
	})
}
