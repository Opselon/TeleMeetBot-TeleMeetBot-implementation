//go:build !windows

package main

import (
	"context"
	"errors"
)

// shouldAutoEnterServiceMode is always false on non-Windows builds: there is
// no SCM, and --service exists only so the same command line works everywhere.
func shouldAutoEnterServiceMode() bool { return false }

// runAsService is unreachable on non-Windows builds. Returning a clear error
// (instead of not existing at all) keeps the flag's behaviour documented in
// the code rather than in the reader's memory.
func runAsService() error {
	return errors.New("--service is only meaningful on Windows")
}

var _ = context.Background
