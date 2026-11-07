//go:build linux

package bluez

import (
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble/bletest"
)

func busNames(t *testing.T, bus *dbus.Conn) []string {
	t.Helper()
	var names []string
	if err := bus.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		t.Fatal(err)
	}
	return names
}

// TestOpenFailureClosesTheBus: an Open that fails leaves nothing behind on the bus. The HCI monitor cannot be
// opened in the tests (see raw_test.go), so RawOn fails Open once it is on the bus.
func TestOpenFailureClosesTheBus(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	defer bz.bus.Close()
	before := len(busNames(t, bz.bus))
	if _, err := Open("hci0", testOptions(Options{RawAdverts: RawOn})); err == nil {
		t.Fatal("opened without the HCI monitor")
	}
	bletest.WaitFor(t, "bus connection closed", func() bool { return len(busNames(t, bz.bus)) == before })
}
