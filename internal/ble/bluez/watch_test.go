//go:build linux

package bluez

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
)

func init() {
	// Before any adapter exists: the supervisors of earlier tests keep running and read these.
	scanRetry = 50 * time.Millisecond
	callTimeout = 300 * time.Millisecond
	pingEvery = 100 * time.Millisecond
	monitorRetry = 400 * time.Millisecond
}

// fakeBlueZ is a minimal org.bluez on a private bus: one adapter, discovery and, if monitors is set, the
// advertisement monitor manager. Each instance is one bluetoothd run; closing it is bluetoothd exiting.
type fakeBlueZ struct {
	t        *testing.T
	bus      *dbus.Conn
	monitors bool
	offload  bool // the controller matches monitor patterns ("controller-patterns")

	mu          sync.Mutex
	powered     bool
	discovering bool
	registered  int                      // monitors BlueZ accepted (read through GetManagedObjects)
	starts      int                      // StartDiscovery calls
	stops       int                      // StopDiscovery calls
	dupData     bool                     // DuplicateData of the last discovery filter
	wedge       chan struct{}            // while set, StartDiscovery waits for it to close, as a wedged bluetoothd does
	hung        chan struct{}            // while set, property reads and writes wait for it to close too
	onPowerOff  func()                   // called when a client powers the adapter off
	devices     map[dbus.ObjectPath]bool // announced with InterfacesAdded
	connectFail bool                     // Device1.Connect fails
	dropLinks   int                      // this many connects succeed, then lose the link before services resolve
	rssi        int16                    // RSSI threshold of the last discovery filter, 0 for none
	quietMons   bool                     // the monitors registered match nothing
	monSender   string                   // who registered the monitors
	connects    []string                 // Device1.Connect and Adapter1.ConnectDevice calls, by method
	hold        chan struct{}            // while set, connecting waits for a value from it
	scanning    []bool                   // per connect: discovering or monitors registered when it started

	exportMu sync.Mutex // godbus's Export is not safe for concurrent use, which concurrent connects make
}

const fakeAdapter = dbus.ObjectPath("/org/bluez/hci0")

func startFakeBlueZ(t *testing.T, addr string, monitors bool) *fakeBlueZ {
	t.Helper()
	bus, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBlueZ{t: t, bus: bus, monitors: monitors, powered: true, devices: map[dbus.ObjectPath]bool{}}
	bus.Export(fakeRoot{f}, "/", ifObjManager)
	bus.Export(fakeProps{f}, fakeAdapter, ifProps)
	bus.Export(fakeAdapterIf{f}, fakeAdapter, ifAdapter)
	if monitors {
		bus.Export(fakeMonitorMgr{f}, fakeAdapter, ifMonitorManager)
	}
	if r, err := bus.RequestName(busName, dbus.NameFlagDoNotQueue); err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("request name: %v %v", r, err)
	}
	// Like bluetoothd, announce the adapter after taking the name.
	bus.Emit("/", ifObjManager+".InterfacesAdded", fakeAdapter, map[string]map[string]dbus.Variant{ifAdapter: {
		"Address": dbus.MakeVariant("00:11:22:33:44:55"), "Powered": dbus.MakeVariant(true)}})
	return f
}

func (f *fakeBlueZ) calls() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

// setWedged makes StartDiscovery stop answering, or answer again (the calls waiting so far reply then, too
// late for the caller).
func (f *fakeBlueZ) setWedged(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if on && f.wedge == nil {
		f.wedge = make(chan struct{})
	} else if !on && f.wedge != nil {
		close(f.wedge)
		f.wedge = nil
	}
}

const fakeDevice = fakeAdapter + "/dev_AA_BB_CC_DD_EE_FF"

// advertise reports an advertisement from one device, as BlueZ does during discovery.
func (f *fakeBlueZ) advertise() { f.advertiseFrom(fakeDevice, -60) }

