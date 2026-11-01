//go:build linux

package bluez

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"btproxy/internal/ble"
	"btproxy/internal/ble/hci"
)

// RawMode selects where advertisements come from.
type RawMode int

const (
	// RawAuto reads advertisements from the HCI monitor when it can be opened and delivers, and rebuilds them
	// from BlueZ's Device1 properties otherwise.
	RawAuto RawMode = iota
	// RawOn requires the HCI monitor: Open fails without it. Should it fail later, the proxy falls back
	// all the same.
	RawOn
	// RawOff always rebuilds advertisements from BlueZ's properties.
	RawOff
)

// ParseRawMode parses auto, on or off.
func ParseRawMode(s string) (RawMode, error) {
	switch s {
	case "auto":
		return RawAuto, nil
	case "on":
		return RawOn, nil
	case "off":
		return RawOff, nil
	}
	return 0, fmt.Errorf("unknown raw advertisement mode %q", s)
}

// rawMissLimit is how many advertisements BlueZ may report before the monitor has delivered any, in auto
// mode, before the proxy gives up on the monitor. The monitor sees an advertisement before bluetoothd does,
// so a working one has delivered long before.
const rawMissLimit = 30

// rawSource is a source of advertisements as received, the HCI monitor outside the tests.
type rawSource interface {
	// Run delivers advertisements until Close is called (nil) or reading fails. active tells whether the
	// scan running is active.
	Run(emit func(ble.Advert), active func() bool) error
	Close()
}

// openRaw opens the monitor for a controller index; the tests replace it.
var openRaw = func(index uint16) (rawSource, error) { return hci.Open(index) }

// rawState is the raw advertisement part of Adapter. BlueZ still drives scanning and connections; only where
// advertisements come from changes. Fields are guarded by Adapter.mu except rawActive.
type rawState struct {
	raw       rawSource // nil: advertisements are rebuilt from Device1 properties
	rawAuto   bool
	rawSeen   bool        // the monitor has delivered an advertisement
	rawMisses int         // advertisements BlueZ reported before that
	rawActive atomic.Bool // the scan armed is active, so scan responses come
}

// initRaw opens the HCI monitor of the adapter (hciN is controller index N) unless mode is RawOff.
func (a *Adapter) initRaw(name string, mode RawMode) error {
	if mode == RawOff {
		return nil
	}
	m, err := func() (rawSource, error) {
		n, ok := strings.CutPrefix(name, "hci")
		idx, err := strconv.ParseUint(n, 10, 16)
		if !ok || err != nil {
			return nil, fmt.Errorf("no controller index in adapter name %q", name)
		}
		return openRaw(uint16(idx))
	}()
	if err != nil {
		if mode == RawOn {
			return fmt.Errorf("raw advertisements: %w", err)
		}
		a.log.Info("raw advertisements unavailable, rebuilding them from BlueZ's properties", "err", err)
		return nil
	}
	a.mu.Lock()
	a.raw, a.rawAuto = m, mode == RawAuto
	a.mu.Unlock()
	a.log.Info("reading raw advertisements from the HCI monitor")
	go func() {
		err := m.Run(a.rawAdvert, a.rawActive.Load)
		a.mu.Lock()
		defer a.mu.Unlock()
		if err != nil && a.raw == m {
			a.log.Warn("HCI monitor failed, rebuilding advertisements from BlueZ's properties", "err", err)
			a.dropRaw()
		}
	}()
	return nil
}

// rawAdvert delivers an advertisement from the monitor.
func (a *Adapter) rawAdvert(ad ble.Advert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.raw == nil {
		return
	}
	a.rawSeen = true
	if a.scanFn != nil {
		a.adverts++
		a.scanFn(ad)
	}
}

// rawHandles tells emit whether the monitor delivers advertisements, so BlueZ's are not wanted. In auto mode,
// a monitor that stays silent while BlueZ reports advertisements is given up. Called with a.mu held.
func (a *Adapter) rawHandles() bool {
	if a.raw == nil {
		return false
	}
	if a.rawAuto && !a.rawSeen && a.scanFn != nil {
		if a.rawMisses++; a.rawMisses >= rawMissLimit {
			a.log.Warn("HCI monitor delivers no advertisements, rebuilding them from BlueZ's properties",
				"bluez_adverts", a.rawMisses)
			a.dropRaw()
			return false
		}
	}
	return true
}

// dropRaw closes the monitor and has the supervisor restart the scan, so discovery reports duplicates
// again. Called with a.mu held.
func (a *Adapter) dropRaw() {
	a.raw.Close()
	a.raw = nil
	a.epoch++
	a.kickScan()
}
