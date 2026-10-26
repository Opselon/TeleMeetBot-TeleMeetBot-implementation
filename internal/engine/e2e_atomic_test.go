package engine

import "sync/atomic"

// atomicError is a race-free error slot for test goroutines that report
// failures from concurrent playback probes.
type atomicError struct{ v atomic.Value }

func (a *atomicError) set(err error) { a.v.Store(err) }
func (a *atomicError) get() error {
	if a.v.Load() == nil {
		return nil
	}
	return a.v.Load().(error)
}
