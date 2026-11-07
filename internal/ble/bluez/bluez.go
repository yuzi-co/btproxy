//go:build linux

// Package bluez implements ble.Adapter on top of BlueZ's D-Bus API, so the adapter stays usable by other
// programs (bluetoothctl, other daemons) while the proxy runs.
package bluez

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
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

// callTimeout bounds every call that bluetoothd answers at once (see Adapter.call). Variables, so the tests
// can shorten them.
var (
	callTimeout = 10 * time.Second
	// disconnectWait is how long a disconnect waits for its Connected=false signal to be handled (see
	// conn.Disconnect). Dispatch normally gets there within milliseconds.
	disconnectWait = 2 * time.Second
)

type Adapter struct {
	bus  *dbus.Conn
	path dbus.ObjectPath
	mac  uint64

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

// Options configures the scanner's watchdog (see watch.go).
type Options struct {
	// StallTimeout restarts active discovery when it delivers no advertisement for this long; 0 disables it.
	StallTimeout time.Duration
	// RestartCmd is a shell command that restarts bluetoothd when scanning stalls or bluetoothd stops
	// answering repeatedly; empty disables it.
	RestartCmd string
	// USBReset lets the watchdog reset the adapter's USB device when nothing else brought scanning back.
	USBReset bool
	// RawAdverts selects where advertisements come from (see raw.go).
	RawAdverts RawMode
	// PauseOnConnect stops scanning while a connect is in progress (see pause.go).
	PauseOnConnect bool
	// ConnectRetries is how often a connect that failed on the radio or lost the link early is retried (see retry.go).
	ConnectRetries int
	// ConnectRetryDelay is the wait before the first retry; each later one waits twice the previous. 0 retries at once.
	ConnectRetryDelay time.Duration
	// Log receives the adapter's log lines; nil discards them.
	Log *logx.Logger
}

// device caches the Device1 properties an advertisement is rebuilt from.
type device struct {
	addr     uint64
	addrType uint32
	rssi     int16 // 0 until BlueZ reports one, so cached devices that are out of range stay silent
	name     string
	txPower  *int16
	flags    []byte
	uuids    []string
	mfr      map[uint16][]byte
	svcData  map[string][]byte
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
	a.stallTimeout, a.restartCmd, a.pauseOnConnect = opt.StallTimeout, opt.RestartCmd, opt.PauseOnConnect
	a.retry = connectRetry(opt.ConnectRetries, opt.ConnectRetryDelay)
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

// call makes a method call that bluetoothd answers at once, giving up after callTimeout. A wedged bluetoothd
// keeps its bus name but never replies, and without a timeout the caller would wait for good. Calls that
// take long by design (connecting, pairing, GATT operations) have their own contexts instead.
func (a *Adapter) call(p dbus.ObjectPath, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	c := a.obj(p).CallWithContext(ctx, method, 0, args...)
	if errors.Is(c.Err, context.DeadlineExceeded) {
		c.Err = fmt.Errorf("%s: no reply from bluetoothd in %v: %w", method[strings.LastIndex(method, ".")+1:],
			callTimeout, c.Err)
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

// evictDevice forgets one device that is not connected; which one is left to chance, so a flood of new
// addresses cannot keep a real device out for long. Called with a.mu held.
func (a *Adapter) evictDevice() {
	for p := range a.devices {
		if a.conns[p] == nil {
			delete(a.devices, p)
			return
		}
	}
}

// updateDevice merges Device1 properties into the cache and, if asked and scanning, returns an advertisement
// for the caller to deliver once it has released a.mu (see emit). While raw advertisements flow, the cache
// is not used for advertisements and its data maps are not rebuilt; they are cleared instead, so a fallback
// does not send stale data. Called with a.mu held.
func (a *Adapter) updateDevice(p dbus.ObjectPath, props map[string]dbus.Variant, emit bool) pending {
	d := a.devices[p]
	if d == nil {
		if len(a.devices) >= maxDevices {
			a.evictDevice()
		}
		d = &device{}
		a.devices[p] = d
	}
	fresh, cache := false, !a.rawProven()
	for k, v := range props {
		switch k {
		case "Address":
			if s, ok := v.Value().(string); ok {
				d.addr, _ = ble.ParseMAC(s)
			}
		case "AddressType":
			if s, _ := v.Value().(string); s == "random" {
				d.addrType = ble.AddrRandom
			} else {
				d.addrType = ble.AddrPublic
			}
		case "RSSI":
			if r, ok := v.Value().(int16); ok {
				d.rssi = r
				fresh = true
			}
		case "Name":
			d.name, _ = v.Value().(string)
		case "TxPower":
			if t, ok := v.Value().(int16); ok {
				d.txPower = &t
			}
		case "AdvertisingFlags":
			d.flags, _ = v.Value().([]byte)
		case "UUIDs":
			d.uuids, _ = v.Value().([]string)
		case "ManufacturerData":
			d.mfr = nil
			if m, ok := v.Value().(map[uint16]dbus.Variant); ok && cache {
				d.mfr = make(map[uint16][]byte, len(m))
				for id, data := range m {
					d.mfr[id], _ = data.Value().([]byte)
				}
			}
			fresh = true
		case "ServiceData":
			d.svcData = nil
			if m, ok := v.Value().(map[string]dbus.Variant); ok && cache {
				d.svcData = make(map[string][]byte, len(m))
				for u, data := range m {
					d.svcData[u], _ = data.Value().([]byte)
				}
			}
			fresh = true
		}
	}
	if emit && fresh {
		return a.emit(d)
	}
	return pending{}
}

// adData rebuilds raw AD structures from the cached properties. BlueZ only exposes the parsed form, so field
// order and the split between advertisement and scan response are not the original; Home Assistant parses
// the fields, so that does not matter. Fields that do not fit in 62 bytes are dropped, least useful last.
func (d *device) adData() []byte {
	out := make([]byte, 0, 62)
	add := func(typ byte, payload []byte) {
		if len(payload) > 0 && len(out)+2+len(payload) <= 62 {
			out = append(out, byte(len(payload)+1), typ)
			out = append(out, payload...)
		}
	}
	if len(d.flags) > 0 {
		add(0x01, d.flags)
	}
	for id, data := range d.mfr {
		add(0xff, append([]byte{byte(id), byte(id >> 8)}, data...))
	}
	for u, data := range d.svcData {
		uu, err := ble.ParseUUID(u)
		if err != nil {
			continue
		}
		if s, ok := uu.Short(); ok && s <= 0xffff {
			add(0x16, append([]byte{byte(s), byte(s >> 8)}, data...))
		} else if ok {
			add(0x20, append([]byte{byte(s), byte(s >> 8), byte(s >> 16), byte(s >> 24)}, data...))
		} else {
			add(0x21, append(uuidLE(uu), data...))
		}
	}
	var u16, u128 []byte
	for _, u := range d.uuids {
		uu, err := ble.ParseUUID(u)
		if err != nil {
			continue
		}
		if s, ok := uu.Short(); ok && s <= 0xffff {
			u16 = append(u16, byte(s), byte(s>>8))
		} else {
			u128 = append(u128, uuidLE(uu)...)
		}
	}
	add(0x03, u16)
	add(0x07, u128)
	add(0x09, []byte(d.name))
	if d.txPower != nil {
		add(0x0a, []byte{byte(int8(*d.txPower))})
	}
	return out
}

// uuidLE is the little-endian byte order AD structures use for 128-bit UUIDs.
func uuidLE(u ble.UUID) []byte {
	b := make([]byte, 16)
	for i := 0; i < 8; i++ {
		b[i] = byte(u.Lo >> (8 * i))
		b[8+i] = byte(u.Hi >> (8 * i))
	}
	return b
}

func (a *Adapter) Connect(addr uint64, addrType uint32, ev ble.Events) (ble.Conn, error) {
	p := a.devicePath(addr)
	c := &conn{a: a, path: p, ev: ev, resolved: make(chan struct{}), lost: make(chan struct{}),
		notifying: map[uint16]bool{}, reading: map[uint16]int{}, held: map[uint16][][]byte{},
		echoes: map[uint16][]echo{}}
	// Before the connection is registered, so that whatever tears it down finds the rule in place.
	if err := a.watchDevice(p); err != nil {
		return nil, fmt.Errorf("match rule: %w", err)
	}
	c.watched = true
	a.mu.Lock()
	if _, busy := a.conns[p]; busy {
		a.mu.Unlock()
		a.unwatchDevice(p)
		return nil, errors.New("connection already in progress")
	}
	a.conns[p] = c
	_, known := a.devices[p]
	a.mu.Unlock()
	// Until the services are resolved or the connect fails: the link is most fragile in its first
	// connection events, and service discovery is a burst of traffic right after.
	defer a.pauseScan()()
	fail := func(err error) (ble.Conn, error) {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.release(a.call(p, ifDevice+".Disconnect").Err)
		return nil, err
	}

	// One deadline for every attempt, retries included.
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	log := a.log.With("addr", ble.FormatMAC(addr))
	var connectTook time.Duration
	retries, err := a.retry.Do(func() error {
		start := time.Now()
		err := a.connect(ctx, p, addr, addrType, known)
		var de dbus.Error
		if errors.As(err, &de) && (known && noObject(de.Name) || !known && de.Name == "org.bluez.Error.AlreadyExists") {
			// BlueZ removed the device or created it just now, before the signal saying so arrived.
			known = !known
			err = a.connect(ctx, p, addr, addrType, known)
		}
		connectTook = time.Since(start)
		if err != nil {
			return err
		}
		return c.awaitResolved()
	}, func(err error) bool {
		if !errors.Is(err, errLinkLost) && !(radioFailure(err) && connectTook <= retryFast) {
			var de dbus.Error
			errors.As(err, &de)
			log.Debug("connect failed, not a radio failure", "err", err, "name", de.Name)
			return false
		}
		// A link that failed in its first connection events was up for BlueZ for a moment: Connected
		// went true and back to false, which tore the connection down. Set it up again for the retry.
		select {
		case <-c.lostCh():
			if err := c.rearm(); err != nil {
				log.Debug("connect failed, cannot retry", "err", err)
				return false
			}
		default:
		}
		return true
	}, ev.Withdrawn, func(n int, err error) {
		log.Debug("connect failed on the radio, retrying", "retry", n, "err", err)
	})
	cancel()
	if err != nil {
		if retries > 0 {
			return fail(fmt.Errorf("connect, %d retries: %w", retries, err))
		}
		return fail(fmt.Errorf("connect: %w", err))
	}
	if retries > 0 {
		log.Info("connected after retries", "retries", retries)
	}

	if err := c.loadGATT(); err != nil {
		return fail(err)
	}
	// From here on a lost link is reported through ev.Disconnected; before, Connect fails instead.
	c.mu.Lock()
	lost := c.closed
	c.up = !lost
	c.mu.Unlock()
	if lost {
		return fail(errors.New("link lost while connecting"))
	}
	return c, nil
}

// awaitResolved waits for the services of a link that just came up. A link that dropped counts as lost even if
// the services resolved first, since they went with it.
func (c *conn) awaitResolved() error {
	v, err := c.a.getProperty(c.path, ifDevice+".ServicesResolved")
	if err == nil && v.Value() == true {
		c.markResolved()
	}
	lost := c.lostCh()
	select {
	case <-c.resolvedCh():
		select {
		case <-lost:
			return errLinkLost
		default:
			return nil
		}
	case <-lost:
		return errLinkLost
	case <-time.After(resolveTimeout):
		return errors.New("services not resolved in time")
	}
}

// connect connects a device BlueZ has an object for through Device1.Connect. For one it has none for, which
// is most devices once bluetoothd reports none (see Adapter.quiet), ConnectDevice (needs bluetoothd
// --experimental) creates and connects it.
func (a *Adapter) connect(ctx context.Context, p dbus.ObjectPath, addr uint64, addrType uint32, known bool) error {
	if known {
		return a.obj(p).CallWithContext(ctx, ifDevice+".Connect", 0).Err
	}
	t := "public"
	if addrType == ble.AddrRandom {
		t = "random"
	}
	return a.obj(a.path).CallWithContext(ctx, ifAdapter+".ConnectDevice", 0,
		map[string]any{"Address": ble.FormatMAC(addr), "AddressType": t}).Err
}

// noObject tells whether an error says that the object called does not exist, as libdbus (UnknownObject, or
// UnknownMethod from a parent's fallback) or godbus (NoSuchObject) put it.
func noObject(name string) bool {
	switch name {
	case "org.freedesktop.DBus.Error.UnknownObject", "org.freedesktop.DBus.Error.UnknownMethod",
		"org.freedesktop.DBus.Error.UnknownInterface", "org.freedesktop.DBus.Error.NoSuchObject":
		return true
	}
	return false
}

// conn is one connected device. GATT objects are looked up by the handle encoded in their D-Bus path
// (see charValueHandle for characteristics).
type conn struct {
	a    *Adapter
	path dbus.ObjectPath
	ev   ble.Events

	gattDB // set before Connect returns, read-only afterwards

	evMu sync.Mutex // serializes notification delivery, so held values do not overtake later ones

	mu        sync.Mutex
	up        bool // connected and reported to the caller
	closed    bool
	resolved  chan struct{} // closed when the services are resolved; replaced by rearm
	lost      chan struct{} // closed when the link goes down; replaced by rearm
	watched   bool          // the device's match rule is in place (see watchDevice)
	notifying map[uint16]bool
	reading   map[uint16]int      // ReadValue calls in flight per characteristic
	held      map[uint16][][]byte // Value changes seen while a read was in flight
	echoes    map[uint16][]echo   // read results whose Value change has not been seen yet
}

// echo is a value our own ReadValue stored in BlueZ's cache. BlueZ announces every read result with a Value
// PropertiesChanged, exactly like a notification, before it replies; the signal and the reply reach us on
// different goroutines, so it can be seen during the read or shortly after. The first Value change for the
// handle that matches is dropped. A real notification carrying the same value within readEchoWindow of a read
// whose echo already went by is not affected; one arriving before the echo is processed may be dropped
// instead of it, which loses nothing since the value is the same.
type echo struct {
	data    []byte
	expires time.Time
}

func (c *conn) markResolved() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.resolved:
	default:
		close(c.resolved)
	}
}

func (c *conn) resolvedCh() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolved
}

// unwatch drops the connection's match rule (see watchDevice) when it ends, whichever way.
func (c *conn) unwatch() {
	c.mu.Lock()
	w := c.watched
	c.watched = false
	c.mu.Unlock()
	if w {
		c.a.unwatchDevice(c.path)
	}
}

func (c *conn) lostCh() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lost
}

