//go:build linux

package bluez

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/bletest"
)

// openPauseTest opens an adapter that pauses scanning for connects, against a fake bluetoothd with the
// monitor API, and starts scanning in mode.
func openPauseTest(t *testing.T, mode ble.ScanMode, opt Options) (*Adapter, *fakeBlueZ, string) {
	t.Helper()
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	t.Cleanup(func() { bz.bus.Close() })
	opt.PauseOnConnect = true
	if opt.RawAdverts == RawAuto {
		opt.RawAdverts = RawOff
	}
	a, err := Open("hci0", testOptions(opt))
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(mode)
	stop, _ := a.Scan(func(ble.Advert) {})
	t.Cleanup(stop)
	bletest.WaitFor(t, "running", statusIs(a, ble.ScanRunning, mode))
	return a, bz, addr
}

// connectAsync connects in the background; the result arrives on the channel.
func connectAsync(a *Adapter, addr uint64) <-chan error {
	errc := make(chan error, 1)
	go func() {
		c, err := a.Connect(addr, ble.AddrRandom, ble.Events{Notification: func(uint16, []byte) {},
			Disconnected: func(error) {}})
		if err == nil {
			c.Disconnect()
		}
		errc <- err
	}()
	return errc
}

func holdConnects(bz *fakeBlueZ) chan struct{} {
	bz.mu.Lock()
	defer bz.mu.Unlock()
	bz.hold = make(chan struct{})
	return bz.hold
}

// scanningAt tells, per connect so far, whether the fake was scanning when it started.
func scanningAt(bz *fakeBlueZ) []bool {
	bz.mu.Lock()
	defer bz.mu.Unlock()
	return append([]bool(nil), bz.scanning...)
}

func scanOff(bz *fakeBlueZ) bool { _, disc, reg := bz.state(); return !disc && reg == 0 }

func passiveUp(a *Adapter, bz *fakeBlueZ) func() bool {
	return func() bool {
		_, _, reg := bz.state()
		return reg == len(monitorPatterns) && statusIs(a, ble.ScanRunning, ble.ScanPassive)()
	}
}

// waitConnects waits for n connects to have reached the fake and checks that scanning was off for each.
func waitConnects(t *testing.T, bz *fakeBlueZ, n int) {
	t.Helper()
	bletest.WaitFor(t, "connect started", func() bool { return len(scanningAt(bz)) == n })
	for i, on := range scanningAt(bz) {
		if on {
			t.Fatalf("connect %d started while scanning", i+1)
		}
	}
}

// TestPausePassive: the monitors are unregistered before the connect reaches bluetoothd, the scanner reports
// idle meanwhile, and they come back as soon as the connect returns.
func TestPausePassive(t *testing.T) {
	a, bz, _ := openPauseTest(t, ble.ScanPassive, Options{})
	hold := holdConnects(bz)
	errc := connectAsync(a, 0xC1C2C3C4C5C6)
	waitConnects(t, bz, 1)
	if !scanOff(bz) || a.ScanStatus() != (ble.ScanStatus{State: ble.ScanIdle, Mode: ble.ScanPassive}) {
		t.Fatalf("while connecting: scanning %v, status %+v", !scanOff(bz), a.ScanStatus())
	}
	time.Sleep(4 * testScanRetry) // the supervisor's ticks must not arm the scan again
	if !scanOff(bz) {
		t.Fatal("scanning again while the connect is in progress")
	}

	hold <- struct{}{}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	up := passiveUp(a, bz)
	for !up() && time.Since(start) < 5*time.Second {
		time.Sleep(time.Millisecond)
	}
	if !up() {
		t.Fatal("not scanning again after the connect")
	}
	t.Logf("scanning resumed %v after the connect returned", time.Since(start))
}

// TestPauseActive: discovery stops for the connect and starts again after it, and the stall watchdog does
// not count the pause as a stall.
func TestPauseActive(t *testing.T) {
	a, bz, _ := openPauseTest(t, ble.ScanActive, Options{WatchdogOptions: ble.WatchdogOptions{StallTimeout: 400 * time.Millisecond}})
	bz.advertise() // the watchdog's clock starts from here
	var failures atomic.Int32
	a.OnScanStatus(func(st ble.ScanStatus) {
		if st.State == ble.ScanFailed {
			failures.Add(1)
		}
	})
	hold := holdConnects(bz)
	errc := connectAsync(a, 0xC1C2C3C4C5C6)
	waitConnects(t, bz, 1)
	bletest.WaitFor(t, "idle", statusIs(a, ble.ScanIdle, ble.ScanActive))
	time.Sleep(time.Second) // over two stall timeouts without advertisements
	hold <- struct{}{}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	bletest.WaitFor(t, "discovery again", func() bool { _, disc, _ := bz.state(); return disc })
	bletest.WaitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanActive))
	if starts, stops := bz.calls(); starts != 2 || stops != 1 || failures.Load() != 0 {
		t.Fatalf("%d StartDiscovery, %d StopDiscovery, %d failures; want 2, 1, 0", starts, stops, failures.Load())
	}
}

// TestPauseFailure: scanning resumes after a connect that fails.
func TestPauseFailure(t *testing.T) {
	a, bz, _ := openPauseTest(t, ble.ScanPassive, Options{})
	bz.mu.Lock()
	bz.connectFail = true
	bz.mu.Unlock()
	if err := <-connectAsync(a, 0xC1C2C3C4C5C6); err == nil {
		t.Fatal("connect succeeded")
	}
	waitConnects(t, bz, 1)
	bletest.WaitFor(t, "passive scan again", passiveUp(a, bz))
}

