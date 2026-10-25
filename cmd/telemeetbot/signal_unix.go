//go:build unix

package main

import (
	"context"
	"os/signal"
	"syscall"
)

// termSignalContext returns a context that is cancelled when the process
// receives SIGINT or SIGTERM. Both are the standard supervision signals on
// Unix: SIGINT from an interactive Ctrl+C, SIGTERM from systemd/launchd.
func termSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
}
