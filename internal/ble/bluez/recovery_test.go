//go:build linux

package bluez

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/bletest"
	"github.com/yuzi-co/btproxy/internal/ble/hci"
	"github.com/yuzi-co/btproxy/internal/logx"
)

// TestRecoverySteps has discovery keep going unanswered, and checks that each step runs once, in order: the
// adapter's power, then the restart command, then the USB device.
func TestRecoverySteps(t *testing.T) {
	bluetoothdRestarts = restartGate{}
	t.Cleanup(func() { bluetoothdRestarts = restartGate{} })
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, false)
	defer bz.bus.Close()
	bz.setWedged(true)

	var mu sync.Mutex
	var steps []string
	step := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, s)
	}
	bz.mu.Lock()
	bz.onPowerOff = func() { step("power") }
	bz.mu.Unlock()
	marker := filepath.Join(t.TempDir(), "restarted")
	resetUSBDevice = func(string) error {
		if _, err := os.Stat(marker); err == nil {
			step("restart")
		}
		step("usb")
		return nil
	}
	t.Cleanup(func() { resetUSBDevice = hci.ResetUSB })
	a, err := Open("hci0", Options{RestartCmd: "touch '" + marker + "'", WatchdogOptions: ble.WatchdogOptions{USBReset: true}})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanActive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()

	bletest.WaitFor(t, "every step", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(steps) >= 3
	})
	n, _ := bz.calls()
	bletest.WaitFor(t, "more unanswered starts", func() bool { s, _ := bz.calls(); return s >= n+4 })
	mu.Lock()
	got := strings.Join(steps, " ")
	mu.Unlock()
	if got != "power restart usb" {
		t.Errorf("steps %q", got)
	}
	if a.Recoveries() != 2 || a.Restarts() != 1 {
		t.Errorf("Recoveries() = %d, Restarts() = %d", a.Recoveries(), a.Restarts())
	}
}

// With nothing else to try, the power step still runs: it needs neither a command nor a flag, which makes it
// the only step in a container.
func TestRecoveryWithOnlyThePowerStep(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, false)
	defer bz.bus.Close()
	bz.setWedged(true)
	off := make(chan struct{}, 8)
	bz.mu.Lock()
	bz.onPowerOff = func() { off <- struct{}{} }
	bz.mu.Unlock()
	a, err := Open("hci0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanActive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	select {
	case <-off:
	case <-time.After(10 * time.Second):
		t.Fatal("adapter not power-cycled")
	}
	bz.setWedged(false)
	bletest.WaitFor(t, "running after the power cycle", statusIs(a, ble.ScanRunning, ble.ScanActive))
}

// A bluetoothd that does not answer cannot power-cycle anything, so the restart comes on the same strike.
func TestHungBluetoothdSkipsThePowerStep(t *testing.T) {
	bluetoothdRestarts = restartGate{}
	t.Cleanup(func() { bluetoothdRestarts = restartGate{} })
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, false)
	defer bz.bus.Close()
	marker := filepath.Join(t.TempDir(), "restarted")
	a, err := Open("hci0", Options{RestartCmd: "touch '" + marker + "'"})
	if err != nil {
		t.Fatal(err)
	}
	hung := make(chan struct{})
	defer close(hung)
	bz.mu.Lock()
	bz.hung = hung
	bz.mu.Unlock()

	a.strikes = restartAfter - 1
	a.lastStrike = time.Now()
	a.strike()
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("not restarted on the strike that found bluetoothd hung: %v", err)
	}
	if a.Recoveries() != 0 {
		t.Errorf("counted %d recoveries that did not happen", a.Recoveries())
	}
}

// BlueZ keeps a monitor application across a power cycle. A scanner that lost track of that gets "Already
// Exists", which must not read as a bluetoothd without the monitor API: scanning stayed active until the next
// bluetoothd restart, seen on a real router.
func TestPowerCycleKeepsPassiveScanning(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	defer bz.bus.Close()
	a, err := Open("hci0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanPassive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	bletest.WaitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanPassive))

	if err := a.registerMonitors(false); err != nil {
		t.Fatalf("registering again: %v", err)
	}
	if !a.powerCycle() {
		t.Fatal("not cycled")
	}
	time.Sleep(200 * time.Millisecond)
	bletest.WaitFor(t, "running passively", statusIs(a, ble.ScanRunning, ble.ScanPassive))
	a.mu.Lock()
	noMonitor := a.noMonitor
	a.mu.Unlock()
	if noMonitor {
		t.Error("passive scanning given up")
	}
}

func noMonitor(a *Adapter) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.noMonitor
}