// advertiseFrom reports an advertisement: InterfacesAdded the first time BlueZ sees a device, as it creates
// its object then, and PropertiesChanged afterwards.
func (f *fakeBlueZ) advertiseFrom(p dbus.ObjectPath, rssi int16) {
	addr := strings.ReplaceAll(strings.TrimPrefix(string(p), string(fakeAdapter)+"/dev_"), "_", ":")
	props := map[string]dbus.Variant{"Address": dbus.MakeVariant(addr), "RSSI": dbus.MakeVariant(rssi)}
	f.mu.Lock()
	known := f.devices[p]
	f.devices[p] = true
	f.mu.Unlock()
	if known {
		f.bus.Emit(p, ifProps+".PropertiesChanged", ifDevice, props, []string{})
		return
	}
	f.export(p)
	f.bus.Emit("/", ifObjManager+".InterfacesAdded", p, map[string]map[string]dbus.Variant{ifDevice: props})
}

// removeDevice drops a device, as BlueZ does when it has not seen one for a while.
func (f *fakeBlueZ) removeDevice(p dbus.ObjectPath) {
	f.mu.Lock()
	delete(f.devices, p)
	f.mu.Unlock()
	f.bus.Emit("/", ifObjManager+".InterfacesRemoved", p, []string{ifDevice})
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
	hung := p.f.hung
	p.f.mu.Unlock()
	if hung != nil {
		<-hung
	}
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	switch name {
	case "Address":
		return dbus.MakeVariant("00:11:22:33:44:55"), nil
	case "Powered":
		return dbus.MakeVariant(p.f.powered), nil
	case "SupportedFeatures":
		if p.f.offload {
			return dbus.MakeVariant([]string{"controller-patterns"}), nil
		}
		if p.f.monitors {
			return dbus.MakeVariant([]string{}), nil
		}
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", nil)
}

func (p fakeProps) Set(iface, name string, v dbus.Variant) *dbus.Error {
	p.f.mu.Lock()
	hung, onOff := p.f.hung, p.f.onPowerOff
	p.f.mu.Unlock()
	if hung != nil {
		<-hung
	}
	if name == "Powered" {
		if v.Value() != true && onOff != nil {
			onOff()
		}
		go p.f.setPowered(v.Value() == true)
	}
	return nil
}

type fakeAdapterIf struct{ f *fakeBlueZ }

func (a fakeAdapterIf) SetDiscoveryFilter(f map[string]dbus.Variant) *dbus.Error {
	a.f.mu.Lock()
	a.f.dupData = f["DuplicateData"].Value() == true
	a.f.rssi, _ = f["RSSI"].Value().(int16)
	a.f.mu.Unlock()
	return nil
}

// ConnectDevice creates and connects a device BlueZ has no object for.
func (a fakeAdapterIf) ConnectDevice(props map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	addr, _ := props["Address"].Value().(string)
	p := fakeAdapter + dbus.ObjectPath("/dev_"+strings.ReplaceAll(addr, ":", "_"))
	a.f.mu.Lock()
	a.f.connects = append(a.f.connects, "ConnectDevice")
	known := a.f.devices[p]
	a.f.mu.Unlock()
	if known {
		return "", dbus.NewError("org.bluez.Error.AlreadyExists", nil)
	}
	a.f.advertiseFrom(p, -60)
	d := fakeDeviceIf{a.f, p}
	return p, d.connected()
}

// hideDevice creates a device object without announcing it, as when the signal is still on its way.
func (f *fakeBlueZ) hideDevice(p dbus.ObjectPath) {
	f.mu.Lock()
	f.devices[p] = true
	f.mu.Unlock()
	f.export(p)
}

func (f *fakeBlueZ) export(p dbus.ObjectPath) {
	f.exportMu.Lock()
	defer f.exportMu.Unlock()
	f.bus.Export(fakeDeviceIf{f, p}, p, ifDevice)
}

func (f *fakeBlueZ) connectCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.connects...)
}

func (a fakeAdapterIf) StartDiscovery() *dbus.Error {
	a.f.mu.Lock()
	a.f.starts++
	wedge := a.f.wedge
	a.f.mu.Unlock()
	if wedge != nil {
		<-wedge
	}
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
	a.f.stops++
	a.f.mu.Unlock()
	return nil
}

