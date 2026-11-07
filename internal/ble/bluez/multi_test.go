//go:build linux

package bluez

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/bletest"
)

// TestAgentAnyAdapter checks that the one pairing agent of the process accepts a device connected through
// any of the adapters, and only those.
func TestAgentAnyAdapter(t *testing.T) {
	dev0 := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	dev1 := dbus.ObjectPath("/org/bluez/hci1/dev_AA_BB_CC_DD_EE_02")
	var l adapterList
	l.add(&Adapter{conns: map[dbus.ObjectPath]*conn{dev0: nil}})
	l.add(&Adapter{conns: map[dbus.ObjectPath]*conn{dev1: nil}})
	for p, want := range map[dbus.ObjectPath]bool{
		dev0: true, dev1: true, "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_02": false,
	} {
		if got := l.connected(p); got != want {
			t.Errorf("connected(%s) = %v, want %v", p, got, want)
		}
	}
}

// TestRestartShared runs two adapters against one wedged bluetoothd: both supervisors strike, but the
// restart command runs once per restartEvery for the two together.
func TestRestartShared(t *testing.T) {
	bluetoothdRestarts = restartGate{}
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, false)
	bz.setWedged(true)
	marker := filepath.Join(t.TempDir(), "restarts")
	restarts := func() int {
		b, _ := os.ReadFile(marker)
		return strings.Count(string(b), "\n")
	}
	var as []*Adapter
	for range 2 {
		a, err := Open("hci0", testOptions(Options{RestartCmd: "echo restart >> '" + marker + "'"}))
		if err != nil {
			t.Fatal(err)
		}
		a.SetScanMode(ble.ScanActive)
		stop, _ := a.Scan(func(ble.Advert) {})
		defer stop()
		as = append(as, a)
	}
	for _, a := range as {
		bletest.WaitFor(t, "failed", statusIs(a, ble.ScanFailed, ble.ScanActive))
	}
	bletest.WaitFor(t, "restart command", func() bool { return restarts() == 1 })
	n, _ := bz.calls()
	// Both supervisors keep striking; with a restart limit of their own, each would have run the command once.
	bletest.WaitFor(t, "more unanswered starts", func() bool { s, _ := bz.calls(); return s >= n+4*restartAfter })
	time.Sleep(2 * testScanRetry)
	if r := restarts(); r != 1 {
		t.Fatalf("restart command ran %d times within restartEvery", r)
	}
	bz.setWedged(false)
	for _, a := range as {
		bletest.WaitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanActive))
	}
	bz.bus.Close()
}
