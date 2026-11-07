//go:build linux

package bluez

import (
	"sync"
	"time"
)

// Scanning pauses while a connection is being made, as on an ESP32. The kernel stops scanning to send LE
// Create Connection, but once the link is up it scans again at once if anything asks for it: a registered
// advertisement monitor (ours, the one that matches nothing included) or a discovery session. A controller
// that scans with a full duty cycle right then can miss the first connection events, and the link fails with
// HCI reason 0x3e (connection failed to be established). So while a connect is in progress the supervisor
// stops discovery and unregisters the monitors, and it arms them again once no connect is in progress.
// Connecting does not need either: the kernel scans for the device it connects to by itself.

// pauseLimit bounds a pause, should a connect hang beyond its own timeouts; a variable, so the tests can
// shorten it.
var pauseLimit = connectTimeout + resolveTimeout

// pauseWait is how long a connect waits for the supervisor to stop scanning before it goes ahead anyway, for
// example while the supervisor is busy restarting bluetoothd.
var pauseWait = 2 * time.Second

// pauseScan counts a connect as in progress, which holds the recovery steps off (see strike), and stops
// scanning for it if PauseOnConnect is set. It returns the function that ends both, which may be called more
// than once. With the pause it returns once the supervisor has stopped scanning, or after pauseWait.
// Overlapping connects share the pause: scanning resumes when the last one ends, or pauseLimit after each
// started, which also bounds how long a hung connect holds recovery off.
func (a *Adapter) pauseScan() (resume func()) {
	pause := a.pauseOnConnect
	var stopped chan struct{}
	a.mu.Lock()
	a.connecting++
	if pause {
		stopped = make(chan struct{})
		a.pauseWaiters = append(a.pauseWaiters, stopped)
	}
	a.mu.Unlock()
	if pause {
		a.kickScan()
	}

	var once sync.Once
	end := func(late bool) {
		once.Do(func() {
			if late && pause {
				a.log.Warn("connect still in progress, resuming scanning", "after", pauseLimit)
			} else if late {
				a.log.Warn("connect still in progress, no longer holding recovery off", "after", pauseLimit)
			}
			a.mu.Lock()
			a.connecting--
			a.mu.Unlock()
			if pause {
				a.kickScan()
			}
		})
	}
	limit := time.AfterFunc(pauseLimit, func() { end(true) })
	resume = func() {
		limit.Stop()
		end(false)
	}
	if !pause {
		return resume
	}

	t := time.NewTimer(pauseWait)
	defer t.Stop()
	select {
	case <-stopped:
	case <-t.C:
		a.log.Warn("scanning not paused in time, connecting anyway", "after", pauseWait)
	}
	return resume
}

// takePause tells the supervisor whether scanning is paused for a connect in progress and hands it the
// connects waiting for scanning to stop, to release once it has acted on the answer.
func (a *Adapter) takePause() (paused bool, waiters []chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	waiters, a.pauseWaiters = a.pauseWaiters, nil
	return a.pauseOnConnect && a.connecting > 0, waiters
}

func release(waiters []chan struct{}) {
	for _, w := range waiters {
		close(w)
	}
}