// rearm sets a connection that went down while connecting up for another attempt: the match rule, its
// place in a.conns and fresh channels.
func (c *conn) rearm() error {
	if err := c.a.watchDevice(c.path); err != nil {
		return fmt.Errorf("match rule: %w", err)
	}
	c.a.mu.Lock()
	if other, ok := c.a.conns[c.path]; ok && other != c {
		c.a.mu.Unlock()
		c.a.unwatchDevice(c.path)
		return errors.New("another connection took the device")
	}
	c.a.conns[c.path] = c
	c.a.mu.Unlock()
	c.mu.Lock()
	c.watched = true
	c.closed = false
	c.resolved = make(chan struct{})
	c.lost = make(chan struct{})
	c.mu.Unlock()
	return nil
}

func (c *conn) deviceChanged(changed map[string]dbus.Variant) {
	if v, ok := changed["ServicesResolved"]; ok && v.Value() == true {
		c.markResolved()
	}
	if v, ok := changed["Connected"]; ok && v.Value() == false {
		// Torn down before lost closes: a Connect being retried wakes on lost and sets the connection up
		// again at once (see rearm), and a teardown that ran after would undo that. The two locks are never
		// nested, here or elsewhere.
		c.forget()
		c.mu.Lock()
		select {
		case <-c.lost:
		default:
			close(c.lost)
		}
		report := c.up && !c.closed
		c.closed = true
		c.mu.Unlock()
		if report {
			c.ev.Disconnected(errors.New("link lost"))
		}
	}
}

