package beam

import (
	"context"
	"errors"
	"sync"
	"time"
)

// chromecastLoadContext keeps request values while giving an active cast
// transport its own lifetime after startup has completed.
func (m *Manager) chromecastLoadContext(startup context.Context) (loadCtx context.Context, handoff func(), cancel context.CancelFunc) {
	if startup == nil {
		startup = context.Background()
	}
	loadCtx, cancelLoad := context.WithCancel(context.WithoutCancel(startup))
	var mu sync.Mutex
	watching := true
	watcherStopped := make(chan struct{})
	handoff = func() {
		mu.Lock()
		defer mu.Unlock()
		if watching {
			watching = false
			close(watcherStopped)
		}
	}
	cancel = func() {
		handoff()
		cancelLoad()
	}
	grace := m.chromecastLoadDeadlineGrace
	if err := startup.Err(); err != nil && (!errors.Is(err, context.DeadlineExceeded) || grace <= 0) {
		cancel()
		return loadCtx, handoff, cancel
	}
	// Background contexts need no watcher. A cancellable request's watcher
	// always exits when playback is handed off or the load itself is canceled.
	if startup.Done() != nil {
		go func() {
			select {
			case <-watcherStopped:
				return
			case <-startup.Done():
			}
			if errors.Is(startup.Err(), context.DeadlineExceeded) && grace > 0 {
				timer := time.NewTimer(grace)
				defer timer.Stop()
				select {
				case <-watcherStopped:
					return
				case <-timer.C:
				}
			}
			// Serialize cancellation with handoff so an expired grace timer
			// cannot cancel a transport already handed to the active session.
			mu.Lock()
			defer mu.Unlock()
			if watching {
				watching = false
				close(watcherStopped)
				cancelLoad()
			}
		}()
	}
	return loadCtx, handoff, cancel
}