type fakeDeviceIf struct {
	f *fakeBlueZ
	p dbus.ObjectPath
}

// Connect announces the connection and its resolved services before replying, as BlueZ does.
func (d fakeDeviceIf) Connect() *dbus.Error {
	d.f.mu.Lock()
	d.f.connects = append(d.f.connects, "Connect")
	d.f.mu.Unlock()
	return d.connected()
}

func (d fakeDeviceIf) connected() *dbus.Error {
	d.f.mu.Lock()
	d.f.scanning = append(d.f.scanning, d.f.discovering || d.f.registered > 0)
	hold := d.f.hold
	d.f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	d.f.mu.Lock()
	fail := d.f.connectFail
	drop := d.f.dropLinks > 0
	if drop {
		d.f.dropLinks--
	}
	d.f.mu.Unlock()
	if fail {
		return dbus.NewError("org.bluez.Error.Failed", []any{"le-connection-abort-by-local"})
	}
	if drop {
		d.f.bus.Emit(d.p, ifProps+".PropertiesChanged", ifDevice, map[string]dbus.Variant{
			"Connected": dbus.MakeVariant(true)}, []string{})
		go func() {
			time.Sleep(20 * time.Millisecond)
			d.f.bus.Emit(d.p, ifProps+".PropertiesChanged", ifDevice, map[string]dbus.Variant{
				"Connected": dbus.MakeVariant(false)}, []string{})
		}()
		return nil
	}
	d.f.bus.Emit(d.p, ifProps+".PropertiesChanged", ifDevice, map[string]dbus.Variant{
		"Connected": dbus.MakeVariant(true), "ServicesResolved": dbus.MakeVariant(true)}, []string{})
	return nil
}

func (d fakeDeviceIf) Disconnect() *dbus.Error {
	d.f.bus.Emit(d.p, ifProps+".PropertiesChanged", ifDevice, map[string]dbus.Variant{
		"Connected": dbus.MakeVariant(false), "ServicesResolved": dbus.MakeVariant(false)}, []string{})
	return nil
}

type fakeMonitorMgr struct{ f *fakeBlueZ }

// RegisterMonitor reads the application's objects as BlueZ does and counts the valid monitors.
func (m fakeMonitorMgr) RegisterMonitor(sender dbus.Sender, root dbus.ObjectPath) *dbus.Error {
	m.f.mu.Lock()
	already := m.f.registered > 0
	m.f.mu.Unlock()
	if already {
		return dbus.NewError("org.bluez.Error.AlreadyExists", []any{"Already Exists"})
	}
	var objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	if err := m.f.bus.Object(string(sender), root).Call(ifObjManager+".GetManagedObjects", 0).Store(&objs); err != nil {
		return dbus.NewError("org.bluez.Error.Failed", []any{err.Error()})
	}
	n, quiet := 0, true
	for _, ifs := range objs {
		var ps []orPattern
		if p, ok := ifs[ifMonitor]["Patterns"]; ok && p.Store(&ps) == nil && len(ps) > 0 && len(ps) <= 7 {
			n++
			for _, p := range ps {
				quiet = quiet && p.Type == 0xff && strings.HasPrefix(string(p.Content), "\xff\xffbtproxy")
			}
		}
	}
	m.f.mu.Lock()
	m.f.registered, m.f.quietMons, m.f.monSender = n, quiet && n > 0, string(sender)
	m.f.mu.Unlock()
	return nil
}

// UnregisterMonitor tells each monitor it is released, as BlueZ does, without waiting for an answer.
func (m fakeMonitorMgr) UnregisterMonitor(dbus.ObjectPath) *dbus.Error {
	m.f.mu.Lock()
	hung := m.f.hung
	m.f.mu.Unlock()
	if hung != nil {
		<-hung
	}
	m.f.releaseMonitors()
	return nil
}

