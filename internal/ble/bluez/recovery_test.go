//go:build linux

package bluez

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
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
	t.Cleanup(func() { resetUSBDevice = usbReset })
	a, err := Open("hci0", Options{RestartCmd: "touch '" + marker + "'", USBReset: true})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanActive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()

	waitFor(t, "every step", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(steps) >= 3
	})
	n, _ := bz.calls()
	waitFor(t, "more unanswered starts", func() bool { s, _ := bz.calls(); return s >= n+4 })
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
	waitFor(t, "running after the power cycle", statusIs(a, ble.ScanRunning, ble.ScanActive))
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
	waitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanPassive))

	if err := a.registerMonitors(false); err != nil {
		t.Fatalf("registering again: %v", err)
	}
	if !a.powerCycle() {
		t.Fatal("not cycled")
	}
	time.Sleep(200 * time.Millisecond)
	waitFor(t, "running passively", statusIs(a, ble.ScanRunning, ble.ScanPassive))
	a.mu.Lock()
	noMonitor := a.noMonitor
	a.mu.Unlock()
	if noMonitor {
		t.Error("passive scanning given up")
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

func TestUSBDevicePath(t *testing.T) {
	sys := t.TempDir()
	dev := filepath.Join(sys, "devices", "platform", "usb2", "2-1")
	iface := filepath.Join(dev, "2-1:1.0")
	if err := os.MkdirAll(filepath.Join(iface, "bluetooth", "hci0"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dev, "busnum"), []byte("2\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "devnum"), []byte("13\n"), 0o644)
	class := filepath.Join(sys, "class", "bluetooth", "hci0")
	if err := os.MkdirAll(class, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(iface, filepath.Join(class, "device")); err != nil {
		t.Fatal(err)
	}
	got, err := usbDevicePath(sys, "hci0")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/dev/bus/usb/002/013" {
		t.Errorf("path %q", got)
	}
	if _, err := usbDevicePath(sys, "hci1"); err == nil {
		t.Error("no error for a missing adapter")
	}

	// An adapter that is not on USB, such as one on a UART.
	uart := filepath.Join(sys, "devices", "serial0")
	os.MkdirAll(uart, 0o755)
	class = filepath.Join(sys, "class", "bluetooth", "hci2")
	os.MkdirAll(class, 0o755)
	os.Symlink(uart, filepath.Join(class, "device"))
	if _, err := usbDevicePath(sys, "hci2"); err == nil {
		t.Error("no error for an adapter that is not on USB")
	}
}

// _IO('U', 20) has a different direction bit on MIPS, which is what most routers run.
func TestUSBResetRequest(t *testing.T) {
	want := uintptr(0x5514)
	if strings.HasPrefix(runtime.GOARCH, "mips") || strings.HasPrefix(runtime.GOARCH, "ppc") {
		want = 0x20005514
	}
	if got := usbResetRequest(runtime.GOARCH); got != want {
		t.Errorf("request %#x, want %#x", got, want)
	}
	if got := usbResetRequest("mipsle"); got != 0x20005514 {
		t.Errorf("mipsle request %#x", got)
	}
	if got := usbResetRequest("arm64"); got != 0x5514 {
		t.Errorf("arm64 request %#x", got)
	}
}
