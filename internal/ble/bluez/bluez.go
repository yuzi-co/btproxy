//go:build linux

// Package bluez implements ble.Adapter on top of BlueZ's D-Bus API, so the adapter stays usable by other
// programs (bluetoothctl, other daemons) while the proxy runs.
package bluez

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

// Limits on what devices in radio range can make the proxy hold. Spoofed random addresses can report any number
// of devices, and a device can send any number of value changes while a read waits, on a router with 128 MB.
const (
	maxDevices   = 4096
	maxHeldValue = 64 // value changes held per characteristic while a read is in flight
)

const (
	busName      = "org.bluez"
	ifAdapter    = "org.bluez.Adapter1"
	ifDevice     = "org.bluez.Device1"
	ifService    = "org.bluez.GattService1"
	ifChar       = "org.bluez.GattCharacteristic1"
	ifDesc       = "org.bluez.GattDescriptor1"
	ifProps      = "org.freedesktop.DBus.Properties"
	ifObjManager = "org.freedesktop.DBus.ObjectManager"

	connectTimeout  = 30 * time.Second
	resolveTimeout  = 30 * time.Second
	gattCallTimeout = 30 * time.Second
	readEchoWindow  = 2 * time.Second // how long a read result may take to show up as a Value change
)

// timings are the waits that bound what bluetoothd and the supervisor do. The tests shorten them through
// Options; a zero field takes the default.
type timings struct {
	// callTimeout bounds every call that bluetoothd answers at once (see Adapter.call).
	callTimeout time.Duration
	// disconnectWait is how long a disconnect waits for its Connected=false signal to be handled (see
	// conn.Disconnect). Dispatch normally gets there within milliseconds.
	disconnectWait time.Duration
	// scanRetry is how often the supervisor retries when the adapter cannot scan (powered off, scan start
	// refused or unanswered) and checks for a stalled scan; recovery from a bluetoothd restart or an adapter
	// coming back is driven by signals instead.
	scanRetry time.Duration
	// pingEvery is how often the supervisor checks that bluetoothd still answers. Advertisements are no proof
	// once they come from the HCI monitor, which keeps reading the controller while bluetoothd hangs.
	pingEvery time.Duration
	// monitorRetry is the first wait before trying monitors BlueZ dropped again (see monitorReleased).
	monitorRetry time.Duration
	// pauseWait is how long a connect waits for the supervisor to stop scanning before it goes ahead anyway,
	// for example while the supervisor is busy restarting bluetoothd.
	pauseWait time.Duration
	// pauseLimit bounds a pause, should a connect hang beyond its own timeouts.
	pauseLimit time.Duration
}

func (t timings) withDefaults() timings {
	return timings{
		callTimeout:    cmp.Or(t.callTimeout, 10*time.Second),
		disconnectWait: cmp.Or(t.disconnectWait, 2*time.Second),
		scanRetry:      cmp.Or(t.scanRetry, 10*time.Second),
		pingEvery:      cmp.Or(t.pingEvery, time.Minute),
		monitorRetry:   cmp.Or(t.monitorRetry, time.Minute),
		pauseWait:      cmp.Or(t.pauseWait, 2*time.Second),
		pauseLimit:     cmp.Or(t.pauseLimit, connectTimeout+resolveTimeout),
	}
}

type Adapter struct {
	bus  *dbus.Conn
	path dbus.ObjectPath
	mac  uint64
	tm   timings

	mu      sync.Mutex
	devices map[dbus.ObjectPath]*device // advertisement state of the devices BlueZ reports, see maxDevices
	scanFn  func(ble.Advert)            // nil while not scanning
	conns   map[dbus.ObjectPath]*conn
	owner   string // bluetoothd's unique bus name, "" while it is not running; see trusted

	signals    *sigQueue
	allDevices bool // subscribed to every device's properties (see signals.go)

	scanState // see watch.go
	rawState  // see raw.go

	retry ble.ConnectRetry // see retry.go
}

// Options configures the adapter: the scanner's watchdog (see watch.go) and connects (see retry.go and
// ble.ScanPause).
type Options struct {
	ble.WatchdogOptions // StallTimeout restarts active discovery
	ble.ConnectOptions
	// RestartCmd is a shell command that restarts bluetoothd when scanning stalls or bluetoothd stops
	// answering repeatedly; empty disables it.
	RestartCmd string
	// RawAdverts selects where advertisements come from (see raw.go).
	RawAdverts RawMode
	// Log receives the adapter's log lines; nil discards them.
	Log *logx.Logger

	timings
}

