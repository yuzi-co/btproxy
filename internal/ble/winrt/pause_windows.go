package winrt

import "github.com/yuzi-co/btproxy/internal/ble"

// Scanning pauses while a connection is being made, as on an ESP32 and with the BlueZ backend (see
// ble.ScanPause): from the connect call until the services are resolved or the connect fails, the
// advertisement watcher is stopped and the scanner reported idle. Windows scans for the device it connects to
// by itself; the watcher only adds scanning with a full duty cycle right when the first connection events have
// to get through. Other programs' watchers keep running, so the radio may still scan.

// pauseScan counts a connect in and returns the function that counts it out, which may be called more than
// once. With PauseOnConnect the watcher is stopped before it returns, and started again when the last connect
// ends, or pauseLimit after this one began.
func (a *Adapter) pauseScan() (resume func()) { return a.pause.Begin() }

// applyPause stops the watcher while scanning is paused for a connect and starts it again after, from the
// gate's state. Nothing starts while a retry is pending: the retry does that if the pause is over by then.
func (a *Adapter) applyPause() {
	a.mu.Lock()
	defer a.unlockPublish()
	if a.pause.Paused() {
		if a.scanning {
			a.stopLocked()
			a.scan.StoreState(ble.ScanIdle)
		}
		return
	}
	if !a.scanning || a.w != nil || a.retry != nil {
		return
	}
	if err := a.startLocked(); err != nil {
		a.log.Warn("resuming scanning failed, retrying", "err", err, "in", a.tm.scanRetry)
		a.failedLocked()
	}
}
