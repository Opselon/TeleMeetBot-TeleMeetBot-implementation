package engine

import (
	"sync"
	"testing"
)

// TestSeekByRapidPressesConcurrentWithStop reproduces the +60s crash report:
// rapid seek presses arriving while the session is being stopped (the user
// pressed the button at the moment a session ended, or the refresh loop raced
// the button handler). Every one of these goroutines must be panic-free.
func TestSeekByRapidPressesConcurrentWithStop(t *testing.T) {
	fm := &fakeMedia{currentTime: 30, duration: 300, readyState: 4, seekable: 3}
	s := &Session{
		ID:       "s-crash",
		mediaRun: fm.run,
		log:      noopLogger{},
	}
	s.SetStopForTesting()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("concurrent SeekBy panicked: %v", r)
				}
			}()
			_, _ = s.SeekBy(60)
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("concurrent Snapshot panicked: %v", r)
				}
			}()
			_ = s.Snapshot()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("concurrent Stop panicked: %v", r)
			}
		}()
		s.Stop()
	}()
	wg.Wait()
}