// forget takes the connection off the adapter: its place in a.conns and its match rule.
func (c *conn) forget() {
	c.a.mu.Lock()
	if c.a.conns[c.path] == c {
		delete(c.a.conns, c.path)
	}
	c.a.mu.Unlock()
	c.unwatch()
}

// release is forget for a connection that asked BlueZ to disconnect, and got the answer err. BlueZ reports
// Connected=false before it answers, so the signal is queued, or already handled, when the answer arrives.
// Until dispatch has handled it, the connection keeps its place in a.conns: a new connection to the device
// registered meanwhile would get the signal and read it as its own link going down. Dispatch normally gets
// there within milliseconds; one that stays behind must not hold the device for good, so the wait is bounded.
// No wait when the call failed: there is no signal to come, or it was handled before the call.
func (c *conn) release(err error) {
	if err == nil {
		select {
		case <-c.lostCh():
		case <-time.After(disconnectWait):
			c.a.log.Warn("link not reported down after disconnect, letting go of the device", "path", c.path,
				"after", disconnectWait)
		}
	}
	c.forget()
}

func (c *conn) valueChanged(p dbus.ObjectPath, data []byte) {
	h, ok := charValueHandle(p)
	if !ok {
		return
	}
	data = append([]byte(nil), data...)
	c.evMu.Lock()
	defer c.evMu.Unlock()
	c.mu.Lock()
	if c.reading[h] > 0 { // may be the read's own result: decide when the read returns
		if len(c.held[h]) >= maxHeldValue {
			c.held[h] = c.held[h][1:] // the newest values matter most
		}
		c.held[h] = append(c.held[h], data)
		c.mu.Unlock()
		return
	}
	forward := !c.takeEcho(h, data) && c.notifying[h]
	c.mu.Unlock()
	if forward {
		c.ev.Notification(h, data)
	}
}

