//go:build windows

package main

import (
	"context"
	"os"
	"os/signal"
)

// termSignalContext returns a context that is cancelled on the Windows
// interruption events. SIGTERM cannot be caught on Windows (the runtime maps
// it to an unconditional TerminateProcess), so the context listens for
// os.Interrupt, which covers both Ctrl+C and the SCM stop signal delivered to
// a console-attached service.
func termSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt)
}
