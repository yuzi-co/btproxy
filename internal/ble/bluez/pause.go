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

// pauseScan stops scanning for a connect and returns the function that ends the pause, which may be called
// more than once. It returns once the supervisor has stopped scanning, or after pauseWait. Overlapping
// connects share the pause: scanning resumes when the last one ends, or pauseLimit after each started.
func (a *Adapter) pauseScan() (resume func()) {
	if !a.pauseOnConnect {
		return func() {}
	}
	stopped := make(chan struct{})
	a.mu.Lock()
	a.connecting++
	a.pauseWaiters = append(a.pauseWaiters, stopped)
	a.mu.Unlock()
	a.kickScan()

	var once sync.Once
	end := func(late bool) {
		once.Do(func() {
			if late {
				a.log.Warn("connect still in progress, resuming scanning", "after", pauseLimit)
			}
			a.mu.Lock()
			a.connecting--
			a.mu.Unlock()
			a.kickScan()
		})
	}
	limit := time.AfterFunc(pauseLimit, func() { end(true) })
	resume = func() {
		limit.Stop()
		end(false)
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

// takePause tells the supervisor whether a connect is in progress and hands it the connects waiting for
// scanning to stop, to release once it has acted on the answer.
func (a *Adapter) takePause() (paused bool, waiters []chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	waiters, a.pauseWaiters = a.pauseWaiters, nil
	return a.connecting > 0, waiters
}

func release(waiters []chan struct{}) {
	for _, w := range waiters {
		close(w)
	}
}
