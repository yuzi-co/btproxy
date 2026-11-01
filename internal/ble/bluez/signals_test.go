//go:build linux

package bluez

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// rssiOf is the RSSI the device cache holds for a device; ok is false if it has no entry.
func rssiOf(a *Adapter, p dbus.ObjectPath) (rssi int16, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d := a.devices[p]; d != nil {
		return d.rssi, true
	}
	return 0, false
}

var flushes atomic.Uint32

// flush waits until the adapter has handled every signal the fake sent so far: it announces a new device
// and waits for it, which the bus and the queue deliver after the earlier signals.
func flush(t *testing.T, a *Adapter, bz *fakeBlueZ) {
	t.Helper()
	n := flushes.Add(1)
	p := dbus.ObjectPath(fmt.Sprintf("%s/dev_00_00_00_00_%02X_%02X", fakeAdapter, byte(n>>8), byte(n)))
	bz.advertiseFrom(p, -90)
	waitFor(t, "flush", func() bool { _, ok := rssiOf(a, p); return ok })
}

func allDevices(a *Adapter) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.allDevices
}

// expectRSSI checks that the device's cached RSSI is want once the signals sent so far are handled.
func expectRSSI(t *testing.T, a *Adapter, bz *fakeBlueZ, want int16, why string) {
	t.Helper()
	flush(t, a, bz)
	if r, _ := rssiOf(a, fakeDevice); r != want {
		t.Fatalf("%s: RSSI %d, want %d", why, r, want)
	}
}

// TestNarrowMatches: once the monitor delivers, the properties of devices that are not connected stop
// coming, while a connection gets them through a rule of its own that every way of ending it removes:
// Disconnect, a failed connect, and bluetoothd exiting.
func TestNarrowMatches(t *testing.T) {
	src := newFakeRaw()
	useRaw(t, src)
	a, bz, _ := openRawTest(t, RawOn, false)
	bz.advertise()
	bz.advertiseFrom(fakeDevice, -61)
	expectRSSI(t, a, bz, -61, "before the monitor delivered")

	src.ads <- ble.Advert{Addr: 1, RSSI: -50}
	waitFor(t, "rule for every device dropped", func() bool { return !allDevices(a) })
	bz.advertiseFrom(fakeDevice, -62)
	expectRSSI(t, a, bz, -61, "not connected")

	disconnected := make(chan error, 1)
	ev := ble.Events{Notification: func(uint16, []byte) {}, Disconnected: func(err error) { disconnected <- err }}
	c, err := a.Connect(0xAABBCCDDEEFF, ble.AddrPublic, ev)
	if err != nil {
		t.Fatal(err)
	}
	bz.advertiseFrom(fakeDevice, -63)
	expectRSSI(t, a, bz, -63, "connected")
	if err := c.Disconnect(); err != nil {
		t.Fatal(err)
	}
	bz.advertiseFrom(fakeDevice, -64)
	expectRSSI(t, a, bz, -63, "after Disconnect")

	bz.mu.Lock()
	bz.connectFail = true
	bz.mu.Unlock()
	if _, err := a.Connect(0xAABBCCDDEEFF, ble.AddrPublic, ev); err == nil {
		t.Fatal("connect did not fail")
	}
	bz.advertiseFrom(fakeDevice, -65)
	expectRSSI(t, a, bz, -63, "after a failed connect")

	bz.mu.Lock()
	bz.connectFail = false
	bz.mu.Unlock()
	if _, err := a.Connect(0xAABBCCDDEEFF, ble.AddrPublic, ev); err != nil {
		t.Fatal(err)
	}
	bz.advertiseFrom(fakeDevice, -66)
	expectRSSI(t, a, bz, -66, "connected again")
	bz.bus.Close() // bluetoothd exits
	if err := <-disconnected; err == nil {
		t.Fatal("Disconnected without a reason")
	}
	bz = startFakeBlueZ(t, os.Getenv("DBUS_SYSTEM_BUS_ADDRESS"), false)
	t.Cleanup(func() { bz.bus.Close() })
	bz.advertise() // a new bluetoothd announces the device again
	waitFor(t, "device announced again", func() bool { r, _ := rssiOf(a, fakeDevice); return r == -60 })
	bz.advertiseFrom(fakeDevice, -67)
	expectRSSI(t, a, bz, -60, "after bluetoothd restarted")
	if allDevices(a) {
		t.Fatal("rule for every device back after bluetoothd restarted")
	}
}

