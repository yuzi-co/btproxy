//go:build linux

package bluez

import (
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

// monitorObjects is the object tree BlueZ reads from the monitor application root.
func monitorObjects() map[dbus.ObjectPath]map[string]map[string]dbus.Variant {
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{}
	for i, ps := range monitorPatterns {
		objs[monitorPath(i)] = map[string]map[string]dbus.Variant{ifMonitor: {
			"Type":     dbus.MakeVariant("or_patterns"),
			"Patterns": dbus.MakeVariant(ps),
		}}
	}
	return objs
}

// monitorApp serves GetManagedObjects on the application root.
type monitorApp struct{}

func (monitorApp) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	return monitorObjects(), nil
}

// monitorProps serves the monitors' properties, for clients that read them one by one.
type monitorProps struct{ path dbus.ObjectPath }

func (m monitorProps) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != ifMonitor {
		return nil, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"no such interface"})
	}
	return monitorObjects()[m.path][ifMonitor], nil
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

// monitor receives BlueZ's calls on one monitor object.
type monitor struct{ a *Adapter }

func (m monitor) Release() *dbus.Error { m.a.monitorReleased(); return nil }

func (monitor) Activate() *dbus.Error { return nil }

// DeviceFound re-sends what is known about a device as it comes into range, since BlueZ reports later
// advertisements only when their content or RSSI changes.
func (m monitor) DeviceFound(p dbus.ObjectPath) *dbus.Error {
	m.a.mu.Lock()
	defer m.a.mu.Unlock()
	if d := m.a.devices[p]; d != nil {
		m.a.emit(d)
	}
	return nil
}

func (monitor) DeviceLost(dbus.ObjectPath) *dbus.Error { return nil }

// exportMonitors puts the monitor application on the bus. The objects stay exported; registering them
// with BlueZ is what turns passive scanning on. Method tables rather than Export, for the binary size (see
// RegisterAgent).
func (a *Adapter) exportMonitors() error {
	app := monitorApp{}
	if err := a.bus.ExportMethodTable(map[string]any{"GetManagedObjects": app.GetManagedObjects},
		monitorRoot, ifObjManager); err != nil {
		return err
	}
	for i := range monitorPatterns {
		p := monitorPath(i)
		m, props := monitor{a}, monitorProps{p}
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

// registerMonitors starts passive scanning. It fails when bluetoothd runs without --experimental.
func (a *Adapter) registerMonitors() error {
	a.mu.Lock()
	a.monitorUp = true
	a.mu.Unlock()
	err := a.call(a.path, ifMonitorManager+".RegisterMonitor", monitorRoot).Err
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
	a.call(a.path, ifMonitorManager+".UnregisterMonitor", monitorRoot)
}

// monitorReleased handles BlueZ dropping a monitor it had accepted, for example because the kernel refused
// it: passive scanning is given up until bluetoothd restarts.
func (a *Adapter) monitorReleased() {
	a.mu.Lock()
	up := a.monitorUp
	if up {
		a.monitorUp = false
		a.noMonitor = true
		a.epoch++
	}
	a.mu.Unlock()
	if up {
		a.log.Warn("bluetoothd released the advertisement monitor, scanning actively")
		a.kickScan()
	}
}
