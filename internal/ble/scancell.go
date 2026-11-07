package ble

import (
	"sync"

	"github.com/yuzi-co/btproxy/internal/logx"
)

// ScanCell is the scanner state a ModeScanner reports: the requested mode, whether passive scanning turned out
// unavailable (the scan then runs actively whatever the mode), the status, and the status callback. It has a
// lock of its own, so a backend reads and sets it holding any lock of its own or none. The callback gets each
// change once, in order, from Set or SetState at once, or from Publish for a backend that sets the status
// holding a lock the callback must not run under (see Store). It runs under no lock but the cell's own notify
// order, so it may read the cell, but must not set it.
type ScanCell struct {
	// Log gets a debug line per status change; nil discards it.
	Log *logx.Logger

	mu         sync.Mutex
	mode       ScanMode
	activeOnly bool
	status     ScanStatus
	published  ScanStatus // what the callback last got
	fn         func(ScanStatus)

	notify sync.Mutex // serializes the callback, so changes reach it in order
}

// Mode is the requested mode.
func (c *ScanCell) Mode() ScanMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// SetMode records the requested mode; the status keeps its mode until the backend sets it.
func (c *ScanCell) SetMode(m ScanMode) {
	c.mu.Lock()
	c.mode = m
	c.mu.Unlock()
}

// ActiveOnly tells whether passive scanning is unavailable.
func (c *ScanCell) ActiveOnly() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeOnly
}

// SetActiveOnly records whether passive scanning is unavailable, which makes Effective active.
func (c *ScanCell) SetActiveOnly(v bool) {
	c.mu.Lock()
	c.activeOnly = v
	c.mu.Unlock()
}

// Effective is the mode the scan runs in: the requested one, unless passive scanning is unavailable.
func (c *ScanCell) Effective() ScanMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.effective()
}

func (c *ScanCell) effective() ScanMode {
	if c.activeOnly {
		return ScanActive
	}
	return c.mode
}

// Setting names how the scan runs, for ScanSettinger.
func (c *ScanCell) Setting() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.activeOnly:
		return "Active, passive unavailable"
	case c.mode == ScanActive:
		return "Active"
	}
	return "Passive"
}

// Status is the status, for ModeScanner.ScanStatus.
func (c *ScanCell) Status() ScanStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// OnStatus sets the callback, for ModeScanner.OnScanStatus.
func (c *ScanCell) OnStatus(fn func(ScanStatus)) {
	c.mu.Lock()
	c.fn = fn
	c.mu.Unlock()
}

// Set records the status and, if it changed, tells the callback.
func (c *ScanCell) Set(st ScanStatus) {
	c.Store(st)
	c.Publish()
}

// SetState is Set with the state and the effective mode.
func (c *ScanCell) SetState(s ScanState) {
	c.StoreState(s)
	c.Publish()
}

// Store records the status without telling the callback, for a backend that sets the status holding a lock
// the callback must not run under: it stores under the lock and publishes after.
func (c *ScanCell) Store(st ScanStatus) {
	c.mu.Lock()
	c.status = st
	c.mu.Unlock()
}

// StoreState is Store with the state and the effective mode.
func (c *ScanCell) StoreState(s ScanState) {
	c.mu.Lock()
	c.status = ScanStatus{State: s, Mode: c.effective()}
	c.mu.Unlock()
}

// Publish tells the callback the status if it changed since the callback last got it.
func (c *ScanCell) Publish() {
	c.notify.Lock()
	defer c.notify.Unlock()
	c.mu.Lock()
	st, fn := c.status, c.fn
	changed := st != c.published
	c.published = st
	c.mu.Unlock()
	if !changed {
		return
	}
	if c.Log != nil {
		c.Log.Debug("scanner status", "state", st.State, "mode", st.Mode)
	}
	if fn != nil {
		fn(st)
	}
}
