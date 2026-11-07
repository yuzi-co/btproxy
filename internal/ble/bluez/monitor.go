//go:build linux

package bluez

import (
	"errors"
	"time"

	"github.com/godbus/dbus/v5"
)

// Passive scanning goes through BlueZ's advertisement monitor API: while a monitor is registered, the kernel
// scans passively and BlueZ updates the Device1 objects of matching devices, whose PropertiesChanged signals
// feed the scanner exactly as during discovery. The API exists only when bluetoothd runs with --experimental
// (BlueZ 5.56 and later).
const (
	ifMonitorManager = "org.bluez.AdvertisementMonitorManager1"
	ifMonitor        = "org.bluez.AdvertisementMonitor1"

	monitorRoot = dbus.ObjectPath("/btproxy/advmon")
)

// orPattern is one entry of a monitor's Patterns property, D-Bus signature (yyay): the content must appear
// at offset Start of an AD structure of type Type.
type orPattern struct {
	Start, Type byte
	Content     []byte
}

// monitorPatterns are the advertisements the passive scanner sees; there is no match-all pattern. BlueZ
// 5.66 sends a monitor to the kernel in a buffer whose length is a uint8, so a monitor holds at most 7
// patterns (1 + 7*34 bytes); larger ones overflow it. Flags cover nearly all connectable and most beacon
// advertisements; the other two catch common sensors and beacons that leave the flags out.
var monitorPatterns = [][]orPattern{
	flagPatterns(0x06, 0x1a, 0x02, 0x04, 0x05, 0x0a, 0x18),
	// Manufacturer data by the low byte of the company id: Apple, Microsoft, Samsung, Google, Ruuvi,
	// Govee (0xec88), SwitchBot (0x0969).
	firstBytePatterns(0xff, 0x4c, 0x06, 0x75, 0xe0, 0x99, 0x88, 0x69),
	// 16-bit service data by the low byte of the UUID: Xiaomi (fe95), BTHome (fcd2), SwitchBot (fd3d),
	// environmental sensing (181a), Google (fe9f, fe2c), Tile (feed).
	firstBytePatterns(0x16, 0x95, 0xd2, 0x3d, 0x1a, 0x9f, 0x2c, 0xed),
}

// quietPatterns is one monitor that matches no real advertisement: manufacturer data under company id
// 0xffff, which the Bluetooth SIG keeps for tests, followed by bytes no device sends. Registered once raw
// advertisements have proven themselves (see Adapter.quiet), it keeps the kernel scanning passively with the
// controller's duplicate filter off, so the HCI monitor sees every advertisement, while bluetoothd, which
// matches the patterns itself, neither creates Device objects nor calls DeviceFound for any device.
var quietPatterns = [][]orPattern{{{Type: 0xff, Content: append([]byte{0xff, 0xff}, "btproxy:quiet"...)}}}

func flagPatterns(values ...byte) []orPattern { return firstBytePatterns(0x01, values...) }

func firstBytePatterns(adType byte, values ...byte) []orPattern {
	ps := make([]orPattern, len(values))
	for i, v := range values {
		ps[i] = orPattern{Type: adType, Content: []byte{v}}
	}
	return ps
}

func monitorPath(i int) dbus.ObjectPath {
	return monitorRoot + dbus.ObjectPath("/m"+string(rune('0'+i)))
}

// monitorObjects is the object tree BlueZ reads from the monitor application root: the real monitors, or
// the one that matches nothing.
func monitorObjects(quiet bool) map[dbus.ObjectPath]map[string]map[string]dbus.Variant {
	patterns := monitorPatterns
	if quiet {
		patterns = quietPatterns
	}
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{}
	for i, ps := range patterns {
		objs[monitorPath(i)] = map[string]map[string]dbus.Variant{ifMonitor: {
			"Type":     dbus.MakeVariant("or_patterns"),
			"Patterns": dbus.MakeVariant(ps),
		}}
	}
	return objs
}

// monitorApp serves GetManagedObjects on the application root.
type monitorApp struct{ a *Adapter }

func (m monitorApp) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	return monitorObjects(m.a.quietMons.Load()), nil
}

// monitorProps serves the monitors' properties, for clients that read them one by one.
type monitorProps struct {
	a    *Adapter
	path dbus.ObjectPath
}

func (m monitorProps) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != ifMonitor {
		return nil, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"no such interface"})
	}
	props, ok := monitorObjects(m.a.quietMons.Load())[m.path][ifMonitor]
	if !ok {
		return nil, dbus.NewError("org.freedesktop.DBus.Error.UnknownObject", []any{"no such monitor"})
	}
	return props, nil
}

func (m monitorProps) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	all, err := m.GetAll(iface)
	if err != nil {
		return dbus.Variant{}, err
	}
	v, ok := all[name]
	if !ok {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"no such property"})
	}
	return v, nil
}

func (monitorProps) Set(string, string, dbus.Variant) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.PropertyReadOnly", []any{"read-only"})
}

const (
	monitorRetryMax = 30 * time.Minute // the longest wait before trying dropped monitors again; twice this without a drop starts over
	ownRelease      = 2 * time.Second  // how long Release calls can trail our UnregisterMonitor's reply
	lateOwnRelease  = time.Minute      // the same when the call timed out, and runs when bluetoothd gets to it
)