func (f *fakeBlueZ) releaseMonitors() {
	f.mu.Lock()
	n, sender := f.registered, f.monSender
	f.registered = 0
	f.mu.Unlock()
	for i := 0; i < n; i++ {
		f.bus.Object(sender, monitorPath(i)).Go(ifMonitor+".Release", dbus.FlagNoReplyExpected, nil)
	}
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
	a, err := Open("hci0", Options{})
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
	time.Sleep(5 * scanRetry) // the bluetoothd start and the adapter's appearance must not both restart it
	if starts, stops := bz.calls(); starts != 1 || stops != 0 {
		t.Fatalf("after bluetoothd came back: %d StartDiscovery, %d StopDiscovery calls", starts, stops)
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

// TestUnanswered has StartDiscovery go unanswered, as with a wedged bluetoothd: the scanner fails within the
// call timeout instead of starting for ever, runs the restart command, at most once per restartEvery, and
// runs again once StartDiscovery is answered.
func TestUnanswered(t *testing.T) {
	bluetoothdRestarts = restartGate{} // shared by the process: an earlier test's restart counts
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, false)
	bz.setWedged(true)
	marker := filepath.Join(t.TempDir(), "restarts")
	restarts := func() int {
		b, _ := os.ReadFile(marker)
		return strings.Count(string(b), "\n")
	}
	a, err := Open("hci0", Options{RestartCmd: "echo restart >> '" + marker + "'"})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanActive)
	start := time.Now()
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()

	waitFor(t, "failed", statusIs(a, ble.ScanFailed, ble.ScanActive))
	if d := time.Since(start); d > callTimeout+time.Second {
		t.Fatalf("failed only after %v", d)
	}
	waitFor(t, "restart command", func() bool { return restarts() == 1 })
	n, _ := bz.calls()
	waitFor(t, "more unanswered starts", func() bool { s, _ := bz.calls(); return s >= n+2*restartAfter })
	if r := restarts(); r != 1 {
		t.Fatalf("restart command ran %d times within restartEvery", r)
	}
	if r := a.Restarts(); r != 1 {
		t.Fatalf("Restarts() = %d, want 1", r)
	}

	bz.setWedged(false)
	waitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanActive))
	bz.bus.Close()
}

// TestStall stops advertisements during active discovery: the scanner reports a failure and restarts
// discovery, while a steady flow of advertisements leaves it alone.
func TestStall(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, false)
	a, err := Open("hci0", Options{StallTimeout: 400 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var failures atomic.Int32
	a.OnScanStatus(func(st ble.ScanStatus) {
		if st.State == ble.ScanFailed {
			failures.Add(1)
		}
	})
	a.SetScanMode(ble.ScanActive)
	var got atomic.Int32
	stop, _ := a.Scan(func(ble.Advert) { got.Add(1) })
	defer stop()
	waitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanActive))

	for i := 0; i < 10; i++ {
		bz.advertise()
		time.Sleep(100 * time.Millisecond)
	}
	if got.Load() == 0 {
		t.Fatal("no advertisements delivered")
	}
	if starts, stops := bz.calls(); starts != 1 || stops != 0 || failures.Load() != 0 {
		t.Fatalf("while advertisements flowed: %d starts, %d stops, %d failures", starts, stops, failures.Load())
	}

	waitFor(t, "discovery restarted", func() bool { s, st := bz.calls(); return s >= 2 && st >= 1 })
	waitFor(t, "running again", statusIs(a, ble.ScanRunning, ble.ScanActive))
	if failures.Load() == 0 {
		t.Fatal("stall not reported as a failure")
	}
	bz.bus.Close()
}

