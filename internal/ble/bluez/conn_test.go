//go:build linux

package bluez

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/bletest"
)

// newTestConn is a connection to fakeDevice on the adapter, registered with it as Connect would, with one
// characteristic, fakeChar.
func newTestConn(t *testing.T, a *Adapter, ev ble.Events) *conn {
	t.Helper()
	c := &conn{a: a, path: fakeDevice, ev: ev, resolved: make(chan struct{}), lost: make(chan struct{}),
		notifying: map[uint16]bool{}, reading: map[uint16]int{}, held: map[uint16][][]byte{},
		echoes: map[uint16][]echo{}}
	c.chars = map[uint16]dbus.ObjectPath{fakeCharHandle: fakeChar}
	if err := c.rearm(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a.mu.Lock()
		if a.conns[fakeDevice] == c {
			delete(a.conns, fakeDevice)
		}
		a.mu.Unlock()
		c.unwatch()
	})
	return c
}

func connOf(a *Adapter, p dbus.ObjectPath) *conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conns[p]
}

func (c *conn) isWatched() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watched
}

func (c *conn) isNotifying(h uint16) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notifying[h]
}

// stallDispatch holds the adapter's signal dispatch on an advertisement until the returned function is called,
// as a scan callback that does not return would. Signals sent meanwhile queue up behind it.
func stallDispatch(t *testing.T, a *Adapter, bz *fakeBlueZ) (resume func()) {
	t.Helper()
	gate, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var armed atomic.Bool
	stop, _ := a.Scan(func(ble.Advert) {
		if armed.Load() {
			once.Do(func() { close(entered) })
			<-gate
		}
	})
	bletest.WaitFor(t, "running", func() bool { return a.ScanStatus().State == ble.ScanRunning })
	armed.Store(true)
	bz.advertise()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no advertisement delivered")
	}
	var resumeOnce sync.Once
	return func() {
		resumeOnce.Do(func() {
			close(gate)
			stop()
		})
	}
}

func connectTo(t *testing.T, a *Adapter, ev ble.Events) *conn {
	t.Helper()
	c, err := a.Connect(0xAABBCCDDEEFF, ble.AddrPublic, ev)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*conn)
}

// TestDisconnectWaitsForTheLinkDownSignal: BlueZ reports Connected=false before it answers Disconnect, and
// dispatch may be behind. Disconnect keeps the device until the signal is handled, so it cannot reach the next
// connection to the device and take it down.
func TestDisconnectWaitsForTheLinkDownSignal(t *testing.T) {
	a, bz := openForRetries(t, 0, 0)
	c := connectTo(t, a, retryEvents)
	resume := stallDispatch(t, a, bz)
	defer resume()

	errc := make(chan error, 1)
	go func() { errc <- c.Disconnect() }()
	bletest.WaitFor(t, "Disconnect call", func() bool { return bz.disconnectCalls() == 1 })
	select {
	case err := <-errc:
		t.Fatalf("Disconnect returned (%v) before its Connected=false signal was handled", err)
	case <-time.After(200 * time.Millisecond):
	}
	if connOf(a, fakeDevice) != c {
		t.Fatal("device let go of before the Connected=false signal was handled")
	}
	resume()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if connOf(a, fakeDevice) != nil || c.isWatched() {
		t.Fatal("connection still registered after Disconnect")
	}

	dropped := make(chan error, 1)
	ev := retryEvents
	ev.Disconnected = func(err error) { dropped <- err }
	c2 := connectTo(t, a, ev)
	defer c2.Disconnect()
	flush(t, a, bz)
	select {
	case err := <-dropped:
		t.Fatalf("the next connection was reported lost: %v", err)
	default:
	}
}

// TestDisconnectGivesUpWaiting: a dispatch that stays behind must not hold Disconnect, or the device, for good.
func TestDisconnectGivesUpWaiting(t *testing.T) {
	a, bz := openForRetries(t, 0, 0)
	a.tm.disconnectWait = 200 * time.Millisecond
	c := connectTo(t, a, retryEvents)
	resume := stallDispatch(t, a, bz)
	defer resume()

	start := time.Now()
	if err := c.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < a.tm.disconnectWait || took > a.tm.disconnectWait+time.Second {
		t.Fatalf("Disconnect took %v, want about %v", took, a.tm.disconnectWait)
	}
	if connOf(a, fakeDevice) != nil || c.isWatched() {
		t.Fatal("connection still registered after Disconnect gave up waiting")
	}
	resume()
	flush(t, a, bz) // the late signal finds no connection
}

// TestUnpairDropsSilently: RemoveDevice takes the link down, and BlueZ reports that like any other loss. The
// caller asked for it, so it is not reported through Events.Disconnected (see ble.Adapter.Unpair).
func TestUnpairDropsSilently(t *testing.T) {
	a, bz := openForRetries(t, 0, 0)
	dropped := make(chan error, 1)
	ev := retryEvents
	ev.Disconnected = func(err error) { dropped <- err }
	c := connectTo(t, a, ev)
	if err := a.Unpair(0xAABBCCDDEEFF); err != nil {
		t.Fatal(err)
	}
	flush(t, a, bz)
	select {
	case err := <-dropped:
		t.Fatalf("the link going down was reported: %v", err)
	default:
	}
	if connOf(a, fakeDevice) != nil || c.isWatched() {
		t.Fatal("connection still registered after Unpair")
	}
	if err := a.Unpair(0xAABBCCDDEEFF); err != nil {
		t.Fatalf("unpairing a device BlueZ does not know: %v", err)
	}
}

// TestLostLinkIsTornDownBeforeTheConnectWakes: a Connect that is retried sets the connection up again as soon
// as lost closes, so the teardown of the lost link (its place on the adapter, its match rule) must be complete
// by then, or it undoes the retry's setup. The teardown needs the adapter's lock; holding it must hold lost.
func TestLostLinkIsTornDownBeforeTheConnectWakes(t *testing.T) {
	a, _ := openForRetries(t, 0, 0)
	c := newTestConn(t, a, retryEvents)
	lost := c.lostCh()
	a.mu.Lock()
	done := make(chan struct{})
	go func() {
		c.deviceChanged(map[string]dbus.Variant{"Connected": dbus.MakeVariant(false)})
		close(done)
	}()
	select {
	case <-lost:
		a.mu.Unlock()
		t.Fatal("lost closed before the connection was taken off the adapter")
	case <-time.After(100 * time.Millisecond):
	}
	a.mu.Unlock()
	<-done
	select {
	case <-lost:
	default:
		t.Fatal("lost not closed")
	}
	if connOf(a, fakeDevice) != nil || c.isWatched() {
		t.Fatal("connection still registered after the link was lost")
	}
	if err := c.rearm(); err != nil {
		t.Fatal(err)
	}
	if connOf(a, fakeDevice) != c || !c.isWatched() {
		t.Fatal("rearm did not set the connection up again")
	}
}