func (c *conn) readStarted(h uint16) {
	c.mu.Lock()
	c.reading[h]++
	c.mu.Unlock()
}

// readDone records a read result as an expected echo and, once no read is in flight on the handle, forwards
// the held Value changes that were not echoes.
func (c *conn) readDone(h uint16, data []byte, ok bool) {
	c.evMu.Lock()
	defer c.evMu.Unlock()
	c.mu.Lock()
	c.reading[h]--
	if ok {
		c.echoes[h] = append(c.echoes[h], echo{data: data, expires: time.Now().Add(readEchoWindow)})
	}
	var out [][]byte
	if c.reading[h] == 0 {
		for _, v := range c.held[h] {
			if !c.takeEcho(h, v) && c.notifying[h] {
				out = append(out, v)
			}
		}
		delete(c.held, h)
		delete(c.reading, h)
	}
	c.mu.Unlock()
	for _, v := range out {
		c.ev.Notification(h, v)
	}
}

// takeEcho drops expired echoes and consumes the first one equal to data. Called with c.mu held.
func (c *conn) takeEcho(h uint16, data []byte) bool {
	now := time.Now()
	es := c.echoes[h][:0]
	found := false
	for _, e := range c.echoes[h] {
		switch {
		case now.After(e.expires):
		case !found && bytes.Equal(e.data, data):
			found = true
		default:
			es = append(es, e)
		}
	}
	if len(es) == 0 {
		delete(c.echoes, h)
	} else {
		c.echoes[h] = es
	}
	return found
}

