// Package fake is an in-memory Bluetooth adapter for testing the API side without a radio. It advertises one
// device, FakeAddr, with a battery service (read) and a test service whose characteristic echoes writes and
// notifies a counter once a second. Lose and Restore simulate the Bluetooth stack going away and coming back.
package fake

import (
	"errors"
	"sync"
	"time"

	"btproxy/internal/ble"
)

const (
	AdapterAddr = 0x00_11_22_33_44_55
	FakeAddr    = 0xAA_BB_CC_DD_EE_01
)

// Handles of the fake GATT database.
const (
	HandleBattery = 0x0003
	HandleEcho    = 0x0013
	HandleCCCD    = 0x0014
)

var services = []ble.Service{
	{UUID: short(0x180f), Handle: 0x0001, Characteristics: []ble.Characteristic{
		{UUID: short(0x2a19), Handle: HandleBattery, Properties: 0x02}, // read
	}},
	{UUID: must("6e400001-b5a3-f393-e0a9-e50e24dcca9e"), Handle: 0x0010, Characteristics: []ble.Characteristic{
		// read, write without response, write, notify
		{UUID: must("6e400002-b5a3-f393-e0a9-e50e24dcca9e"), Handle: HandleEcho, Properties: 0x02 | 0x04 | 0x08 | 0x10,
			Descriptors: []ble.Descriptor{{UUID: short(0x2902), Handle: HandleCCCD}}},
	}},
}

func short(v uint32) ble.UUID { return ble.UUID{Hi: uint64(v)<<32 | 0x1000, Lo: 0x800000805f9b34fb} }

func must(s string) ble.UUID {
	u, err := ble.ParseUUID(s)
	if err != nil {
		panic(err)
	}
	return u
}

// Adapter is the fake controller; the zero value is ready to use.
type Adapter struct {
	Addr uint64 // controller address; 0 means AdapterAddr

	mu       sync.Mutex
	down     bool
	scanning bool
	mode     ble.ScanMode
	statusFn func(ble.ScanStatus)
	conns    map[*conn]bool
}

func (a *Adapter) MAC() uint64 {
	if a.Addr != 0 {
		return a.Addr
	}
	return AdapterAddr
}

func (a *Adapter) SetScanMode(m ble.ScanMode) {
	a.mu.Lock()
	a.mode = m
	a.mu.Unlock()
	a.changed()
}

func (a *Adapter) ScanStatus() ble.ScanStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := ble.ScanStatus{State: ble.ScanIdle, Mode: a.mode}
	switch {
	case a.scanning && a.down:
		st.State = ble.ScanFailed
	case a.scanning:
		st.State = ble.ScanRunning
	}
	return st
}

func (a *Adapter) OnScanStatus(fn func(ble.ScanStatus)) {
	a.mu.Lock()
	a.statusFn = fn
	a.mu.Unlock()
}

func (a *Adapter) changed() {
	a.mu.Lock()
	fn := a.statusFn
	a.mu.Unlock()
	if fn != nil {
		fn(a.ScanStatus())
	}
}

// Lose simulates bluetoothd or the controller going away: scanning fails and connections drop.
func (a *Adapter) Lose() {
	a.mu.Lock()
	a.down = true
	conns := a.conns
	a.conns = nil
	a.mu.Unlock()
	for c := range conns {
		if c.Disconnect() == nil {
			c.ev.Disconnected(errors.New("adapter lost"))
		}
	}
	a.changed()
}

// Restore brings the adapter back after Lose.
func (a *Adapter) Restore() {
	a.mu.Lock()
	a.down = false
	a.mu.Unlock()
	a.changed()
}

func (a *Adapter) Scan(fn func(ble.Advert)) (func(), error) {
	a.mu.Lock()
	a.scanning = true
	a.mu.Unlock()
	a.changed()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		// flags, complete name "fake", manufacturer 0xffff data 01 02
		data := []byte{0x02, 0x01, 0x06, 0x05, 0x09, 'f', 'a', 'k', 'e', 0x05, 0xff, 0xff, 0xff, 0x01, 0x02}
		for {
			select {
			case <-done:
				return
			case <-t.C:
				a.mu.Lock()
				down := a.down
				a.mu.Unlock()
				if !down {
					fn(ble.Advert{Addr: FakeAddr, AddrType: ble.AddrRandom, RSSI: -60, Data: data})
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			a.mu.Lock()
			a.scanning = false
			a.mu.Unlock()
			a.changed()
		})
	}, nil
}

