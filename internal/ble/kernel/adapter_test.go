package kernel

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

const (
	testIndex = 0
	testMAC   = uint64(0x00_0a_cd_3f_e5_d1)
	testPeer  = uint64(0xf4_24_a2_06_a9_47)
)

// fakeKernel answers management commands and hands out fake peripherals for connects.
type fakeKernel struct {
	mu        sync.Mutex
	cmds      []uint16
	params    map[uint16][][]byte
	fail      map[uint16]byte // status to answer an opcode with
	hang      map[uint16]bool
	onEvent   func(op uint16, data []byte)
	monitors  int
	nextMon   uint16
	discovery bool
	running   atomic.Bool // bluetoothd

	dial     func(sec bool) (socket, error)
	dials    atomic.Int32
	dialSecs []bool

	emit func(ble.Advert)
	mon  *fakeMonitor
}

func newFakeKernel() *fakeKernel {
	return &fakeKernel{params: map[uint16][][]byte{}, fail: map[uint16]byte{}, hang: map[uint16]bool{}}
}

func (k *fakeKernel) openMgmt(index uint16, onEvent func(uint16, []byte)) (mgmtConn, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.onEvent = onEvent
	return k, nil
}

func (k *fakeKernel) Close() error { return nil }

func (k *fakeKernel) Command(op uint16, param []byte, _ time.Duration) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cmds = append(k.cmds, op)
	k.params[op] = append(k.params[op], append([]byte(nil), param...))
	if k.hang[op] {
		return nil, errMgmtTimeout
	}
	if st, ok := k.fail[op]; ok {
		return nil, &MgmtError{Op: op, Status: st}
	}
	switch op {
	case opReadInfo:
		rsp := make([]byte, 280)
		putAddr(rsp, testMAC)
		return rsp, nil
	case opAddAdvMonitor:
		k.nextMon++
		k.monitors++
		return []byte{byte(k.nextMon), 0}, nil
	case opRemoveAdvMonitor:
		k.monitors = 0
	case opStartDiscovery:
		k.discovery = true
	case opStopDiscovery:
		k.discovery = false
	}
	return nil, nil
}

func (k *fakeKernel) event(op uint16, data []byte) {
	k.mu.Lock()
	fn := k.onEvent
	k.mu.Unlock()
	fn(op, data)
}

func (k *fakeKernel) count(op uint16) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for _, c := range k.cmds {
		if c == op {
			n++
		}
	}
	return n
}

func (k *fakeKernel) lastParam(op uint16) []byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	p := k.params[op]
	if len(p) == 0 {
		return nil
	}
	return p[len(p)-1]
}

func (k *fakeKernel) state() (monitors int, discovery bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.monitors, k.discovery
}

func (k *fakeKernel) openMonitor(uint16) (advSource, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.mon = &fakeMonitor{stop: make(chan struct{})}
	return k.mon, nil
}

func (k *fakeKernel) dialATT(_, _ uint64, _ uint32, secure bool, _ func() bool) (socket, error) {
	k.dials.Add(1)
	k.mu.Lock()
	k.dialSecs = append(k.dialSecs, secure)
	k.mu.Unlock()
	return k.dial(secure)
}

func (k *fakeKernel) stackRunning() bool { return k.running.Load() }

type fakeMonitor struct {
	emit atomic.Pointer[func(ble.Advert)]
	stop chan struct{}
	once sync.Once
}

func (m *fakeMonitor) Run(emit func(ble.Advert), _ func() bool) error {
	m.emit.Store(&emit)
	<-m.stop
	return nil
}
func (m *fakeMonitor) Close() { m.once.Do(func() { close(m.stop) }) }

// fakeSocket adds what a socket has beyond a transport to a fake peripheral.
type fakeSocket struct {
	*peripheral
	secures atomic.Int32
}

func (s *fakeSocket) Secure() error {
	s.secures.Add(1)
	s.mu.Lock()
	s.encrypted = true
	s.mu.Unlock()
	return nil
}

