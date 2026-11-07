//go:build linux

package bluez

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/bletest"
)

func TestHandleOf(t *testing.T) {
	for p, want := range map[dbus.ObjectPath]uint16{
		"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a":                   0x0a,
		"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a/char000b":          0x0b,
		"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a/char000b/desc00d0": 0xd0,
	} {
		if h, ok := handleOf(p); !ok || h != want {
			t.Errorf("handleOf(%s) = %#x, %v; want %#x", p, h, ok, want)
		}
	}
	if _, ok := handleOf("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF"); ok {
		t.Error("device path parsed as a handle")
	}
}

func TestADData(t *testing.T) {
	tx := int16(-4)
	d := device{
		flags:   []byte{0x06},
		name:    "Tion",
		txPower: &tx,
		uuids:   []string{"0000180f-0000-1000-8000-00805f9b34fb", "6e400001-b5a3-f393-e0a9-e50e24dcca9e"},
		mfr:     map[uint16][]byte{0x004c: {0x02, 0x15}},
		svcData: map[string][]byte{"0000fe95-0000-1000-8000-00805f9b34fb": {0x01}},
	}
	want := []byte{
		0x02, 0x01, 0x06,
		0x05, 0xff, 0x4c, 0x00, 0x02, 0x15,
		0x04, 0x16, 0x95, 0xfe, 0x01,
		0x03, 0x03, 0x0f, 0x18,
		0x11, 0x07, 0x9e, 0xca, 0xdc, 0x24, 0x0e, 0xe5, 0xa9, 0xe0, 0x93, 0xf3, 0xa3, 0xb5, 0x01, 0x00, 0x40, 0x6e,
		0x05, 0x09, 'T', 'i', 'o', 'n',
		0x02, 0x0a, 0xfc,
	}
	if got := d.adData(); !bytes.Equal(got, want) {
		t.Errorf("adData\n got %x\nwant %x", got, want)
	}
	d.name = string(make([]byte, 40)) // too long to fit: dropped, the rest stays
	if got := d.adData(); len(got) > ble.MaxAdvert || bytes.Contains(got, []byte{41, 0x09}) {
		t.Errorf("oversized name not dropped: %x", got)
	}
}

// TestBuildGATT checks that characteristics are reported by value handle (BlueZ's path number + 1, as an
// ESP32 reports them) while services and descriptors keep the handle from their path.
func TestBuildGATT(t *testing.T) {
	dev := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	svc, chr := dev+"/service000a", dev+"/service000a/char000b"
	uuid := func(s string) dbus.Variant { return dbus.MakeVariant(s) }
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		dev: {ifDevice: {}},
		svc: {ifService: {"UUID": uuid("0000180f-0000-1000-8000-00805f9b34fb")}},
		chr: {ifChar: {"UUID": uuid("00002a19-0000-1000-8000-00805f9b34fb"), "Service": dbus.MakeVariant(svc),
			"Flags": dbus.MakeVariant([]string{"read", "notify"}), "MTU": dbus.MakeVariant(uint16(247))}},
		chr + "/desc000e": {ifDesc: {"UUID": uuid("00002904-0000-1000-8000-00805f9b34fb"),
			"Characteristic": dbus.MakeVariant(chr)}},
		chr + "/desc000d": {ifDesc: {"UUID": uuid("00002902-0000-1000-8000-00805f9b34fb"),
			"Characteristic": dbus.MakeVariant(chr)}},
		"/org/bluez/hci0/dev_11_22_33_44_55_66/service0001": {ifService: {}}, // another device
	}
	db := buildGATT(dev, objs)
	if len(db.services) != 1 || db.services[0].Handle != 0x0a {
		t.Fatalf("services = %+v", db.services)
	}
	chars := db.services[0].Characteristics
	if len(chars) != 1 || chars[0].Handle != 0x0c || chars[0].Properties != 0x12 {
		t.Fatalf("characteristics = %+v", chars)
	}
	if d := chars[0].Descriptors; len(d) != 2 || d[0].Handle != 0x0d || d[1].Handle != 0x0e {
		t.Errorf("descriptors = %+v", d)
	}
	if db.chars[0x0c] != chr || db.descs[0x0d] != chr+"/desc000d" || db.cccdOwner[0x0d] != 0x0c || db.mtu != 247 {
		t.Errorf("maps: chars %v descs %v cccd %v mtu %d", db.chars, db.descs, db.cccdOwner, db.mtu)
	}
	if h, ok := charValueHandle(chr); !ok || h != 0x0c {
		t.Errorf("charValueHandle(%s) = %#x, %v", chr, h, ok)
	}
}