func (a *Adapter) Connect(addr uint64, _ uint32, ev ble.Events) (ble.Conn, error) {
	a.mu.Lock()
	down := a.down
	a.mu.Unlock()
	if down {
		return nil, errors.New("adapter down")
	}
	if addr != FakeAddr {
		time.Sleep(time.Second)
		return nil, errors.New("device not found")
	}
	c := &conn{ev: ev, value: []byte("hello"), done: make(chan struct{})}
	live.Lock()
	live.conn = c
	live.Unlock()
	a.mu.Lock()
	if a.conns == nil {
		a.conns = map[*conn]bool{}
	}
	a.conns[c] = true
	a.mu.Unlock()
	return c, nil
}

func (*Adapter) ClearCache(uint64) error { return nil }

// Unpair drops the connection to FakeAddr, as BlueZ does when it removes a device. Other addresses have no
// bond to remove, which succeeds, as with the bluez backend.
func (*Adapter) Unpair(addr uint64) error {
	if addr != FakeAddr {
		return nil
	}
	live.Lock()
	c := live.conn
	live.conn = nil
	live.Unlock()
	if c != nil {
		c.Disconnect()
	}
	return nil
}

var _ ble.ConnParamsSetter = (*Adapter)(nil)

// SetConnParams accepts parameters within the ranges of the Bluetooth Core spec for a live connection to
// FakeAddr, and refuses others as a controller would.
func (*Adapter) SetConnParams(addr uint64, p ble.ConnParams) error {
	live.Lock()
	c := live.conn
	live.Unlock()
	if addr != FakeAddr || c == nil {
		return ble.ErrNotConnected
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ble.ErrNotConnected
	}
	if p.MinInterval < 6 || p.MaxInterval > 3200 || p.MinInterval > p.MaxInterval || p.Latency > 499 ||
		p.Timeout < 10 || p.Timeout > 3200 || uint32(p.Timeout)*4 <= (1+uint32(p.Latency))*uint32(p.MaxInterval) {
		return errors.New("invalid connection parameters")
	}
	return nil
}

// live is the latest connection to FakeAddr, for Unpair and SetConnParams.
var live struct {
	sync.Mutex
	conn *conn
}

type conn struct {
	ev ble.Events

	mu        sync.Mutex
	value     []byte
	notifying bool
	done      chan struct{}
	closed    bool
}

func (c *conn) MTU() uint32 { return 247 }

func (c *conn) Services() ([]ble.Service, error) { return services, nil }

func (c *conn) Read(h uint16) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch h {
	case HandleBattery:
		return []byte{87}, nil
	case HandleEcho:
		return append([]byte(nil), c.value...), nil
	}
	return nil, errors.New("not readable")
}

func (c *conn) Write(h uint16, data []byte, _ bool) error {
	if h != HandleEcho {
		return errors.New("not writable")
	}
	c.mu.Lock()
	c.value = append([]byte(nil), data...)
	c.mu.Unlock()
	return nil
}

func (c *conn) ReadDescriptor(h uint16) ([]byte, error) {
	if h != HandleCCCD {
		return nil, errors.New("no such descriptor")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.notifying {
		return []byte{1, 0}, nil
	}
	return []byte{0, 0}, nil
}

func (c *conn) WriteDescriptor(h uint16, _ []byte) error {
	if h != HandleCCCD {
		return errors.New("no such descriptor")
	}
	return nil
}

func (c *conn) Notify(h uint16, enable bool) error {
	if h != HandleEcho {
		return errors.New("cannot notify")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if enable && !c.notifying {
		go c.notifyLoop()
	}
	c.notifying = enable
	return nil
}

func (c *conn) notifyLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for n := byte(0); ; n++ {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		c.mu.Lock()
		on := c.notifying
		c.mu.Unlock()
		if !on {
			return
		}
		c.ev.Notification(HandleEcho, []byte{n})
	}
}

func (c *conn) Pair() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ble.ErrNotConnected
	}
	return nil
}

func (c *conn) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ble.ErrNotConnected
	}
	c.closed = true
	close(c.done)
	return nil
}
