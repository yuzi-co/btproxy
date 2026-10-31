// Package fake is an in-memory Bluetooth adapter for testing the API side without a radio. It advertises one
// device, FakeAddr, with a battery service (read) and a test service whose characteristic echoes writes and
// notifies a counter once a second.
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

type Adapter struct{}

func (Adapter) MAC() uint64 { return AdapterAddr }

func (Adapter) Scan(fn func(ble.Advert)) (func(), error) {
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
				fn(ble.Advert{Addr: FakeAddr, AddrType: ble.AddrRandom, RSSI: -60, Data: data})
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }, nil
}

func (Adapter) Connect(addr uint64, _ uint32, ev ble.Events) (ble.Conn, error) {
	if addr != FakeAddr {
		time.Sleep(time.Second)
		return nil, errors.New("device not found")
	}
	return &conn{ev: ev, value: []byte("hello"), done: make(chan struct{})}, nil
}

func (Adapter) ClearCache(uint64) error { return nil }

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

func (c *conn) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	return nil
}