func openTest(t *testing.T, k *fakeKernel, tweak func(*Options)) *Adapter {
	t.Helper()
	opt := Options{BondDir: t.TempDir(), StallTimeout: 0, ConnectRetries: 4, Log: logx.Discard(),
		tick: 5 * time.Millisecond}
	if tweak != nil {
		tweak(&opt)
	}
	a, err := open("hci0", opt, k)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("never: " + what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOpenSetsUpController(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	if a.MAC() != testMAC {
		t.Errorf("MAC %x", a.MAC())
	}
	for _, op := range []uint16{opSetLE, opSetBondable, opSetSecureConn, opSetIOCap, opLoadLTKs, opLoadIRKs, opSetPowered} {
		if k.count(op) == 0 {
			t.Errorf("command %#x not sent", op)
		}
	}
	if got := k.lastParam(opSetIOCap); !bytes.Equal(got, []byte{ioCapNoInputNoOutput}) {
		t.Errorf("IO capability % x", got)
	}
	if got := k.lastParam(opSetPowered); !bytes.Equal(got, []byte{1}) {
		t.Errorf("powered % x", got)
	}
	// Monitors left by an earlier run would keep the scan going.
	if got := k.lastParam(opRemoveAdvMonitor); !bytes.Equal(got, []byte{0, 0}) {
		t.Errorf("remove monitors % x", got)
	}
}

func TestOpenRefusesWithBluetoothd(t *testing.T) {
	k := newFakeKernel()
	k.running.Store(true)
	_, err := open("hci0", Options{BondDir: t.TempDir(), Log: logx.Discard()}, k)
	if err == nil || !strings.Contains(err.Error(), "bluetoothd") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenBadAdapterName(t *testing.T) {
	if _, err := open("usb0", Options{Log: logx.Discard()}, newFakeKernel()); err == nil {
		t.Fatal("no error")
	}
}

func TestPassiveScanUsesMonitorOnly(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	a.SetScanMode(ble.ScanPassive)
	stop, err := a.Scan(func(ble.Advert) {})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "monitor added", func() bool { m, _ := k.state(); return m == 1 })
	if _, d := k.state(); d {
		t.Error("discovery started for a passive scan")
	}
	eventually(t, "running", func() bool {
		st := a.ScanStatus()
		return st.State == ble.ScanRunning && st.Mode == ble.ScanPassive
	})
	if a.ScanSetting() != "Passive" {
		t.Errorf("setting %q", a.ScanSetting())
	}
	stop()
	eventually(t, "monitor removed", func() bool { m, _ := k.state(); return m == 0 })
}

// The monitor stays during an active scan too: it is what turns the controller's duplicate filter off.
func TestActiveScanKeepsMonitorAndRestartsDiscovery(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	a.SetScanMode(ble.ScanActive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	eventually(t, "discovery", func() bool { m, d := k.state(); return m == 1 && d })
	if got := k.lastParam(opStartDiscovery); !bytes.Equal(got, []byte{addrTypeLEAll}) {
		t.Errorf("discovery type % x", got)
	}
	// The kernel ends discovery by itself after some seconds, and for every connect.
	n := k.count(opStartDiscovery)
	k.mu.Lock()
	k.discovery = false
	k.mu.Unlock()
	k.event(evDiscovering, []byte{addrTypeLEAll, 0})
	eventually(t, "discovery restarted", func() bool { return k.count(opStartDiscovery) > n })
	a.SetScanMode(ble.ScanPassive)
	eventually(t, "discovery stopped", func() bool { m, d := k.state(); return m == 1 && !d })
}

func TestActiveWithoutMonitorSupport(t *testing.T) {
	k := newFakeKernel()
	k.fail[opAddAdvMonitor] = mgmtStatusUnknownCommand
	a := openTest(t, k, nil)
	a.SetScanMode(ble.ScanPassive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	eventually(t, "discovery instead", func() bool { _, d := k.state(); return d })
	eventually(t, "reports active", func() bool {
		return a.ScanStatus().Mode == ble.ScanActive && a.ScanSetting() == "Active, passive unavailable"
	})
}

func TestAdvertsDelivered(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	got := make(chan ble.Advert, 1)
	stop, _ := a.Scan(func(ad ble.Advert) { got <- ad })
	eventually(t, "monitor runs", func() bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		return k.mon != nil && k.mon.emit.Load() != nil
	})
	emit := *k.mon.emit.Load()
	emit(ble.Advert{Addr: 1})
	select {
	case ad := <-got:
		if ad.Addr != 1 {
			t.Errorf("%+v", ad)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no advert")
	}
	stop()
	emit(ble.Advert{Addr: 2}) // must not reach the stopped callback, or block on the full channel
	if a.AdvertSource() != "HCI monitor" {
		t.Errorf("source %q", a.AdvertSource())
	}
}

func TestStatusCallback(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	var mu sync.Mutex
	var states []ble.ScanState
	a.OnScanStatus(func(s ble.ScanStatus) {
		mu.Lock()
		states = append(states, s.State)
		mu.Unlock()
	})
	stop, _ := a.Scan(func(ble.Advert) {})
	eventually(t, "running", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(states, ble.ScanRunning)
	})
	stop()
	eventually(t, "idle", func() bool { return a.ScanStatus().State == ble.ScanIdle })
}

func conFor(t *testing.T, k *fakeKernel, a *Adapter, ev ble.Events) (*fakeSocket, ble.Conn) {
	t.Helper()
	s := &fakeSocket{peripheral: heartRate()}
	k.dial = func(bool) (socket, error) { return s, nil }
	c, err := a.Connect(testPeer, ble.AddrPublic, ev)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, c
}

func TestConnectExchangesMTUAndDoesNotDiscover(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	s, c := conFor(t, k, a, ble.Events{})
	if c.MTU() != 247 {
		t.Errorf("MTU %d", c.MTU())
	}
	if ops := s.ops(); !bytes.Equal(ops, []byte{opMTUReq}) {
		t.Errorf("connect sent % x", ops)
	}
}

func TestDisconnectedReportedOnce(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	var n atomic.Int32
	s, _ := conFor(t, k, a, ble.Events{Disconnected: func(error) { n.Add(1) }})
	s.Close()
	eventually(t, "disconnected", func() bool { return n.Load() == 1 })
}

func TestDisconnectRequestedIsSilent(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	var n atomic.Int32
	s, c := conFor(t, k, a, ble.Events{Disconnected: func(error) { n.Add(1) }})
	if err := c.Disconnect(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("socket left open")
	}
	want := append(make([]byte, 6), bdAddrLEPublic)
	putAddr(want, testPeer)
	if got := k.lastParam(opDisconnect); !bytes.Equal(got, want) {
		t.Errorf("disconnect % x, want % x", got, want)
	}
	time.Sleep(20 * time.Millisecond)
	if n.Load() != 0 {
		t.Error("Disconnected called for a requested disconnect")
	}
}

func TestDisconnectNotConnectedIsFine(t *testing.T) {
	k := newFakeKernel()
	k.fail[opDisconnect] = mgmtStatusNotConnected
	a := openTest(t, k, nil)
	_, c := conFor(t, k, a, ble.Events{})
	if err := c.Disconnect(); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestConnectRetriesFastFailures(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	s := &fakeSocket{peripheral: heartRate()}
	t.Cleanup(func() { s.Close() })
	k.dial = func(bool) (socket, error) {
		if k.dials.Load() < 3 {
			return nil, errors.New("radio")
		}
		return s, nil
	}
	if _, err := a.Connect(testPeer, ble.AddrPublic, ble.Events{}); err != nil {
		t.Fatal(err)
	}
	if k.dials.Load() != 3 {
		t.Errorf("%d dials", k.dials.Load())
	}
}

func TestConnectDoesNotRetryPermanentErrors(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	k.dial = func(bool) (socket, error) { return nil, errPermanent("no permission") }
	if _, err := a.Connect(testPeer, ble.AddrPublic, ble.Events{}); err == nil {
		t.Fatal("no error")
	}
	if k.dials.Load() != 1 {
		t.Errorf("%d dials", k.dials.Load())
	}
}

func TestConnectRetriesLinkLostDuringMTU(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	good := &fakeSocket{peripheral: heartRate()}
	t.Cleanup(func() { good.Close() })
	k.dial = func(bool) (socket, error) {
		if k.dials.Load() == 1 {
			bad := &fakeSocket{peripheral: heartRate()}
			bad.Close()
			return bad, nil
		}
		return good, nil
	}
	var disc atomic.Int32
	if _, err := a.Connect(testPeer, ble.AddrPublic, ble.Events{Disconnected: func(error) { disc.Add(1) }}); err != nil {
		t.Fatal(err)
	}
	if k.dials.Load() != 2 {
		t.Errorf("%d dials", k.dials.Load())
	}
	time.Sleep(20 * time.Millisecond)
	if disc.Load() != 0 {
		t.Error("Disconnected called for an attempt Connect retried")
	}
}

func TestConnectWithdrawnStopsRetrying(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	k.dial = func(bool) (socket, error) { return nil, errors.New("radio") }
	withdrawn := func() bool { return k.dials.Load() > 0 }
	if _, err := a.Connect(testPeer, ble.AddrPublic, ble.Events{Withdrawn: withdrawn}); err == nil {
		t.Fatal("no error")
	}
	if k.dials.Load() != 1 {
		t.Errorf("%d dials", k.dials.Load())
	}
}

func TestDiscoveryPausedWhileConnecting(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	a.SetScanMode(ble.ScanActive)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	eventually(t, "discovery", func() bool { _, d := k.state(); return d })
	s := &fakeSocket{peripheral: heartRate()}
	t.Cleanup(func() { s.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	k.dial = func(bool) (socket, error) {
		close(entered)
		<-release
		return s, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.Connect(testPeer, ble.AddrPublic, ble.Events{})
		done <- err
	}()
	<-entered
	eventually(t, "discovery stopped for the connect", func() bool { _, d := k.state(); return !d })
	n := k.count(opStartDiscovery)
	time.Sleep(30 * time.Millisecond) // several supervisor ticks
	if k.count(opStartDiscovery) != n {
		t.Error("discovery started during a connect")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	eventually(t, "discovery back", func() bool { _, d := k.state(); return d })
}

// connectSeeing connects and reports how many monitors the kernel had when the attempt reached the radio.
func connectSeeing(t *testing.T, k *fakeKernel, a *Adapter) (monitors int) {
	t.Helper()
	s := &fakeSocket{peripheral: heartRate()}
	t.Cleanup(func() { s.Close() })
	k.dial = func(bool) (socket, error) {
		monitors, _ = k.state()
		return s, nil
	}
	if _, err := a.Connect(testPeer, ble.AddrPublic, ble.Events{}); err != nil {
		t.Fatal(err)
	}
	return monitors
}

func TestMonitorRemovedBeforeConnecting(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, func(o *Options) { o.PauseOnConnect = true })
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	eventually(t, "monitor", func() bool { m, _ := k.state(); return m == 1 })
	if m := connectSeeing(t, k, a); m != 0 {
		t.Errorf("%d monitors during the connect: the kernel would scan through the first connection events", m)
	}
	eventually(t, "monitor back after the connect", func() bool { m, _ := k.state(); return m == 1 })
}

func TestMonitorStaysWithoutPause(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	eventually(t, "monitor", func() bool { m, _ := k.state(); return m == 1 })
	if m := connectSeeing(t, k, a); m != 1 {
		t.Errorf("%d monitors during the connect", m)
	}
	if n := k.count(opAddAdvMonitor); n != 1 {
		t.Errorf("monitor added %d times", n)
	}
}

// A supervisor stuck on a command must delay a connect only so long.
func TestConnectGoesAheadIfPauseIsLate(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, func(o *Options) { o.PauseOnConnect = true; o.pauseWait = 10 * time.Millisecond })
	close(a.stop) // nothing answers the kick from here on
	<-a.stopped
	a.stop = make(chan struct{}) // for the Close at the end of the test
	a.stopped = make(chan struct{})
	close(a.stopped)
	connectSeeing(t, k, a)
}

func TestPair(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	s, c := conFor(t, k, a, ble.Events{})
	if err := c.Pair(); err != nil {
		t.Fatal(err)
	}
	want := append(make([]byte, 6), bdAddrLEPublic, ioCapNoInputNoOutput)
	putAddr(want, testPeer)
	if got := k.lastParam(opPairDevice); !bytes.Equal(got, want) {
		t.Errorf("pair % x, want % x", got, want)
	}
	_ = s
}

func TestPairAlreadyPairedEncryptsInstead(t *testing.T) {
	k := newFakeKernel()
	k.fail[opPairDevice] = mgmtStatusAlreadyPaired
	a := openTest(t, k, nil)
	s, c := conFor(t, k, a, ble.Events{})
	if err := c.Pair(); err != nil {
		t.Fatal(err)
	}
	if s.secures.Load() != 1 {
		t.Errorf("%d Secure calls", s.secures.Load())
	}
}

func TestPairFailure(t *testing.T) {
	k := newFakeKernel()
	k.fail[opPairDevice] = 0x05
	a := openTest(t, k, nil)
	_, c := conFor(t, k, a, ble.Events{})
	if err := c.Pair(); err == nil {
		t.Fatal("no error")
	}
}

func ltkEvent(addr uint64, hint byte) []byte {
	b := make([]byte, 1+ltkSize)
	b[0] = hint
	putAddr(b[1:], addr)
	b[7] = bdAddrLEPublic
	b[8] = 0x02 // key type
	b[9] = 1    // central
	b[10] = 16
	for i := 19; i < len(b); i++ {
		b[i] = byte(i)
	}
	return b
}

func TestBondsStoredAndLoaded(t *testing.T) {
	dir := t.TempDir()
	k := newFakeKernel()
	a := openTest(t, k, func(o *Options) { o.BondDir = dir })
	k.event(evNewLTK, ltkEvent(testPeer, 1))
	irk := make([]byte, 7+irkSize)
	irk[0] = 1
	putAddr(irk[7:], testPeer)
	irk[13] = bdAddrLEPublic
	k.event(evNewIRK, irk)
	k.event(evNewLTK, ltkEvent(0x010203040506, 0)) // the kernel says not to keep it
	a.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	if fi, _ := os.Stat(files[0]); fi.Mode().Perm()&0o077 != 0 && !isWindows() {
		t.Errorf("bond file mode %v", fi.Mode())
	}

	k2 := newFakeKernel()
	a2 := openTest(t, k2, func(o *Options) { o.BondDir = dir })
	_ = a2
	want := append([]byte{1, 0}, ltkEvent(testPeer, 1)[1:]...)
	if got := k2.lastParam(opLoadLTKs); !bytes.Equal(got, want) {
		t.Errorf("LTKs\n got % x\nwant % x", got, want)
	}
	wantIRK := append([]byte{1, 0}, irk[7:]...)
	if got := k2.lastParam(opLoadIRKs); !bytes.Equal(got, wantIRK) {
		t.Errorf("IRKs\n got % x\nwant % x", got, wantIRK)
	}
}

func TestBondedPeerConnectsEncrypted(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	conFor(t, k, a, ble.Events{})
	k.event(evNewLTK, ltkEvent(testPeer, 1))
	conFor(t, k, a, ble.Events{})
	k.mu.Lock()
	defer k.mu.Unlock()
	if !slices.Equal(k.dialSecs, []bool{false, true}) {
		t.Errorf("secure flags %v", k.dialSecs)
	}
}

func TestUnpairForgetsAndDropsSilently(t *testing.T) {
	dir := t.TempDir()
	k := newFakeKernel()
	a := openTest(t, k, func(o *Options) { o.BondDir = dir })
	var n atomic.Int32
	s, _ := conFor(t, k, a, ble.Events{Disconnected: func(error) { n.Add(1) }})
	k.event(evNewLTK, ltkEvent(testPeer, 1))
	if err := a.Unpair(testPeer); err != nil {
		t.Fatal(err)
	}
	if k.count(opUnpairDevice) == 0 {
		t.Error("kernel not told")
	}
	select {
	case <-s.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("connection left up")
	}
	time.Sleep(20 * time.Millisecond)
	if n.Load() != 0 {
		t.Error("Disconnected called on unpair")
	}
	k2 := newFakeKernel()
	openTest(t, k2, func(o *Options) { o.BondDir = dir })
	if got := k2.lastParam(opLoadLTKs); !bytes.Equal(got, []byte{0, 0}) {
		t.Errorf("bond survived: % x", got)
	}
}

func TestUnpairUnknownDevice(t *testing.T) {
	k := newFakeKernel()
	k.fail[opUnpairDevice] = mgmtStatusNotPaired
	a := openTest(t, k, nil)
	if err := a.Unpair(testPeer); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestClearCacheIsAccepted(t *testing.T) {
	if err := openTest(t, newFakeKernel(), nil).ClearCache(testPeer); err != nil {
		t.Error(err)
	}
}

func TestConfirmRequestAccepted(t *testing.T) {
	k := newFakeKernel()
	openTest(t, k, nil)
	ev := make([]byte, 12)
	putAddr(ev, testPeer)
	ev[6] = bdAddrLERandom
	k.event(evUserConfirm, ev)
	eventually(t, "confirmed", func() bool { return k.count(opUserConfirm) == 1 })
	if got := k.lastParam(opUserConfirm); !bytes.Equal(got, ev[:7]) {
		t.Errorf("confirm % x", got)
	}
}

func TestControllerReturnIsSetUpAgain(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, nil)
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	eventually(t, "monitor", func() bool { m, _ := k.state(); return m == 1 })
	k.event(evIndexRemoved, nil)
	eventually(t, "failed", func() bool { return a.ScanStatus().State == ble.ScanFailed })
	k.mu.Lock()
	k.monitors = 0
	powered := len(k.params[opSetPowered])
	k.mu.Unlock()
	k.event(evIndexAdded, nil)
	eventually(t, "set up again", func() bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		return len(k.params[opSetPowered]) > powered && k.monitors == 1
	})
	eventually(t, "running", func() bool { return a.ScanStatus().State == ble.ScanRunning })
}

func TestStallRecoveryLadder(t *testing.T) {
	k := newFakeKernel()
	var resets atomic.Int32
	a := openTest(t, k, func(o *Options) {
		o.StallTimeout = 30 * time.Millisecond
		o.resetUSB = func(string) error { resets.Add(1); return nil }
		o.USBReset = true
		o.maxStallBackoff = 40 * time.Millisecond
	})
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	eventually(t, "monitor rebuilt first", func() bool { return k.count(opAddAdvMonitor) >= 2 })
	eventually(t, "power cycle", func() bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		for _, p := range k.params[opSetPowered] {
			if p[0] == 0 {
				return true
			}
		}
		return false
	})
	eventually(t, "USB reset", func() bool { return resets.Load() > 0 })
	if a.Recoveries() == 0 {
		t.Error("recoveries not counted")
	}
}

func TestStallDoesNotPowerCycleConnections(t *testing.T) {
	k := newFakeKernel()
	a := openTest(t, k, func(o *Options) {
		o.StallTimeout = 10 * time.Millisecond
		o.maxStallBackoff = 10 * time.Millisecond
	})
	conFor(t, k, a, ble.Events{})
	stop, _ := a.Scan(func(ble.Advert) {})
	defer stop()
	eventually(t, "scan rebuilt", func() bool { return k.count(opAddAdvMonitor) >= 3 })
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, p := range k.params[opSetPowered] {
		if p[0] == 0 {
			t.Fatal("power cycled with a live connection")
		}
	}
}

func TestRetryable(t *testing.T) {
	slow, fast := 4*time.Second, 100*time.Millisecond
	for _, tc := range []struct {
		name string
		err  error
		took time.Duration
		want bool
	}{
		{"radio, slow: the kernel waited for an advert first", errRadio, slow, true},
		{"link lost, slow", errLinkLost, slow, true},
		{"other, fast", errors.New("x"), fast, true},
		{"other, slow: a timeout", errors.New("x"), slow, false},
		{"permanent, fast", errPermanent("x"), fast, false},
	} {
		if got := retryable(tc.err, tc.took); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

// syncBuffer collects log lines from the supervisor's goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A bluetoothd that starts after the proxy, such as later in a boot, is not refused by the check in open. It gets
// one error each time it appears, not one a minute.
func TestBluetoothdStartingLaterIsReported(t *testing.T) {
	var logs syncBuffer
	k := newFakeKernel()
	openTest(t, k, func(o *Options) {
		o.Log = logx.New(&logs, logx.LevelInfo, nil)
		o.stackCheck = time.Millisecond
	})
	k.running.Store(true)
	eventually(t, "reported", func() bool { return strings.Contains(logs.String(), "bluetoothd started") })
	time.Sleep(20 * time.Millisecond)
	if n := strings.Count(logs.String(), "bluetoothd started"); n != 1 {
		t.Errorf("reported %d times:\n%s", n, logs.String())
	}
	k.running.Store(false)
	time.Sleep(20 * time.Millisecond)
	k.running.Store(true)
	eventually(t, "reported again", func() bool { return strings.Count(logs.String(), "bluetoothd started") == 2 })
}