// monitor receives BlueZ's calls on one monitor object.
type monitor struct{ a *Adapter }

func (m monitor) Release() *dbus.Error { m.a.monitorReleased(); return nil }

func (monitor) Activate() *dbus.Error { return nil }

// DeviceFound re-sends what is known about a device as it comes into range, since BlueZ reports later
// advertisements only when their content or RSSI changes.
func (m monitor) DeviceFound(p dbus.ObjectPath) *dbus.Error {
	var ad pending
	m.a.mu.Lock()
	if d := m.a.devices[p]; d != nil {
		ad = m.a.emit(d)
	}
	m.a.mu.Unlock()
	ad.deliver()
	return nil
}

func (monitor) DeviceLost(dbus.ObjectPath) *dbus.Error { return nil }

// exportMonitors puts the monitor application on the bus. The objects stay exported; registering them
// with BlueZ is what turns passive scanning on. Method tables rather than Export, for the binary size (see
// RegisterAgent).
func (a *Adapter) exportMonitors() error {
	app := monitorApp{a}
	if err := a.bus.ExportMethodTable(map[string]any{"GetManagedObjects": app.GetManagedObjects},
		monitorRoot, ifObjManager); err != nil {
		return err
	}
	for i := range monitorPatterns {
		p := monitorPath(i)
		m, props := monitor{a}, monitorProps{a, p}
		if err := a.bus.ExportMethodTable(map[string]any{"Release": m.Release, "Activate": m.Activate,
			"DeviceFound": m.DeviceFound, "DeviceLost": m.DeviceLost}, p, ifMonitor); err != nil {
			return err
		}
		if err := a.bus.ExportMethodTable(map[string]any{"Get": props.Get, "GetAll": props.GetAll,
			"Set": props.Set}, p, ifProps); err != nil {
			return err
		}
	}
	return nil
}

// registerMonitors starts passive scanning, with the monitor that matches nothing if quiet. It fails when
// bluetoothd runs without --experimental.
func (a *Adapter) registerMonitors(quiet bool) error {
	a.quietMons.Store(quiet) // BlueZ reads the objects during the call
	a.mu.Lock()
	a.monitorUp = true
	a.mu.Unlock()
	err := a.call(a.path, ifMonitorManager+".RegisterMonitor", monitorRoot).Err
	var de dbus.Error
	if errors.As(err, &de) && de.Name == "org.bluez.Error.AlreadyExists" {
		// BlueZ kept the application, as across a power cycle, and the scanner did not unregister it. Start over,
		// so that BlueZ reads the monitors as they are now.
		a.callUnregister()
		err = a.call(a.path, ifMonitorManager+".RegisterMonitor", monitorRoot).Err
	}
	if err != nil {
		a.mu.Lock()
		a.monitorUp = false
		a.mu.Unlock()
	}
	return err
}

func (a *Adapter) unregisterMonitors() {
	a.mu.Lock()
	a.monitorUp = false // the Release calls that follow are ours
	a.mu.Unlock()
	a.callUnregister()
}

// callUnregister asks BlueZ to drop the monitors, and it calls Release on each of them. Those calls can arrive
// after the scanner has registered the monitors again: right after the reply, as they run on their own goroutines,
// or much later when the call timed out, since a slow bluetoothd still runs it when it gets to it. They must not
// read as BlueZ dropping the monitors.
func (a *Adapter) callUnregister() {
	a.expectRelease(a.tm.callTimeout + ownRelease)
	if isTimeout(a.call(a.path, ifMonitorManager+".UnregisterMonitor", monitorRoot).Err) {
		a.expectRelease(lateOwnRelease)
	} else {
		a.expectRelease(ownRelease)
	}
}

func (a *Adapter) expectRelease(d time.Duration) {
	a.mu.Lock()
	a.ownReleaseUntil = time.Now().Add(d)
	a.mu.Unlock()
}

// monitorReleased handles BlueZ dropping a monitor it had accepted, for example because the kernel refused
// it: the scanner scans actively, and tries the monitors again later, less often each time it goes on.
func (a *Adapter) monitorReleased() {
	now := time.Now()
	a.mu.Lock()
	up := a.monitorUp && !now.Before(a.ownReleaseUntil)
	var wait time.Duration
	if up {
		a.monitorUp = false
		a.scan.SetActiveOnly(true)
		a.epoch++
		wait = a.tm.monitorRetry
		if now.Sub(a.lastRelease) < 2*monitorRetryMax {
			wait = min(2*a.releaseWait, monitorRetryMax)
		}
		a.releaseWait, a.lastRelease, a.retryMonitorAt = wait, now, now.Add(wait)
	}
	a.mu.Unlock()
	if up {
		a.log.Warn("bluetoothd released the advertisement monitor, scanning actively", "retry_in", wait)
		a.kickScan()
	}
}

// retryMonitors ends the fallback of monitorReleased when its time is up. The new epoch makes reconcile arm again.
func (a *Adapter) retryMonitors() {
	a.mu.Lock()
	due := a.scan.ActiveOnly() && !a.retryMonitorAt.IsZero() && !time.Now().Before(a.retryMonitorAt)
	if due {
		a.scan.SetActiveOnly(false)
		a.retryMonitorAt = time.Time{}
		a.epoch++
	}
	a.mu.Unlock()
	if due {
		a.log.Info("trying the advertisement monitor again")
	}
}