// handleOf reads the attribute handle from the last path element (service000a, char000b, desc000d).
func handleOf(p dbus.ObjectPath) (uint16, bool) {
	s := string(p)
	last := s[strings.LastIndex(s, "/")+1:]
	for _, prefix := range []string{"service", "char", "desc"} {
		if strings.HasPrefix(last, prefix) {
			v, err := strconv.ParseUint(last[len(prefix):], 16, 16)
			return uint16(v), err == nil
		}
	}
	return 0, false
}

var charFlags = map[string]uint32{
	"broadcast": 0x01, "read": 0x02, "write-without-response": 0x04, "write": 0x08, "notify": 0x10,
	"indicate": 0x20, "authenticated-signed-writes": 0x40, "extended-properties": 0x80,
}

// loadGATT reads BlueZ's object tree into the connection's GATT database.
func (c *conn) loadGATT() error {
	objs, err := c.a.managedObjects()
	if err != nil {
		return fmt.Errorf("gatt objects: %w", err)
	}
	c.gattDB = buildGATT(c.path, objs)
	return nil
}

// gattDB is the remote GATT database of a connection, with the D-Bus object behind every handle.
type gattDB struct {
	mtu       uint32
	services  []ble.Service
	chars     map[uint16]dbus.ObjectPath // by value handle
	descs     map[uint16]dbus.ObjectPath
	cccdOwner map[uint16]uint16 // CCCD descriptor handle -> characteristic value handle
}