// TestReadEcho checks that the Value change BlueZ emits for our own ReadValue is not forwarded as a
// notification, whether it is seen during the read or after it, while real notifications still are.
// Value changes held during a read are capped, keeping the newest.
func TestHeldValuesCapped(t *testing.T) {
	const h = 0x0c
	p := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a/char000b")
	var got []byte
	c := &conn{notifying: map[uint16]bool{h: true}, reading: map[uint16]int{}, held: map[uint16][][]byte{},
		echoes: map[uint16][]echo{},
		ev:     ble.Events{Notification: func(_ uint16, d []byte) { got = append(got, d[0]) }}}
	c.readStarted(h)
	for i := range 2 * maxHeldValue {
		c.valueChanged(p, []byte{byte(i)})
	}
	c.readDone(h, nil, false)
	if len(got) != maxHeldValue || got[len(got)-1] != 2*maxHeldValue-1 {
		t.Fatalf("%d values, last %d", len(got), got[len(got)-1])
	}
}

func TestReadEcho(t *testing.T) {
	const h = 0x0c
	p := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a/char000b")
	var got []string
	c := &conn{notifying: map[uint16]bool{h: true}, reading: map[uint16]int{}, held: map[uint16][][]byte{},
		echoes: map[uint16][]echo{},
		ev: ble.Events{Notification: func(hh uint16, d []byte) {
			if hh != h {
				t.Errorf("notification for %#x", hh)
			}
			got = append(got, string(d))
		}}}

	// Echo seen while the read is in flight, next to a real notification.
	c.readStarted(h)
	c.valueChanged(p, []byte("n1"))
	c.valueChanged(p, []byte("r1"))
	c.readDone(h, []byte("r1"), true)
	// Echo seen after the read returned; a later notification with the same value is real.
	c.readStarted(h)
	c.readDone(h, []byte("r2"), true)
	c.valueChanged(p, []byte("n2"))
	c.valueChanged(p, []byte("r2"))
	c.valueChanged(p, []byte("r2"))
	// A failed read expects no echo.
	c.readStarted(h)
	c.readDone(h, nil, false)
	c.valueChanged(p, nil)

	want := []string{"n1", "n2", "r2", ""}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("notifications %q, want %q", got, want)
	}
}

// newTestConn is a connection to fakeDevice on the adapter, registered with it as Connect would, with one
// characteristic, fakeChar.
func newTestConn(t *testing.T, a *Adapter, ev ble.Events) *conn {
	t.Helper()
	c := &conn{a: a, path: fakeDevice, ev: ev, resolved: make(chan struct{}), lost: make(chan struct{}),
		notifying: map[uint16]bool{}, reading: map[uint16]int{}, held: map[uint16][][]byte{},
		echoes: map[uint16][]echo{}}
	c.chars = map[uint16]dbus.ObjectPath{fakeCharHandle: fakeChar}
	if err := c.rearm(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a.mu.Lock()
		if a.conns[fakeDevice] == c {
			delete(a.conns, fakeDevice)
		}
		a.mu.Unlock()
		c.unwatch()
	})
	return c
}

func connOf(a *Adapter, p dbus.ObjectPath) *conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conns[p]
}

func (c *conn) isWatched() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watched
}

func (c *conn) isNotifying(h uint16) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notifying[h]
}

// TestFirstNotificationBeforeTheReply: BlueZ's reply to StartNotify and the first Value change reach the
// adapter on different goroutines, so the value can be handled before Notify sees the reply. It must not be
// dropped, and a StartNotify that fails leaves notifications off.
func TestFirstNotificationBeforeTheReply(t *testing.T) {
	a, bz := openForRetries(t, 0, 0)
	bz.exportChar(fakeChar)
	got := make(chan []byte, 8)
	c := newTestConn(t, a, ble.Events{Notification: func(_ uint16, d []byte) { got <- d }})

	if err := c.Notify(fakeCharHandle, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the first notification was dropped")
	}
	if err := c.Notify(fakeCharHandle, false); err != nil {
		t.Fatal(err)
	}

	bz.mu.Lock()
	bz.notifyFail = true
	bz.mu.Unlock()
	if err := c.Notify(fakeCharHandle, true); err == nil {
		t.Fatal("StartNotify failure not reported")
	}
	if c.isNotifying(fakeCharHandle) {
		t.Fatal("notifying after a failed StartNotify")
	}
}

// stallDispatch holds the adapter's signal dispatch on an advertisement until the returned function is called,
// as a scan callback that does not return would. Signals sent meanwhile queue up behind it.
func stallDispatch(t *testing.T, a *Adapter, bz *fakeBlueZ) (resume func()) {
	t.Helper()
	gate, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var armed atomic.Bool
	stop, _ := a.Scan(func(ble.Advert) {
		if armed.Load() {
			once.Do(func() { close(entered) })
			<-gate
		}
	})
	bletest.WaitFor(t, "running", func() bool { return a.ScanStatus().State == ble.ScanRunning })
	armed.Store(true)
	bz.advertise()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no advertisement delivered")
	}
	var resumeOnce sync.Once
	return func() {
		resumeOnce.Do(func() {
			close(gate)
			stop()
		})
	}
}

func connectTo(t *testing.T, a *Adapter, ev ble.Events) *conn {
	t.Helper()
	c, err := a.Connect(0xAABBCCDDEEFF, ble.AddrPublic, ev)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*conn)
}

