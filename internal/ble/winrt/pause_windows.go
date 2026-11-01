package winrt

import (
	"sync"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// Scanning pauses while a connection is being made, as on an ESP32 and with the BlueZ backend: from the connect
// call until the services are resolved or the connect fails, the advertisement watcher is stopped and the
// scanner reported idle. Windows scans for the device it connects to by itself; the watcher only adds scanning
// with a full duty cycle right when the first connection events have to get through. Other programs' watchers
// keep running, so the radio may still scan.

// pauseLimit bounds a pause, should a connect hang beyond its own timeouts; a variable, so the tests can
// shorten it.
var pauseLimit = connectTimeout + callTimeout

// pauseScan stops the watcher for a connect and returns the function that ends the pause, which may be called
// more than once. Overlapping connects share the pause: scanning resumes when the last one ends, or
// pauseLimit after each started.
func (a *Adapter) pauseScan() (resume func()) {
	if !a.pauseOnConnect {
		return func() {}
	}
	a.mu.Lock()
	a.connecting++
	if a.connecting == 1 && a.scanning {
		a.stopLocked()
		a.setStatusLocked(ble.ScanIdle)
	}
	a.mu.Unlock()

	var once sync.Once
	end := func(late bool) {
		once.Do(func() {
			if late {
				a.log.Warn("connect still in progress, resuming scanning", "after", pauseLimit)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			a.connecting--
			if a.connecting > 0 || !a.scanning || a.w != nil || a.retry != nil {
				return
			}
			if err := a.startLocked(); err != nil {
				a.log.Warn("resuming scanning failed, retrying", "err", err, "in", scanRetry)
				a.failedLocked()
			}
		})
	}
	limit := time.AfterFunc(pauseLimit, func() { end(true) })
	return func() {
		limit.Stop()
		end(false)
	}
}
