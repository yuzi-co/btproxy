//go:build linux

package bluez

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// A connection's life: the conn type and its state, the link going down, teardown (forget, release, rearm for
// a retry) and Disconnect. GATT traffic on the connection is in gatt.go, the database it works on in gattdb.go.

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
			c.ev.Disconnected(ble.ErrLinkLost)
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
		case <-time.After(c.a.tm.disconnectWait):
			c.a.log.Warn("link not reported down after disconnect, letting go of the device", "path", c.path,
				"after", c.a.tm.disconnectWait)
		}
	}
	c.forget()
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
