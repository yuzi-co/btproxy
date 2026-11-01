//go:build linux

package bluez

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"btproxy/internal/ble"
)

func init() {
	// No test opens the real HCI monitor: it needs CAP_NET_RAW and a kernel with Bluetooth.
	openRaw = func(uint16) (rawSource, error) { return nil, errors.New("no HCI monitor in tests") }
}

// fakeRaw stands in for the HCI monitor.
type fakeRaw struct {
	ads    chan ble.Advert
	fail   chan error
	done   chan struct{}
	once   sync.Once
	closed atomic.Bool
}

func newFakeRaw() *fakeRaw {
	return &fakeRaw{ads: make(chan ble.Advert), fail: make(chan error), done: make(chan struct{})}
}

func (f *fakeRaw) Run(emit func(ble.Advert), active func() bool) error {
	for {
		select {
		case ad := <-f.ads:
			emit(ad)
		case err := <-f.fail:
			return err
		case <-f.done:
			return nil
		}
	}
}

func (f *fakeRaw) Close() { f.once.Do(func() { f.closed.Store(true); close(f.done) }) }

// useRaw makes Open find src for hci0.
func useRaw(t *testing.T, src rawSource) {
	old := openRaw
	openRaw = func(idx uint16) (rawSource, error) {
		if idx != 0 {
			t.Errorf("controller index %d, want 0", idx)
		}
		return src, nil
	}
	t.Cleanup(func() { openRaw = old })
}

// openRawTest opens the adapter, scans actively and returns the advertisements' RSSI values as they arrive.
func openRawTest(t *testing.T, mode RawMode, monitors bool) (*Adapter, *fakeBlueZ, func() []int8) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, monitors)
	t.Cleanup(func() { bz.bus.Close() })
	a, err := Open("hci0", Options{RawAdverts: mode})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanActive)
	var mu sync.Mutex
	var rssi []int8
	stop, _ := a.Scan(func(ad ble.Advert) {
		mu.Lock()
		rssi = append(rssi, ad.RSSI)
		mu.Unlock()
	})
	t.Cleanup(stop)
	waitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanActive))
	return a, bz, func() []int8 {
		mu.Lock()
		defer mu.Unlock()
		return append([]int8(nil), rssi...)
	}
}

func dupData(bz *fakeBlueZ) bool {
	bz.mu.Lock()
	defer bz.mu.Unlock()
	return bz.dupData
}

// TestRawUnavailable: without the monitor, auto mode rebuilds advertisements from BlueZ's properties and
// on mode refuses to start.
func TestRawUnavailable(t *testing.T) {
	_, bz, got := openRawTest(t, RawAuto, true)
	if _, _, reg := bz.state(); reg != 0 {
		t.Errorf("%d monitors registered without the HCI monitor", reg)
	}
	if !dupData(bz) {
		t.Error("discovery without DuplicateData")
	}
	bz.advertise()
	waitFor(t, "BlueZ advertisement", func() bool { return len(got()) == 1 })

	if _, err := Open("hci0", Options{RawAdverts: RawOn}); err == nil {
		t.Error("RawOn opened without the monitor")
	}
}

// TestRawSource: advertisements come from the monitor and BlueZ's are left out, until the monitor fails.
// Meanwhile discovery runs without DuplicateData and with the advertisement monitors registered, which turns
// the controller's duplicate filter off.
func TestRawSource(t *testing.T) {
	src := newFakeRaw()
	useRaw(t, src)
	a, bz, got := openRawTest(t, RawOn, true)
	if _, disc, reg := bz.state(); dupData(bz) || !disc || reg != len(monitorPatterns) {
		t.Errorf("discovering %v, DuplicateData %v, %d monitors registered", disc, dupData(bz), reg)
	}
	src.ads <- ble.Advert{Addr: 1, RSSI: -50}
	for i := 0; i < 2*rawMissLimit; i++ { // monitor delivered: never given up
		bz.advertise()
	}
	src.ads <- ble.Advert{Addr: 1, RSSI: -51}
	time.Sleep(100 * time.Millisecond)
	if g := got(); len(g) != 2 || g[0] != -50 || g[1] != -51 {
		t.Fatalf("advertisements %v, want the monitor's only", g)
	}

	src.fail <- errors.New("read: network is down")
	waitFor(t, "fallback", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.raw == nil })
	waitFor(t, "discovery with DuplicateData", func() bool { return dupData(bz) })
	if _, disc, reg := bz.state(); !disc || reg != 0 {
		t.Errorf("after the fallback: discovering %v, %d monitors registered", disc, reg)
	}
	bz.advertise()
	waitFor(t, "BlueZ advertisement", func() bool { g := got(); return len(g) == 3 && g[2] == -60 })
}

// TestRawSilent: in auto mode a monitor that delivers nothing while BlueZ reports advertisements is given
// up after rawMissLimit of them, and BlueZ's are delivered from then on.
func TestRawSilent(t *testing.T) {
	src := newFakeRaw()
	useRaw(t, src)
	_, bz, got := openRawTest(t, RawAuto, false) // without the monitor API: discovery alone
	for i := 0; i < rawMissLimit-1; i++ {
		bz.advertise()
	}
	time.Sleep(100 * time.Millisecond)
	if len(got()) != 0 || src.closed.Load() {
		t.Fatalf("given up early: %d advertisements, closed %v", len(got()), src.closed.Load())
	}
	bz.advertise()
	waitFor(t, "monitor closed", src.closed.Load)
	waitFor(t, "BlueZ advertisement", func() bool { return len(got()) >= 1 })
	waitFor(t, "discovery with DuplicateData", func() bool { return dupData(bz) })
}
