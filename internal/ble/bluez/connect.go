//go:build linux

package bluez

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// Connect: setting a connection up, the D-Bus calls that make the link, the retries (see retry.go) and the
// wait for the services to be resolved. What happens to the connection afterwards is in conn.go.

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
	defer a.pause.Begin()()
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
	err := a.retry.Run(log, func() error {
		err := a.connect(ctx, p, addr, addrType, known)
		var de dbus.Error
		if errors.As(err, &de) && (known && noObject(de.Name) || !known && de.Name == "org.bluez.Error.AlreadyExists") {
			// BlueZ removed the device or created it just now, before the signal saying so arrived.
			known = !known
			err = a.connect(ctx, p, addr, addrType, known)
		}
		if err != nil {
			return err
		}
		return c.awaitResolved()
	}, func(err error) bool {
		if !retryable(err) {
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
	}, ev.Withdrawn)
	cancel()
	if err != nil {
		return fail(err)
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
		return fail(fmt.Errorf("%w while connecting", ble.ErrLinkLost))
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