// TestDisconnectWaitsForTheLinkDownSignal: BlueZ reports Connected=false before it answers Disconnect, and
// dispatch may be behind. Disconnect keeps the device until the signal is handled, so it cannot reach the next
// connection to the device and take it down.
func TestDisconnectWaitsForTheLinkDownSignal(t *testing.T) {
	a, bz := openForRetries(t, 0, 0)
	c := connectTo(t, a, retryEvents)
	resume := stallDispatch(t, a, bz)
	defer resume()

	errc := make(chan error, 1)
	go func() { errc <- c.Disconnect() }()
	bletest.WaitFor(t, "Disconnect call", func() bool { return bz.disconnectCalls() == 1 })
	select {
	case err := <-errc:
		t.Fatalf("Disconnect returned (%v) before its Connected=false signal was handled", err)
	case <-time.After(200 * time.Millisecond):
	}
	if connOf(a, fakeDevice) != c {
		t.Fatal("device let go of before the Connected=false signal was handled")
	}
	resume()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if connOf(a, fakeDevice) != nil || c.isWatched() {
		t.Fatal("connection still registered after Disconnect")
	}

	dropped := make(chan error, 1)
	ev := retryEvents
	ev.Disconnected = func(err error) { dropped <- err }
	c2 := connectTo(t, a, ev)
	defer c2.Disconnect()
	flush(t, a, bz)
	select {
	case err := <-dropped:
		t.Fatalf("the next connection was reported lost: %v", err)
	default:
	}
}

// TestDisconnectGivesUpWaiting: a dispatch that stays behind must not hold Disconnect, or the device, for good.
func TestDisconnectGivesUpWaiting(t *testing.T) {
	old := disconnectWait
	disconnectWait = 200 * time.Millisecond
	t.Cleanup(func() { disconnectWait = old })
	a, bz := openForRetries(t, 0, 0)
	c := connectTo(t, a, retryEvents)
	resume := stallDispatch(t, a, bz)
	defer resume()

	start := time.Now()
	if err := c.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < disconnectWait || took > disconnectWait+time.Second {
		t.Fatalf("Disconnect took %v, want about %v", took, disconnectWait)
	}
	if connOf(a, fakeDevice) != nil || c.isWatched() {
		t.Fatal("connection still registered after Disconnect gave up waiting")
	}
	resume()
	flush(t, a, bz) // the late signal finds no connection
}

// TestUnpairDropsSilently: RemoveDevice takes the link down, and BlueZ reports that like any other loss. The
// caller asked for it, so it is not reported through Events.Disconnected (see ble.Adapter.Unpair).
func TestUnpairDropsSilently(t *testing.T) {
	a, bz := openForRetries(t, 0, 0)
	dropped := make(chan error, 1)
	ev := retryEvents
	ev.Disconnected = func(err error) { dropped <- err }
	c := connectTo(t, a, ev)
	if err := a.Unpair(0xAABBCCDDEEFF); err != nil {
		t.Fatal(err)
	}
	flush(t, a, bz)
	select {
	case err := <-dropped:
		t.Fatalf("the link going down was reported: %v", err)
	default:
	}
	if connOf(a, fakeDevice) != nil || c.isWatched() {
		t.Fatal("connection still registered after Unpair")
	}
	if err := a.Unpair(0xAABBCCDDEEFF); err != nil {
		t.Fatalf("unpairing a device BlueZ does not know: %v", err)
	}
}

// TestLostLinkIsTornDownBeforeTheConnectWakes: a Connect that is retried sets the connection up again as soon
// as lost closes, so the teardown of the lost link (its place on the adapter, its match rule) must be complete
// by then, or it undoes the retry's setup. The teardown needs the adapter's lock; holding it must hold lost.
func TestLostLinkIsTornDownBeforeTheConnectWakes(t *testing.T) {
	a, _ := openForRetries(t, 0, 0)
	c := newTestConn(t, a, retryEvents)
	lost := c.lostCh()
	a.mu.Lock()
	done := make(chan struct{})
	go func() {
		c.deviceChanged(map[string]dbus.Variant{"Connected": dbus.MakeVariant(false)})
		close(done)
	}()
	select {
	case <-lost:
		a.mu.Unlock()
		t.Fatal("lost closed before the connection was taken off the adapter")
	case <-time.After(100 * time.Millisecond):
	}
	a.mu.Unlock()
	<-done
	select {
	case <-lost:
	default:
		t.Fatal("lost not closed")
	}
	if connOf(a, fakeDevice) != nil || c.isWatched() {
		t.Fatal("connection still registered after the link was lost")
	}
	if err := c.rearm(); err != nil {
		t.Fatal(err)
	}
	if connOf(a, fakeDevice) != c || !c.isWatched() {
		t.Fatal("rearm did not set the connection up again")
	}
}

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
	if _, err := Open("hci0", Options{RawAdverts: RawOn}); err == nil {
		t.Fatal("opened without the HCI monitor")
	}
	bletest.WaitFor(t, "bus connection closed", func() bool { return len(busNames(t, bz.bus)) == before })
}
