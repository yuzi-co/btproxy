//go:build linux

package bluez

import (
	"context"
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"
)

// Which signals the bus sends us. During discovery bluetoothd reports every advertisement as a Device1
// PropertiesChanged signal (RSSI, ManufacturerData, ServiceData), dozens a second in a busy place, and
// decoding one costs more than the rest of the proxy's work on it. With raw advertisements from the HCI
// monitor those signals are not needed, so once the monitor has delivered, the rule for every device's
// properties is dropped. What stays: objects coming and going (the device cache, the adapter, bluetoothd
// restarts), the adapter's properties, characteristic values (notifications), and the properties of each
// device the proxy connects to, through a rule of its own for the life of the connection.

// propsMatch is a PropertiesChanged rule for one interface; where is the path or path namespace.
func propsMatch(iface string, where dbus.MatchOption) []dbus.MatchOption {
	return []dbus.MatchOption{dbus.WithMatchSender(busName), dbus.WithMatchInterface(ifProps),
		dbus.WithMatchMember("PropertiesChanged"), dbus.WithMatchArg(0, iface), where}
}

// addMatches subscribes to the signals the proxy always follows. The rule for every device's properties is
// syncMatches' business.
func (a *Adapter) addMatches() error {
	for _, m := range [][]dbus.MatchOption{
		{dbus.WithMatchSender(busName), dbus.WithMatchInterface(ifObjManager)},
		propsMatch(ifAdapter, dbus.WithMatchObjectPath(a.path)),
		propsMatch(ifChar, dbus.WithMatchPathNamespace(a.path)),
	} {
		if err := a.matchCall(true, m); err != nil {
			return err
		}
	}
	return nil
}

// matchCall adds or removes a match rule. The bus daemon answers at once; the timeout only guards against a
// stuck one.
func (a *Adapter) matchCall(add bool, m []dbus.MatchOption) error {
	ctx, cancel := context.WithTimeout(context.Background(), a.tm.callTimeout)
	defer cancel()
	if add {
		return a.bus.AddMatchSignalContext(ctx, m...)
	}
	return a.bus.RemoveMatchSignalContext(ctx, m...)
}

// syncMatches subscribes to every device's properties unless raw advertisements have proven themselves, and
// unsubscribes once they have. Until the monitor delivers, BlueZ's advertisements are what rawHandles
// compares it against, and after a fallback they are the advertisements. Run by the supervisor and by Open
// before the supervisor starts.
func (a *Adapter) syncMatches() error {
	a.mu.Lock()
	want, have, fallback := !a.rawProven(), a.allDevices, a.rawSeen
	a.mu.Unlock()
	if want == have {
		return nil
	}
	if err := a.matchCall(want, propsMatch(ifDevice, dbus.WithMatchPathNamespace(a.path))); err != nil {
		return fmt.Errorf("match rule for every device: %w", err)
	}
	a.mu.Lock()
	a.allDevices = want
	a.mu.Unlock()
	if !want {
		a.log.Info("raw advertisements flow, following the properties of connected devices only")
	} else if fallback {
		a.log.Info("following every device's properties again")
	}
	return nil
}

// watchDevice subscribes to a device's properties for a connection, which needs Connected, ServicesResolved
// and Paired whatever syncMatches does. The bus daemon counts identical rules, so every watchDevice that
// succeeds is undone by exactly one unwatchDevice.
func (a *Adapter) watchDevice(p dbus.ObjectPath) error {
	return a.matchCall(true, propsMatch(ifDevice, dbus.WithMatchObjectPath(p)))
}

func (a *Adapter) unwatchDevice(p dbus.ObjectPath) {
	if err := a.matchCall(false, propsMatch(ifDevice, dbus.WithMatchObjectPath(p))); err != nil {
		a.log.Warn("D-Bus match rule", "device", p, "err", err)
	}
}

// Queue limits. Past sigSoftLimit queued signals, advertisement updates are dropped, as the next one
// replaces them; past sigHardLimit, characteristic values too. Signals that change state (objects, the
// adapter, connections) are always queued: they come at the pace of connections and devices appearing.
const (
	sigSoftLimit = 256
	sigHardLimit = 4096
)

// sigQueue is the connection's signal handler, in place of godbus's default one. That one hands each signal
// to a goroutine of its own when the channel is full, so a dispatch that falls behind grows goroutines
// without bound and sees signals out of order. This one keeps them in order in a queue that dispatch empties
// in batches, and drops what is safe to drop when it falls far behind. DeliverSignal runs on the
// connection's reader, which method replies come through too, so it never blocks.
type sigQueue struct {
	mu      sync.Mutex
	q       []*dbus.Signal
	dropped int
	closed  bool
	ready   chan struct{} // holds a token while the queue may be non-empty or is closed
}

func newSigQueue() *sigQueue { return &sigQueue{ready: make(chan struct{}, 1)} }

func (s *sigQueue) DeliverSignal(_, _ string, sig *dbus.Signal) {
	s.mu.Lock()
	switch {
	case s.closed:
	case len(s.q) >= sigSoftLimit && advertOnly(sig), len(s.q) >= sigHardLimit && expendable(sig):
		s.dropped++
	default:
		s.q = append(s.q, sig)
	}
	s.mu.Unlock()
	s.wake()
}

// Terminate is called when the connection closes; take then reports the end once the queue is empty.
func (s *sigQueue) Terminate() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.wake()
}

func (s *sigQueue) wake() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// take waits for signals and returns all queued ones, reusing buf, and how many were dropped since the last
// call. ok is false once the connection is closed and everything was taken.
func (s *sigQueue) take(buf []*dbus.Signal) (sigs []*dbus.Signal, dropped int, ok bool) {
	for {
		s.mu.Lock()
		if len(s.q) > 0 {
			sigs, s.q = s.q, buf[:0]
			dropped, s.dropped = s.dropped, 0
			s.mu.Unlock()
			return sigs, dropped, true
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return nil, 0, false
		}
		<-s.ready
	}
}

// advertOnly tells whether a signal only updates a device's advertisement.
func advertOnly(s *dbus.Signal) bool {
	if s.Name != ifProps+".PropertiesChanged" || len(s.Body) < 2 {
		return false
	}
	if iface, _ := s.Body[0].(string); iface != ifDevice {
		return false
	}
	changed, _ := s.Body[1].(map[string]dbus.Variant)
	for k := range changed {
		switch k {
		case "RSSI", "ManufacturerData", "ServiceData", "TxPower", "AdvertisingFlags", "AdvertisingData":
		default:
			return false
		}
	}
	return true
}

// expendable tells whether a signal is an advertisement update or a characteristic value (a notification or
// a read result).
func expendable(s *dbus.Signal) bool {
	if advertOnly(s) {
		return true
	}
	if s.Name != ifProps+".PropertiesChanged" || len(s.Body) < 1 {
		return false
	}
	iface, _ := s.Body[0].(string)
	return iface == ifChar
}