// TestFallbackMatches: when the monitor fails, the rule for every device comes back before discovery
// restarts, and BlueZ's advertisements are delivered again.
func TestFallbackMatches(t *testing.T) {
	src := newFakeRaw()
	useRaw(t, src)
	a, bz, got := openRawTest(t, RawOn, false)
	bz.advertise()
	src.ads <- ble.Advert{Addr: 1, RSSI: -50}
	waitFor(t, "rule for every device dropped", func() bool { return !allDevices(a) })
	bz.advertiseFrom(fakeDevice, -61)
	expectRSSI(t, a, bz, -60, "raw advertisements")

	src.fail <- errors.New("read: network is down")
	waitFor(t, "discovery with DuplicateData", func() bool { return dupData(bz) })
	if !allDevices(a) {
		t.Fatal("discovery restarted before the rule for every device came back")
	}
	bz.advertiseFrom(fakeDevice, -67)
	waitFor(t, "BlueZ advertisement", func() bool { g := got(); return len(g) > 1 && g[len(g)-1] == -67 })
}

// TestNoOrphan: a property change for a device BlueZ has removed does not bring it back into the cache.
func TestNoOrphan(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, false)
	defer bz.bus.Close()
	a, err := Open("hci0", Options{RawAdverts: RawOff})
	if err != nil {
		t.Fatal(err)
	}
	bz.advertise()
	waitFor(t, "device", func() bool { _, ok := rssiOf(a, fakeDevice); return ok })
	bz.removeDevice(fakeDevice)
	bz.bus.Emit(fakeDevice, ifProps+".PropertiesChanged", ifDevice,
		map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-70))}, []string{})
	flush(t, a, bz)
	if _, ok := rssiOf(a, fakeDevice); ok {
		t.Fatal("removed device back in the cache")
	}
}

// TestSigQueue: past the soft limit advertisement updates are dropped, past the hard limit characteristic
// values too, and the rest is always kept, in order.
func TestSigQueue(t *testing.T) {
	props := func(iface string, changed map[string]dbus.Variant) *dbus.Signal {
		return &dbus.Signal{Name: ifProps + ".PropertiesChanged", Body: []any{iface, changed, []string{}}}
	}
	advert := props(ifDevice, map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-60))})
	value := props(ifChar, map[string]dbus.Variant{"Value": dbus.MakeVariant([]byte{1})})
	connected := props(ifDevice, map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-60)),
		"Connected": dbus.MakeVariant(false)})
	added := &dbus.Signal{Name: ifObjManager + ".InterfacesAdded"}

	q := newSigQueue()
	for i := 0; i < sigSoftLimit; i++ {
		q.DeliverSignal("", "", advert)
	}
	q.DeliverSignal("", "", advert)    // dropped
	q.DeliverSignal("", "", value)     // kept
	q.DeliverSignal("", "", connected) // kept
	for i := 0; i < sigHardLimit; i++ {
		q.DeliverSignal("", "", added)
	}
	q.DeliverSignal("", "", value)     // dropped
	q.DeliverSignal("", "", connected) // kept
	sigs, dropped, ok := q.take(nil)
	if !ok || dropped != 2 || len(sigs) != sigSoftLimit+2+sigHardLimit+1 {
		t.Fatalf("ok %v, %d dropped, %d queued", ok, dropped, len(sigs))
	}
	if sigs[sigSoftLimit] != value || sigs[sigSoftLimit+1] != connected || sigs[len(sigs)-1] != connected {
		t.Fatal("signals out of order")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if s, _, ok := q.take(sigs); !ok || len(s) != 1 || s[0] != added {
			t.Errorf("after waiting: ok %v, %d signals", ok, len(s))
		}
		if _, _, ok := q.take(nil); ok {
			t.Error("take after Terminate")
		}
	}()
	q.DeliverSignal("", "", added)
	q.Terminate()
	q.DeliverSignal("", "", added) // after the connection closed: ignored
	<-done
}
