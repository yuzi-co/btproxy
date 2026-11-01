//go:build linux

package bluez

import (
	"errors"
	"slices"
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
)

func fakeQuiet(bz *fakeBlueZ) (rssi int16, quietMons bool, registered int) {
	bz.mu.Lock()
	defer bz.mu.Unlock()
	return bz.rssi, bz.quietMons, bz.registered
}

// TestQuietPassive: once the monitor delivers, passive scanning goes on with a monitor that matches nothing,
// and the real monitors come back when it fails.
func TestQuietPassive(t *testing.T) {
	src := newFakeRaw()
	useRaw(t, src)
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	t.Cleanup(func() { bz.bus.Close() })
	a, err := Open("hci0", Options{RawAdverts: RawOn})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanPassive)
	stop, _ := a.Scan(func(ble.Advert) {})
	t.Cleanup(stop)
	waitFor(t, "passive scan", statusIs(a, ble.ScanRunning, ble.ScanPassive))
	if _, q, reg := fakeQuiet(bz); q || reg != len(monitorPatterns) {
		t.Fatalf("before the monitor delivered: quiet %v, %d monitors", q, reg)
	}

	src.ads <- ble.Advert{Addr: 1, RSSI: -50}
	waitFor(t, "monitor that matches nothing", func() bool { _, q, reg := fakeQuiet(bz); return q && reg == 1 })
	waitFor(t, "passive scan", statusIs(a, ble.ScanRunning, ble.ScanPassive))
	if _, disc, _ := bz.state(); disc {
		t.Fatal("discovering")
	}

	src.fail <- errors.New("read: network is down")
	waitFor(t, "real monitors", func() bool {
		_, q, reg := fakeQuiet(bz)
		return !q && reg == len(monitorPatterns) && allDevices(a)
	})
	waitFor(t, "passive scan", statusIs(a, ble.ScanRunning, ble.ScanPassive))
}

// TestQuietActive: once the monitor delivers, discovery runs with an RSSI threshold no advertisement reaches
// and the monitor that matches nothing, and without either after a fallback.
func TestQuietActive(t *testing.T) {
	src := newFakeRaw()
	useRaw(t, src)
	a, bz, _ := openRawTest(t, RawOn, true)
	if rssi, q, reg := fakeQuiet(bz); rssi != 0 || q || reg != len(monitorPatterns) {
		t.Fatalf("before the monitor delivered: RSSI %d, quiet %v, %d monitors", rssi, q, reg)
	}

	src.ads <- ble.Advert{Addr: 1, RSSI: -50}
	waitFor(t, "quiet discovery", func() bool {
		rssi, q, reg := fakeQuiet(bz)
		_, disc, _ := bz.state()
		return rssi == quietRSSI && q && reg == 1 && disc
	})

	src.fail <- errors.New("read: network is down")
	waitFor(t, "discovery after the fallback", func() bool {
		rssi, _, reg := fakeQuiet(bz)
		_, disc, _ := bz.state()
		return rssi == 0 && reg == 0 && disc && dupData(bz)
	})
	waitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanActive))
}

// TestQuietOffload: when the controller matches monitor patterns itself, bluetoothd keeps reporting devices,
// as a monitor that matches nothing would hide every advertisement from the HCI monitor too.
func TestQuietOffload(t *testing.T) {
	src := newFakeRaw()
	useRaw(t, src)
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	bz.offload = true
	t.Cleanup(func() { bz.bus.Close() })
	a, err := Open("hci0", Options{RawAdverts: RawOn})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanActive)
	stop, _ := a.Scan(func(ble.Advert) {})
	t.Cleanup(stop)
	waitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanActive))
	src.ads <- ble.Advert{Addr: 1, RSSI: -50}
	waitFor(t, "discovery armed again", func() bool { s, _ := bz.calls(); return s >= 2 })
	waitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanActive))
	if rssi, q, reg := fakeQuiet(bz); rssi != 0 || q || reg != len(monitorPatterns) {
		t.Fatalf("with controller patterns: RSSI %d, quiet %v, %d monitors", rssi, q, reg)
	}
}

// TestConnectUnknown: a device BlueZ has no object for, which is most of them once bluetoothd reports no
// devices, is connected through ConnectDevice, one it has through Device1.Connect, and when the device
// cache is behind BlueZ, the other call follows.
func TestConnectUnknown(t *testing.T) {
	src := newFakeRaw()
	useRaw(t, src)
	a, bz, _ := openRawTest(t, RawOn, true)
	src.ads <- ble.Advert{Addr: 1, RSSI: -50}
	waitFor(t, "quiet discovery", func() bool { rssi, _, _ := fakeQuiet(bz); return rssi == quietRSSI })
	ev := ble.Events{Notification: func(uint16, []byte) {}, Disconnected: func(error) {}}
	connect := func(addr uint64, want ...string) {
		t.Helper()
		before := len(bz.connectCalls())
		c, err := a.Connect(addr, ble.AddrRandom, ev)
		if err != nil {
			t.Fatal(err)
		}
		if got := bz.connectCalls()[before:]; !slices.Equal(got, want) {
			t.Fatalf("connect %x: calls %v, want %v", addr, got, want)
		}
		if err := c.Disconnect(); err != nil {
			t.Fatal(err)
		}
	}

	connect(0xC1C2C3C4C5C6, "ConnectDevice")
	waitFor(t, "device announced", func() bool { _, ok := rssiOf(a, fakeAdapter+"/dev_C1_C2_C3_C4_C5_C6"); return ok })
	connect(0xC1C2C3C4C5C6, "Connect")

	bz.hideDevice(fakeAdapter + "/dev_D1_D2_D3_D4_D5_D6")
	connect(0xD1D2D3D4D5D6, "ConnectDevice", "Connect")

	bz.advertise()
	waitFor(t, "device announced", func() bool { _, ok := rssiOf(a, fakeDevice); return ok })
	bz.bus.Export(nil, fakeDevice, ifDevice) // BlueZ removed it, the signal is on its way
	bz.mu.Lock()
	delete(bz.devices, fakeDevice)
	bz.mu.Unlock()
	connect(0xAABBCCDDEEFF, "ConnectDevice")
}