// TestPauseLimit: a connect that hangs holds scanning off for pauseLimit only.
func TestPauseLimit(t *testing.T) {
	a, bz, _ := openPauseTest(t, ble.ScanPassive, Options{timings: timings{pauseLimit: 300 * time.Millisecond}})
	hold := holdConnects(bz)
	errc := connectAsync(a, 0xC1C2C3C4C5C6)
	waitConnects(t, bz, 1)
	bletest.WaitFor(t, "passive scan with the connect still in progress", passiveUp(a, bz))
	select {
	case err := <-errc:
		t.Fatalf("connect returned early: %v", err)
	default:
	}
	hold <- struct{}{}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * testScanRetry) // the connect returning must not upset the count
	if !passiveUp(a, bz)() {
		t.Fatal("not scanning after the connect")
	}
}

// TestPauseBluetoothdLost: bluetoothd exiting during a connect fails it, and scanning starts on the next
// bluetoothd.
func TestPauseBluetoothdLost(t *testing.T) {
	a, bz, addr := openPauseTest(t, ble.ScanPassive, Options{})
	hold := holdConnects(bz)
	errc := connectAsync(a, 0xC1C2C3C4C5C6)
	waitConnects(t, bz, 1)
	bz.bus.Close()
	close(hold)
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("connect succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connect still waiting after bluetoothd exited")
	}
	bz = startFakeBlueZ(t, addr, true)
	t.Cleanup(func() { bz.bus.Close() })
	bletest.WaitFor(t, "passive scan on the new bluetoothd", passiveUp(a, bz))
}

// TestPauseOverlap: overlapping connects share one pause, which ends with the last of them.
func TestPauseOverlap(t *testing.T) {
	a, bz, _ := openPauseTest(t, ble.ScanPassive, Options{})
	hold := holdConnects(bz)
	errA := connectAsync(a, 0xC1C2C3C4C5C6)
	errB := connectAsync(a, 0xD1D2D3D4D5D6)
	waitConnects(t, bz, 2)

	hold <- struct{}{}
	var rest <-chan error
	select {
	case err := <-errA:
		if err != nil {
			t.Fatal(err)
		}
		rest = errB
	case err := <-errB:
		if err != nil {
			t.Fatal(err)
		}
		rest = errA
	}
	time.Sleep(4 * testScanRetry)
	if !scanOff(bz) {
		t.Fatal("scanning again while the other connect is in progress")
	}

	hold <- struct{}{}
	if err := <-rest; err != nil {
		t.Fatal(err)
	}
	bletest.WaitFor(t, "passive scan again", passiveUp(a, bz))
}

// TestPauseQuiet: with raw advertisements, the monitor that matches nothing goes for the connect too and comes
// back after it, and advertisements flow again.
func TestPauseQuiet(t *testing.T) {
	src := newFakeRaw()
	useRaw(t, src)
	var got atomic.Int32
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	t.Cleanup(func() { bz.bus.Close() })
	a, err := Open("hci0", testOptions(Options{RawAdverts: RawOn, ConnectOptions: ble.ConnectOptions{PauseOnConnect: true}}))
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanPassive)
	stop, _ := a.Scan(func(ble.Advert) { got.Add(1) })
	t.Cleanup(stop)
	src.ads <- ble.Advert{Addr: 1, RSSI: -50}
	bletest.WaitFor(t, "monitor that matches nothing", func() bool { _, q, reg := fakeQuiet(bz); return q && reg == 1 })

	hold := holdConnects(bz)
	errc := connectAsync(a, 0xC1C2C3C4C5C6)
	waitConnects(t, bz, 1)
	hold <- struct{}{}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	bletest.WaitFor(t, "monitor that matches nothing again", func() bool {
		_, q, reg := fakeQuiet(bz)
		return q && reg == 1 && statusIs(a, ble.ScanRunning, ble.ScanPassive)()
	})
	n := got.Load()
	src.ads <- ble.Advert{Addr: 2, RSSI: -50}
	bletest.WaitFor(t, "advertisement", func() bool { return got.Load() > n })
}

// TestPauseOff: without PauseOnConnect, scanning goes on during connects, which still count as in progress
// for the recovery steps (see strike).
func TestPauseOff(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	t.Cleanup(func() { bz.bus.Close() })
	a, err := Open("hci0", testOptions(Options{RawAdverts: RawOff}))
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanPassive)
	stop, _ := a.Scan(func(ble.Advert) {})
	t.Cleanup(stop)
	bletest.WaitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanPassive))
	hold := holdConnects(bz)
	errc := connectAsync(a, 0xC1C2C3C4C5C6)
	bletest.WaitFor(t, "connect started", func() bool { return len(scanningAt(bz)) == 1 })
	if !a.pause.Connecting() {
		t.Fatal("connect not counted while in progress")
	}
	hold <- struct{}{}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if s := scanningAt(bz); len(s) != 1 || !s[0] {
		t.Fatalf("scanning at connect: %v, want [true]", s)
	}
	if a.pause.Connecting() {
		t.Fatal("connect still counted after it returned")
	}
}