// A monitor call that timed out still runs when bluetoothd wakes up, and its Release then comes while the scanner
// has registered the monitors again. Seen on a real router, where it cost two thirds of the advertisements until
// bluetoothd restarted.
func TestLateReleaseOfOurOwnCallKeepsPassiveScanning(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	defer bz.bus.Close()
	a, err := Open("hci0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanPassive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	bletest.WaitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanPassive))

	hung := make(chan struct{})
	bz.mu.Lock()
	bz.hung = hung
	bz.mu.Unlock()
	a.unregisterMonitors() // times out
	bz.mu.Lock()
	bz.hung = nil
	bz.mu.Unlock()
	a.mu.Lock()
	a.monitorUp = true
	a.mu.Unlock()
	close(hung)
	time.Sleep(200 * time.Millisecond)
	if noMonitor(a) {
		t.Error("passive scanning given up over our own call")
	}
}

// Whatever made BlueZ drop the monitor, the scanner tries again: giving up until bluetoothd restarts cost two
// thirds of the advertisements for as long as nobody noticed.
func TestReleasedMonitorIsTriedAgain(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	defer bz.bus.Close()
	a, err := Open("hci0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.ScanSetting(); got != "Passive" && got != "Active" {
		t.Errorf("setting %q before scanning", got)
	}
	a.SetScanMode(ble.ScanPassive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	bletest.WaitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanPassive))
	if got := a.ScanSetting(); got != "Passive" {
		t.Errorf("setting %q while passive", got)
	}

	bz.releaseMonitors()
	bletest.WaitFor(t, "active after the release", statusIs(a, ble.ScanRunning, ble.ScanActive))
	if got := a.ScanSetting(); got != "Active, passive unavailable" {
		t.Errorf("setting %q in the fallback", got)
	}
	bletest.WaitFor(t, "passive again", statusIs(a, ble.ScanRunning, ble.ScanPassive))
	if got := a.ScanSetting(); got != "Passive" {
		t.Errorf("setting %q after the retry", got)
	}
}

// A monitor BlueZ keeps refusing must not make the scanner restart every minute for ever.
func TestMonitorRetryBacksOff(t *testing.T) {
	saved := monitorRetry
	monitorRetry = time.Minute
	defer func() { monitorRetry = saved }()
	a := &Adapter{}
	a.log = logx.Discard()
	a.kick = make(chan struct{}, 1)
	now := time.Now()
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		a.monitorUp = true
		a.monitorReleased()
		if got := a.retryMonitorAt.Sub(now); got < want || got > want+time.Second {
			t.Errorf("retry in %v, want %v", got, want)
		}
	}
	for range 10 {
		a.monitorUp = true
		a.monitorReleased()
	}
	if got := time.Until(a.retryMonitorAt); got > monitorRetryMax {
		t.Errorf("retry in %v, past the limit", got)
	}
	a.lastRelease = time.Now().Add(-2 * monitorRetryMax) // it held for a long time, so start over
	a.monitorUp = true
	a.monitorReleased()
	if got := time.Until(a.retryMonitorAt); got > time.Minute+time.Second {
		t.Errorf("retry in %v after a long quiet time", got)
	}
}

// Without the monitor API nothing changes by retrying, so only a bluetoothd restart clears that.
func TestMissingMonitorAPIIsNotTriedAgain(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, false)
	defer bz.bus.Close()
	a, err := Open("hci0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanPassive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	bletest.WaitFor(t, "active", statusIs(a, ble.ScanRunning, ble.ScanActive))
	starts, _ := bz.calls()
	time.Sleep(3 * monitorRetry)
	if now, _ := bz.calls(); now != starts {
		t.Errorf("discovery restarted %d times", now-starts)
	}
}

// No step while a connect runs: each one would end it.
func TestNoRecoveryDuringAConnect(t *testing.T) {
	a := &Adapter{}
	a.log = logx.Discard()
	called := false
	a.resetUSB = func() error { called = true; return nil }
	a.connecting = 1
	a.step = stepUSB
	a.strikes = restartAfter
	a.lastStrike = time.Now()
	a.strike()
	if called {
		t.Error("USB reset during a connect")
	}
	a.connecting = 0
	a.strike()
	if !called {
		t.Error("no USB reset after the connect")
	}
}

// The USB reset is limited like the restart command: a stick that stays broken must not be reset in a loop.
func TestUSBResetLimit(t *testing.T) {
	a := &Adapter{}
	a.log = logx.Discard()
	n := 0
	a.resetUSB = func() error { n++; return errors.New("no such device") }
	a.lastPowerCycle = time.Now() // leaves only the USB step
	for range 3 {
		a.strikes = restartAfter
		a.lastStrike = time.Now()
		a.step = stepUSB
		a.strike()
	}
	if n != 1 {
		t.Errorf("reset %d times", n)
	}
	if a.Recoveries() != 0 {
		t.Errorf("a failed reset counted: %d", a.Recoveries())
	}
}
