//go:build windows

package svcinst

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// ServiceNameEnv lets the service dispatcher and the main binary agree on
// the registered name without an extra flag on the command line.
const ServiceNameEnv = "TELEMEETBOT_SERVICE_NAME"

// ErrNotAService is returned when the process was not started by the SCM.
// It is the expected condition for every interactive run and the caller must
// treat it as "run normally", not as a failure.
var ErrNotAService = errors.New("not started by the Windows service control manager")

// IsWindowsService reports whether this process is running as a Windows
// service (i.e. started by the SCM). Returns false on every non-Windows
// build and for interactive runs.
func IsWindowsService() bool {
	ok, _ := svc.IsWindowsService()
	return ok
}

// RunService connects the process to the SCM and runs the service loop,
// calling runFn when the SCM sends a Start command and cancelling the
// returned context when it sends Stop. runFn must return when ctx is
// cancelled: the SCM enforces a hard 30s timeout after a Stop.
func RunService(name string, runFn func(ctx context.Context) error) error {
	if !IsWindowsService() {
		return ErrNotAService
	}
	return svc.Run(name, &serviceHandler{name: name, runFn: runFn})
}

// serviceHandler implements windows/svc.Handler.
type serviceHandler struct {
	name  string
	runFn func(context.Context) error
}

type svcCHANGE = uint32

// Execute is the SCM entry point.
func (h *serviceHandler) Execute(args []string, changes <-chan svc.ChangeRequest, status chan<- svc.Status) (ssec bool, errno uint32) {
	const timeouts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending, Accepts: timeouts}
	defer func() { status <- svc.Status{State: svc.Stopped} }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	started := make(chan struct{}, 1)
	go func() {
		started <- struct{}{}
		runErr <- h.runFn(ctx)
	}()
	<-started

	status <- svc.Status{State: svc.Running, Accepts: timeouts}

	for {
		select {
		case c := <-changes:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				// The SCM gives the service a bounded window (30s by
				// default) to exit after Stop. Cancel first so the bot
				// unwinds cleanly, then report success when it returns.
				cancel()
				select {
				case <-runErr:
				case <-time.After(25 * time.Second):
					// Timed out waiting: report stopped anyway; the OS will
					// kill the process. Never block the SCM indefinitely.
				}
				return false, 0
			default:
				// Ignore commands the service does not accept (Pause,
				// Continue, etc.): the reported Accepts mask already says so.
			}
		case err := <-runErr:
			if err != nil {
				// ERROR_EXCEPTION_IN_SERVICE (1064) tells the SCM the run
				// loop failed; the recovery policy on the service then
				// decides whether to restart it.
				return false, uint32(windows.ERROR_EXCEPTION_IN_SERVICE)
			}
			return false, 0
		}
	}
}

// errServiceStart wraps a start failure for the event log / SCM exit code.
func errServiceStart(err error) error {
	return fmt.Errorf("service start: %w", err)
}

var _ = errServiceStart
