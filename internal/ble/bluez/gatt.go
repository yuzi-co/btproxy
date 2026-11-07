//go:build linux

package bluez

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// GATT operations on a connection: reads, writes, notifications, and the Value changes BlueZ sends for both,
// since it announces a read's result as a Value change too (see echo).

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
