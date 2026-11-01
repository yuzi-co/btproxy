//go:build linux

package bluez

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"btproxy/internal/ble"
)

// fakeBlueZ is a minimal org.bluez on a private bus: one adapter, discovery and, if monitors is set, the
// advertisement monitor manager. Each instance is one bluetoothd run; closing it is bluetoothd exiting.
type fakeBlueZ struct {
	t        *testing.T
	bus      *dbus.Conn
	monitors bool

	mu          sync.Mutex
	powered     bool
	discovering bool
	registered  int // monitors BlueZ accepted (read through GetManagedObjects)
}

const fakeAdapter = dbus.ObjectPath("/org/bluez/hci0")

func startFakeBlueZ(t *testing.T, addr string, monitors bool) *fakeBlueZ {
	t.Helper()
	bus, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBlueZ{t: t, bus: bus, monitors: monitors, powered: true}
	bus.Export(fakeRoot{f}, "/", ifObjManager)
	bus.Export(fakeProps{f}, fakeAdapter, ifProps)
	bus.Export(fakeAdapterIf{f}, fakeAdapter, ifAdapter)
	if monitors {
		bus.Export(fakeMonitorMgr{f}, fakeAdapter, ifMonitorManager)
	}
	if r, err := bus.RequestName(busName, dbus.NameFlagDoNotQueue); err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("request name: %v %v", r, err)
	}
	return f
}

func (f *fakeBlueZ) state() (powered, discovering bool, registered int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.powered, f.discovering, f.registered
}

func (f *fakeBlueZ) setPowered(on bool) {
	f.mu.Lock()
	f.powered = on
	if !on {
		f.discovering = false
	}
	f.mu.Unlock()
	f.bus.Emit(fakeAdapter, ifProps+".PropertiesChanged", ifAdapter,
		map[string]dbus.Variant{"Powered": dbus.MakeVariant(on)}, []string{})
}

type fakeRoot struct{ f *fakeBlueZ }

func (r fakeRoot) GetManagedObjects() (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, *dbus.Error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return map[dbus.ObjectPath]map[string]map[string]dbus.Variant{fakeAdapter: {ifAdapter: {
		"Address": dbus.MakeVariant("00:11:22:33:44:55"), "Powered": dbus.MakeVariant(r.f.powered)}}}, nil
}

type fakeProps struct{ f *fakeBlueZ }

func (p fakeProps) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	switch name {
	case "Address":
		return dbus.MakeVariant("00:11:22:33:44:55"), nil
	case "Powered":
		return dbus.MakeVariant(p.f.powered), nil
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", nil)
}

func (p fakeProps) Set(iface, name string, v dbus.Variant) *dbus.Error {
	if name == "Powered" {
		go p.f.setPowered(v.Value() == true)
	}
	return nil
}

type fakeAdapterIf struct{ f *fakeBlueZ }

func (fakeAdapterIf) SetDiscoveryFilter(map[string]dbus.Variant) *dbus.Error { return nil }

func (a fakeAdapterIf) StartDiscovery() *dbus.Error {
	a.f.mu.Lock()
	defer a.f.mu.Unlock()
	if !a.f.powered {
		return dbus.NewError("org.bluez.Error.NotReady", nil)
	}
	a.f.discovering = true
	return nil
}

func (a fakeAdapterIf) StopDiscovery() *dbus.Error {
	a.f.mu.Lock()
	a.f.discovering = false
	a.f.mu.Unlock()
	return nil
}

type fakeMonitorMgr struct{ f *fakeBlueZ }

// RegisterMonitor reads the application's objects as BlueZ does and counts the valid monitors.
func (m fakeMonitorMgr) RegisterMonitor(sender dbus.Sender, root dbus.ObjectPath) *dbus.Error {
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := m.f.bus.Object(string(sender), root).Call(ifObjManager+".GetManagedObjects", 0).Store(&objs); err != nil {
		return dbus.NewError("org.bluez.Error.Failed", []any{err.Error()})
	}
	n := 0
	for _, ifs := range objs {
		var ps []orPattern
		if p, ok := ifs[ifMonitor]["Patterns"]; ok && p.Store(&ps) == nil && len(ps) > 0 && len(ps) <= 7 {
			n++
		}
	}
	m.f.mu.Lock()
	m.f.registered = n
	m.f.mu.Unlock()
	return nil
}

func (m fakeMonitorMgr) UnregisterMonitor(dbus.ObjectPath) *dbus.Error {
	m.f.mu.Lock()
	m.f.registered = 0
	m.f.mu.Unlock()
	return nil
}

// startBus runs a private dbus-daemon and points the system bus address at it.
func startBus(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon not installed")
	}
	cmd := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1")
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	addr = strings.TrimSpace(addr)
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", addr)
	os.Unsetenv("DBUS_STARTER_ADDRESS")
	return addr
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func statusIs(a *Adapter, state ble.ScanState, mode ble.ScanMode) func() bool {
	return func() bool { return a.ScanStatus() == ble.ScanStatus{State: state, Mode: mode} }
}

// TestRecovery runs the scanner against a fake bluetoothd: passive scanning through the monitor, a mode
// switch, a power-off, bluetoothd exiting and coming back without the monitor API (active fallback), and
// once more with it.
func TestRecovery(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	a, err := Open("hci0")
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanPassive)
	stop, _ := a.Scan(func(ble.Advert) {})

	waitFor(t, "passive scan", statusIs(a, ble.ScanRunning, ble.ScanPassive))
	if _, disc, reg := bz.state(); disc || reg != len(monitorPatterns) {
		t.Fatalf("discovering %v, %d monitors registered", disc, reg)
	}

	a.SetScanMode(ble.ScanActive)
	waitFor(t, "active scan", statusIs(a, ble.ScanRunning, ble.ScanActive))
	if _, disc, reg := bz.state(); !disc || reg != 0 {
		t.Fatalf("discovering %v, %d monitors registered", disc, reg)
	}

	bz.setPowered(false)
	waitFor(t, "re-power", func() bool { on, disc, _ := bz.state(); return on && disc })
	waitFor(t, "running after re-power", statusIs(a, ble.ScanRunning, ble.ScanActive))

	a.SetScanMode(ble.ScanPassive)
	bz.bus.Close() // bluetoothd exits
	waitFor(t, "failed", func() bool { return a.ScanStatus().State == ble.ScanFailed })

	bz = startFakeBlueZ(t, addr, false) // back without --experimental
	waitFor(t, "active fallback", statusIs(a, ble.ScanRunning, ble.ScanActive))
	if _, disc, _ := bz.state(); !disc {
		t.Fatal("not discovering after fallback")
	}

	bz.bus.Close()
	waitFor(t, "failed again", func() bool { return a.ScanStatus().State == ble.ScanFailed })
	bz = startFakeBlueZ(t, addr, true) // back with --experimental: passive again
	waitFor(t, "passive after restart", statusIs(a, ble.ScanRunning, ble.ScanPassive))

	stop()
	waitFor(t, "idle", statusIs(a, ble.ScanIdle, ble.ScanPassive))
	if _, disc, reg := bz.state(); disc || reg != 0 {
		t.Fatalf("after stop: discovering %v, %d monitors registered", disc, reg)
	}
	bz.bus.Close()
}