// charValueHandle maps a characteristic object path to its value handle. BlueZ names the object after the
// characteristic declaration (char000b), while ESPHome reports the value handle that follows it (0x000c).
// Home Assistant keeps one GATT cache per device across proxies, so the handles must match an ESP32's.
func charValueHandle(p dbus.ObjectPath) (uint16, bool) {
	h, ok := handleOf(p)
	return h + 1, ok
}

// buildGATT builds the service list and handle maps from the objects under a device path.
func buildGATT(dev dbus.ObjectPath, objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) gattDB {
	prefix := string(dev) + "/"
	svcs := map[dbus.ObjectPath]*ble.Service{}
	type charEntry struct {
		svc dbus.ObjectPath
		ch  ble.Characteristic
	}
	chars := map[dbus.ObjectPath]*charEntry{}
	db := gattDB{mtu: 23, chars: map[uint16]dbus.ObjectPath{}, descs: map[uint16]dbus.ObjectPath{},
		cccdOwner: map[uint16]uint16{}}
	uuidOf := func(props map[string]dbus.Variant) ble.UUID {
		s, _ := props["UUID"].Value().(string)
		u, _ := ble.ParseUUID(s)
		return u
	}
	for p, ifs := range objs {
		if !strings.HasPrefix(string(p), prefix) {
			continue
		}
		if props, ok := ifs[ifService]; ok {
			if h, ok := handleOf(p); ok {
				svcs[p] = &ble.Service{UUID: uuidOf(props), Handle: h}
			}
		}
		if props, ok := ifs[ifChar]; ok {
			h, ok := charValueHandle(p)
			if !ok {
				continue
			}
			ch := ble.Characteristic{UUID: uuidOf(props), Handle: h}
			flags, _ := props["Flags"].Value().([]string)
			for _, f := range flags {
				ch.Properties |= charFlags[f]
			}
			if m, ok := props["MTU"].Value().(uint16); ok && uint32(m) > db.mtu {
				db.mtu = uint32(m)
			}
			svc, _ := props["Service"].Value().(dbus.ObjectPath)
			chars[p] = &charEntry{svc: svc, ch: ch}
			db.chars[h] = p
		}
	}
	for p, ifs := range objs {
		props, ok := ifs[ifDesc]
		if !ok || !strings.HasPrefix(string(p), prefix) {
			continue
		}
		h, ok := handleOf(p)
		if !ok {
			continue
		}
		chPath, _ := props["Characteristic"].Value().(dbus.ObjectPath)
		ce := chars[chPath]
		if ce == nil {
			continue
		}
		d := ble.Descriptor{UUID: uuidOf(props), Handle: h}
		ce.ch.Descriptors = append(ce.ch.Descriptors, d)
		db.descs[h] = p
		if s, ok := d.UUID.Short(); ok && s == 0x2902 {
			db.cccdOwner[h] = ce.ch.Handle
		}
	}
	for _, ce := range chars {
		if s := svcs[ce.svc]; s != nil {
			sortDescs(ce.ch.Descriptors)
			s.Characteristics = append(s.Characteristics, ce.ch)
		}
	}
	for _, s := range svcs {
		sortChars(s.Characteristics)
		db.services = append(db.services, *s)
	}
	sortServices(db.services)
	return db
}