// Open connects to the system bus and the named adapter (hci0).
func Open(name string, opt Options) (*Adapter, error) {
	signals := newSigQueue()
	bus, err := dbus.ConnectSystemBus(dbus.WithSignalHandler(signals))
	if err != nil {
		return nil, fmt.Errorf("system bus: %w", err)
	}
	a := &Adapter{
		bus: bus, path: dbus.ObjectPath("/org/bluez/" + name), signals: signals,
		devices: map[dbus.ObjectPath]*device{}, conns: map[dbus.ObjectPath]*conn{},
	}
	a.kick = make(chan struct{}, 1)
	// Nothing below outlives the bus connection but the supervisor and dispatch, which start last: closing it
	// on failure ends the godbus reader and the signal queue, and drops the match rules and exported objects.
	ok := false
	defer func() {
		if ok {
			return
		}
		a.mu.Lock()
		m := a.raw
		a.raw = nil
		a.mu.Unlock()
		if m != nil {
			m.Close()
		}
		bus.Close()
	}()
	if opt.Log == nil {
		opt.Log = logx.Discard()
	}
	a.log = opt.Log.With("adapter", string(a.path))
	a.tm = opt.timings.withDefaults()
	a.stallTimeout, a.restartCmd = opt.StallTimeout, opt.RestartCmd
	a.retry = opt.Retry(retryFast)
	a.scan.Log = a.log
	a.pause = ble.ScanPause{Pause: opt.PauseOnConnect, Wait: a.tm.pauseWait, Limit: a.tm.pauseLimit,
		Kick: a.kickScan, Log: a.log}
	if opt.USBReset {
		a.resetUSB = func() error { return resetUSBDevice(name) }
	}
	v, err := a.getProperty(a.path, ifAdapter+".Address")
	if err != nil {
		return nil, fmt.Errorf("adapter %s: %w", name, err)
	}
	if a.mac, err = ble.ParseMAC(v.Value().(string)); err != nil {
		return nil, err
	}
	if err := a.setProperty(a.path, ifAdapter+".Powered", true); err != nil {
		return nil, fmt.Errorf("power on %s: %w", name, err)
	}

	if err := a.addMatches(); err != nil {
		return nil, err
	}
	if err := a.syncMatches(); err != nil { // every device, until raw advertisements prove themselves
		return nil, err
	}

	objs, err := a.managedObjects()
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	for p, ifs := range objs {
		if props, ok := ifs[ifDevice]; ok && a.underAdapter(p) {
			a.updateDevice(p, props, false)
		}
	}
	a.mu.Unlock()
	if err := a.initRaw(name, opt.RawAdverts); err != nil {
		return nil, err
	}
	if err := a.initScan(); err != nil { // starts the supervisor, so after everything that can fail
		return nil, err
	}
	go a.dispatch()
	opened.add(a)
	ok = true
	return a, nil
}

// opened lists the adapters open in the process. BlueZ calls one default agent for pairings on every
// adapter, so the agent looks for the device among the connections of all of them.
var opened adapterList

type adapterList struct {
	mu   sync.Mutex
	list []*Adapter
}

func (l *adapterList) add(a *Adapter) {
	l.mu.Lock()
	l.list = append(l.list, a)
	l.mu.Unlock()
}

// connected tells whether one of the adapters has a connection to the device at p.
func (l *adapterList) connected(p dbus.ObjectPath) bool {
	l.mu.Lock()
	list := l.list
	l.mu.Unlock()
	for _, a := range list {
		a.mu.Lock()
		_, ok := a.conns[p]
		a.mu.Unlock()
		if ok {
			return true
		}
	}
	return false
}

func (a *Adapter) obj(p dbus.ObjectPath) dbus.BusObject { return a.bus.Object(busName, p) }

// call makes a method call that bluetoothd answers at once, giving up after the call timeout. A wedged
// bluetoothd keeps its bus name but never replies, and without a timeout the caller would wait for good. Calls
// that take long by design (connecting, pairing, GATT operations) have their own contexts instead.
func (a *Adapter) call(p dbus.ObjectPath, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), a.tm.callTimeout)
	defer cancel()
	c := a.obj(p).CallWithContext(ctx, method, 0, args...)
	if errors.Is(c.Err, context.DeadlineExceeded) {
		c.Err = fmt.Errorf("%s: no reply from bluetoothd in %v: %w", method[strings.LastIndex(method, ".")+1:],
			a.tm.callTimeout, c.Err)
	}
	return c
}

// isTimeout tells whether a call failed because bluetoothd did not answer.
func isTimeout(err error) bool { return errors.Is(err, context.DeadlineExceeded) }

// getProperty and setProperty are BusObject.GetProperty and SetProperty with call's timeout; prop is
// "interface.Name".
func (a *Adapter) getProperty(p dbus.ObjectPath, prop string) (dbus.Variant, error) {
	i := strings.LastIndex(prop, ".")
	var v dbus.Variant
	err := a.call(p, ifProps+".Get", prop[:i], prop[i+1:]).Store(&v)
	return v, err
}

func (a *Adapter) setProperty(p dbus.ObjectPath, prop string, v any) error {
	i := strings.LastIndex(prop, ".")
	return a.call(p, ifProps+".Set", prop[:i], prop[i+1:], dbus.MakeVariant(v)).Err
}

func (a *Adapter) underAdapter(p dbus.ObjectPath) bool {
	return strings.HasPrefix(string(p), string(a.path)+"/")
}

func (a *Adapter) managedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, error) {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err := a.call("/", ifObjManager+".GetManagedObjects").Store(&objs)
	return objs, err
}