// TestCallbackOutsideLock has the scan callback take a lock that is held while Scan is called, as the API
// server does: the callback must not run with the adapter's lock held, or the two wait for each other.
func TestCallbackOutsideLock(t *testing.T) {
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, false)
	defer bz.bus.Close()
	a, err := Open("hci0", Options{RawAdverts: RawOff})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanActive)
	var server sync.Mutex // the API server's lock
	var armed atomic.Bool
	entered := make(chan struct{})
	var once sync.Once
	stop, _ := a.Scan(func(ble.Advert) {
		if armed.Load() {
			once.Do(func() { close(entered) })
		}
		server.Lock()
		server.Unlock()
	})
	waitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanActive))

	server.Lock()
	armed.Store(true)
	bz.advertise()
	select {
	case <-entered: // the callback now waits for server
	case <-time.After(5 * time.Second):
		server.Unlock()
		t.Fatal("no advertisement delivered")
	}
	done := make(chan struct{})
	go func() {
		stop() // the server stops scanning with its lock held
		stop, _ = a.Scan(func(ble.Advert) {})
		close(done)
	}()
	select {
	case <-done:
		server.Unlock()
		stop()
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: Scan and stop wait for the adapter lock the advertisement callback holds")
	}
}

// TestHealthCheck hangs bluetoothd while passive scanning runs, where no stall watchdog applies and
// advertisements from the HCI monitor would keep flowing: the periodic check must notice and restart it.
func TestHealthCheck(t *testing.T) {
	bluetoothdRestarts = restartGate{} // shared by the process: an earlier test's restart counts
	addr := startBus(t)
	bz := startFakeBlueZ(t, addr, true)
	marker := filepath.Join(t.TempDir(), "restarts")
	restarts := func() int {
		b, _ := os.ReadFile(marker)
		return strings.Count(string(b), "\n")
	}
	a, err := Open("hci0", Options{RestartCmd: "echo restart >> '" + marker + "'"})
	if err != nil {
		t.Fatal(err)
	}
	a.SetScanMode(ble.ScanPassive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	waitFor(t, "running", statusIs(a, ble.ScanRunning, ble.ScanPassive))
	time.Sleep(5 * pingEvery)
	if r := restarts(); r != 0 {
		t.Fatalf("restart command ran %d times while bluetoothd answered", r)
	}
	hung := make(chan struct{})
	bz.mu.Lock()
	bz.hung = hung
	bz.mu.Unlock()
	waitFor(t, "restart command", func() bool { return restarts() == 1 })
	bz.mu.Lock()
	bz.hung = nil
	bz.mu.Unlock()
	close(hung)
	bz.bus.Close()
}

// Signals are believed only from the bus daemon and bluetoothd, however they are addressed.
func TestTrusted(t *testing.T) {
	a := &Adapter{}
	for sender, want := range map[string]bool{"org.freedesktop.DBus": true, ":1.5": false, "": false} {
		if got := a.trusted(&dbus.Signal{Sender: sender}); got != want {
			t.Errorf("no owner, sender %q: %v", sender, got)
		}
	}
	a.owner = ":1.5"
	for sender, want := range map[string]bool{"org.freedesktop.DBus": true, ":1.5": true, ":1.6": false, "": false} {
		if got := a.trusted(&dbus.Signal{Sender: sender}); got != want {
			t.Errorf("sender %q: %v", sender, got)
		}
	}
}

// The device cache stops growing, and never forgets a connected device.
func TestMaxDevices(t *testing.T) {
	const connected = dbus.ObjectPath("/org/bluez/hci0/dev_connected")
	a := &Adapter{devices: map[dbus.ObjectPath]*device{}, conns: map[dbus.ObjectPath]*conn{connected: {}}}
	a.updateDevice(connected, nil, false)
	for i := range 2 * maxDevices {
		a.updateDevice(dbus.ObjectPath("/org/bluez/hci0/dev_"+strconv.Itoa(i)), nil, false)
		if len(a.devices) > maxDevices {
			t.Fatalf("%d devices", len(a.devices))
		}
		if a.devices[connected] == nil {
			t.Fatal("connected device forgotten")
		}
	}
}

// The restart command does not inherit btproxy's secrets.
func TestRestartEnv(t *testing.T) {
	got := restartEnv([]string{"PATH=/bin", "BTPROXY_ENCRYPTION_KEY=k", "HOME=/root", "BTPROXY_UI_PASSWORD=p", "BTPROXY_UI_STATE=a t"})
	if strings.Join(got, " ") != "PATH=/bin HOME=/root" {
		t.Fatalf("%q", got)
	}
}