func (c *conn) MTU() uint32 { return c.mtu }

func (c *conn) Services() ([]ble.Service, error) { return c.services, nil }

func (c *conn) call(p dbus.ObjectPath, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), gattCallTimeout)
	defer cancel()
	return c.a.obj(p).CallWithContext(ctx, method, 0, args...)
}

func (c *conn) charPath(h uint16) (dbus.ObjectPath, error) {
	if p, ok := c.chars[h]; ok {
		return p, nil
	}
	return "", fmt.Errorf("no characteristic with handle 0x%04x", h)
}

func (c *conn) descPath(h uint16) (dbus.ObjectPath, error) {
	if p, ok := c.descs[h]; ok {
		return p, nil
	}
	return "", fmt.Errorf("no descriptor with handle 0x%04x", h)
}

func (c *conn) Read(h uint16) ([]byte, error) {
	p, err := c.charPath(h)
	if err != nil {
		return nil, err
	}
	var data []byte
	c.readStarted(h)
	err = c.call(p, ifChar+".ReadValue", map[string]any{}).Store(&data)
	c.readDone(h, data, err == nil)
	return data, err
}

func (c *conn) Write(h uint16, data []byte, withResponse bool) error {
	p, err := c.charPath(h)
	if err != nil {
		return err
	}
	typ := "command"
	if withResponse {
		typ = "request"
	}
	return c.call(p, ifChar+".WriteValue", data, map[string]any{"type": typ}).Err
}

func (c *conn) ReadDescriptor(h uint16) ([]byte, error) {
	p, err := c.descPath(h)
	if err != nil {
		return nil, err
	}
	var data []byte
	err = c.call(p, ifDesc+".ReadValue", map[string]any{}).Store(&data)
	return data, err
}

// WriteDescriptor writes a descriptor. BlueZ owns the CCCD (it refuses client writes), so a CCCD write turns
// notifications on or off through StartNotify/StopNotify instead.
func (c *conn) WriteDescriptor(h uint16, data []byte) error {
	if ch, ok := c.cccdOwner[h]; ok {
		return c.Notify(ch, len(data) > 0 && data[0]&0x03 != 0)
	}
	p, err := c.descPath(h)
	if err != nil {
		return err
	}
	return c.call(p, ifDesc+".WriteValue", data, map[string]any{}).Err
}

func (c *conn) Notify(h uint16, enable bool) error {
	p, err := c.charPath(h)
	if err != nil {
		return err
	}
	// Set before StartNotify is called: the reply and the first Value change reach us on different goroutines,
	// and valueChanged drops the value if it comes first. Undone if the call fails. StopNotify clears it after
	// the reply, so values sent until then still go out.
	c.mu.Lock()
	already := c.notifying[h] == enable
	if enable {
		c.notifying[h] = true
	}
	c.mu.Unlock()
	if already {
		return nil
	}
	method := ifChar + ".StopNotify"
	if enable {
		method = ifChar + ".StartNotify"
	}
	err = c.call(p, method).Err
	if enable && err != nil || !enable && err == nil {
		c.mu.Lock()
		delete(c.notifying, h)
		c.mu.Unlock()
	}
	return err
}

func (c *conn) Disconnect() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	start := time.Now()
	err := c.call(c.path, ifDevice+".Disconnect").Err
	took := time.Since(start)
	c.release(err)
	if took > 5*time.Second {
		c.a.log.Warn("slow disconnect", "path", c.path, "took", took.Round(time.Millisecond))
	}
	// BlueZ answers a disconnect within the supervision timeout. No answer has been the first sign of a hung
	// bluetoothd, so the supervisor counts it toward a restart and checks bluetoothd at once.
	if isTimeout(err) {
		c.a.unanswered.Add(1)
		c.a.kickScan()
	}
	return err
}