func (a *Adapter) MAC() uint64 { return a.mac }

func (a *Adapter) startDiscovery() error {
	err := a.call(a.path, ifAdapter+".StartDiscovery").Err
	var de dbus.Error
	if errors.As(err, &de) && de.Name == "org.bluez.Error.InProgress" {
		return nil
	}
	return err
}

func (a *Adapter) devicePath(addr uint64) dbus.ObjectPath {
	return dbus.ObjectPath(string(a.path) + "/dev_" + strings.ReplaceAll(ble.FormatMAC(addr), ":", "_"))
}

// ClearCache removes the device from BlueZ, which drops its cached GATT database, unless it is paired or
// bonded: RemoveDevice would delete the bond too, and BlueZ has no call that clears only the GATT cache. For
// those devices this reports success and keeps the cache; BlueZ re-reads it only when the device's Service
// Changed indication or a fresh pairing tells it to.
func (a *Adapter) ClearCache(addr uint64) error {
	p := a.devicePath(addr)
	a.mu.Lock()
	_, connected := a.conns[p]
	a.mu.Unlock()
	if connected {
		return errors.New("device is connected")
	}
	for _, prop := range []string{"Paired", "Bonded"} {
		if v, err := a.getProperty(p, ifDevice+"."+prop); err == nil && v.Value() == true {
			return nil
		}
	}
	err := a.call(a.path, ifAdapter+".RemoveDevice", p).Err
	var de dbus.Error
	if errors.As(err, &de) && de.Name == "org.bluez.Error.DoesNotExist" {
		return nil
	}
	return err
}

// dispatch routes D-Bus signals to the scanner cache and to connections.
func (a *Adapter) dispatch() {
	var buf []*dbus.Signal
	var dropped int
	var lastDrop time.Time
	for {
		sigs, n, ok := a.signals.take(buf)
		if !ok {
			return
		}
		if dropped += n; dropped > 0 && time.Since(lastDrop) >= time.Minute {
			a.log.Warn("D-Bus signals dropped, the proxy falls behind", "signals", dropped)
			dropped, lastDrop = 0, time.Now()
		}
		for _, s := range sigs {
			a.handle(s)
		}
		clear(sigs)
		buf = sigs
	}
}

// trusted tells whether a signal comes from the bus daemon or bluetoothd. Match rules only choose which signals
// the bus sends us: any local client allowed on the bus can address a signal to us directly.
func (a *Adapter) trusted(s *dbus.Signal) bool {
	if s.Sender == "org.freedesktop.DBus" {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.owner != "" && s.Sender == a.owner
}

// handle routes one signal.
func (a *Adapter) handle(s *dbus.Signal) {
	if !a.trusted(s) {
		return
	}
	a.watch(s)
	switch s.Name {
	case ifProps + ".PropertiesChanged":
		if len(s.Body) < 2 {
			return
		}
		iface, _ := s.Body[0].(string)
		changed, _ := s.Body[1].(map[string]dbus.Variant)
		switch iface {
		case ifDevice:
			var ad pending
			a.mu.Lock()
			if a.devices[s.Path] != nil { // a change racing InterfacesRemoved must not bring it back
				ad = a.updateDevice(s.Path, changed, true)
			}
			c := a.conns[s.Path]
			a.mu.Unlock()
			ad.deliver()
			if c != nil {
				c.deviceChanged(changed)
			}
		case ifChar:
			if v, ok := changed["Value"]; ok {
				if c := a.connFor(s.Path); c != nil {
					if data, ok := v.Value().([]byte); ok {
						c.valueChanged(s.Path, data)
					}
				}
			}
		}
	case ifObjManager + ".InterfacesAdded":
		if len(s.Body) < 2 {
			return
		}
		p, _ := s.Body[0].(dbus.ObjectPath)
		ifs, _ := s.Body[1].(map[string]map[string]dbus.Variant)
		if props, ok := ifs[ifDevice]; ok && a.underAdapter(p) {
			a.mu.Lock()
			ad := a.updateDevice(p, props, true)
			a.mu.Unlock()
			ad.deliver()
		}
	case ifObjManager + ".InterfacesRemoved":
		if len(s.Body) < 2 {
			return
		}
		p, _ := s.Body[0].(dbus.ObjectPath)
		ifs, _ := s.Body[1].([]string)
		for _, i := range ifs {
			if i == ifDevice {
				a.mu.Lock()
				delete(a.devices, p)
				c := a.conns[p]
				a.mu.Unlock()
				if c != nil {
					c.deviceChanged(map[string]dbus.Variant{"Connected": dbus.MakeVariant(false)})
				}
			}
		}
	}
}

// connFor finds the connection owning a GATT object path (/org/bluez/hci0/dev_XX/serviceNNNN/charNNNN).
func (a *Adapter) connFor(p dbus.ObjectPath) *conn {
	s := string(p)
	i := strings.Index(s, "/service")
	if i < 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conns[dbus.ObjectPath(s[:i])]
}
