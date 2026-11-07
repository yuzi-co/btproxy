package kernel

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

const (
	defaultTick     = 5 * time.Second
	defaultMaxStall = 30 * time.Minute
	// defaultPauseWait is how long a connect waits for scanning to stop before it goes ahead anyway.
	defaultPauseWait = 2 * time.Second
	// defaultPauseLimit bounds a pause, should a connect hang beyond its own timeouts.
	defaultPauseLimit = connectTimeout + mtuTimeout
	// defaultStackCheck is how often the supervisor looks for a bluetoothd that started after the proxy.
	defaultStackCheck = time.Minute

	connectTimeout = 30 * time.Second // until the kernel reports the link up
	mtuTimeout     = 10 * time.Second
)

var (
	// errLinkLost is a link that came up and dropped before it could be used.
	errLinkLost = fmt.Errorf("%w right after connecting", ble.ErrLinkLost)
	// errRadio is HCI reason 0x3e, connection failed to be established: the first connection events did not get
	// through. The kernel waits for an advertisement before it connects, so this can come after some seconds.
	errRadio = errors.New("connection failed to be established (HCI 0x3e)")
)

// retryable tells whether a connect error is worth another attempt: a link that came up and dropped, or HCI
// reason 0x3e. Neither is held to a fast cut-off (Connect retries with ble.ConnectRetry.Fast at the budget):
// the kernel waits for an advertisement before it connects, so a 0x3e can come after some seconds. Everything
// else, timeouts included, fails at once.
func retryable(err error) bool {
	var perm permanentError
	if errors.As(err, &perm) {
		return false
	}
	return errors.Is(err, ble.ErrLinkLost) || errors.Is(err, errRadio)
}

// permanentError is a connect failure that another attempt will not change.
type permanentError struct{ msg string }

func (e permanentError) Error() string { return e.msg }

func errPermanent(msg string) error { return permanentError{msg} }

// Options configures Open.
type Options struct {
	// BondDir holds the keys of bonded devices.
	BondDir string
	// BlueZDir is where BlueZ keeps its bonds, to copy from the first time this controller is used. Empty for none.
	BlueZDir string
	ble.WatchdogOptions
	ble.ConnectOptions // PauseOnConnect stops the monitor too (see applyScan)
	// Log receives the adapter's log lines; nil discards them.
	Log *logx.Logger

	// Timings the tests shorten; zero takes the default.
	tick            time.Duration
	maxStallBackoff time.Duration
	pauseWait       time.Duration
	pauseLimit      time.Duration
	stackCheck      time.Duration
	resetUSB        func(adapter string) error
}

// socket is an L2CAP socket on the ATT channel.
type socket interface {
	transport
	// Secure encrypts the link, pairing if there is no bond, and returns once that is done or failed.
	Secure() error
}

// advSource delivers advertisements as received, the HCI monitor outside the tests.
type advSource interface {
	Run(emit func(ble.Advert), active func() bool) error
	Close()
}

// system is what the adapter needs from the operating system; the tests replace it.
type system interface {
	openMgmt(index uint16, onEvent func(op uint16, data []byte)) (mgmtConn, error)
	openMonitor(index uint16) (advSource, error)
	// dialATT connects to a device. withdrawn, if it returns true, ends the attempt early.
	dialATT(local, addr uint64, addrType uint32, secure bool, withdrawn func() bool) (socket, error)
	// stackRunning tells whether bluetoothd is running.
	stackRunning() bool
}

// Adapter implements ble.Adapter on the kernel's Bluetooth stack.
type Adapter struct {
	name  string
	index uint16
	opt   Options
	log   *logx.Logger
	sys   system
	mgmt  mgmtConn
	mac   uint64
	bonds *bondStore
	retry ble.ConnectRetry

	dialMu sync.Mutex // the kernel makes one LE connection at a time

	scan  ble.ScanCell  // the mode and status reported; ActiveOnly once the kernel turns out to have no advertisement monitors
	pause ble.ScanPause // connects in progress, during which discovery must not start

	mu     sync.Mutex
	scanFn func(ble.Advert)
	conns  map[uint64]*conn

	// Supervisor state: the supervisor goroutine alone writes these, except where noted.
	present     bool   // the controller exists (mu)
	needSetup   bool   // the controller came back and has not been set up (mu)
	monitor     uint16 // handle of the monitor registered; 0 if none
	discovering bool   // (mu)
	wedged      bool   // a command timed out (mu)
	stallStep   int
	stallNext   time.Time
	stallWait   time.Duration
	stackSeen   bool // bluetoothd was running at the last check

	lastAdvert atomic.Int64 // UnixNano
	activeScan atomic.Bool
	recoveries atomic.Uint64

	source   advSource
	kick     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	stopped  chan struct{}
}

var (
	_ ble.Adapter         = (*Adapter)(nil)
	_ ble.ModeScanner     = (*Adapter)(nil)
	_ ble.AdvertSourcer   = (*Adapter)(nil)
	_ ble.ScanSettinger   = (*Adapter)(nil)
	_ ble.RecoveryCounter = (*Adapter)(nil)
)

func open(name string, opt Options, sys system) (*Adapter, error) {
	n, ok := strings.CutPrefix(name, "hci")
	idx, err := strconv.ParseUint(n, 10, 16)
	if !ok || err != nil {
		return nil, fmt.Errorf("no controller index in adapter name %q", name)
	}
	if sys.stackRunning() {
		return nil, errors.New("bluetoothd is running and manages the adapter: stop it, or use -backend bluez")
	}
	if opt.tick == 0 {
		opt.tick = defaultTick
	}
	if opt.maxStallBackoff == 0 {
		opt.maxStallBackoff = defaultMaxStall
	}
	if opt.pauseWait == 0 {
		opt.pauseWait = defaultPauseWait
	}
	if opt.pauseLimit == 0 {
		opt.pauseLimit = defaultPauseLimit
	}
	if opt.stackCheck == 0 {
		opt.stackCheck = defaultStackCheck
	}
	a := &Adapter{name: name, index: uint16(idx), opt: opt, log: opt.Log, sys: sys,
		conns: map[uint64]*conn{}, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		stopped: make(chan struct{}), present: true}
	// No fast cut-off short of the budget, see retryable.
	a.retry = opt.Retry(ble.ConnectBudget)
	a.scan.Log = a.log
	a.pause = ble.ScanPause{Pause: opt.PauseOnConnect, Wait: opt.pauseWait, Limit: opt.pauseLimit,
		Kick: a.kickSupervisor, Log: a.log}
	a.lastAdvert.Store(time.Now().UnixNano())
	a.mgmt, err = sys.openMgmt(a.index, a.event)
	if err != nil {
		return nil, fmt.Errorf("management socket: %w", err)
	}
	fail := func(err error) (*Adapter, error) {
		a.mgmt.Close()
		return nil, err
	}
	info, err := a.mgmt.Command(opReadInfo, nil, mgmtTimeout)
	if err != nil {
		return fail(fmt.Errorf("adapter %s: %w", name, err))
	}
	if len(info) < 6 {
		return fail(fmt.Errorf("adapter %s: short controller information", name))
	}
	a.mac = ble.AddrFromLE(info)
	if a.bonds, err = openBonds(opt.BondDir, a.mac); err != nil {
		return fail(fmt.Errorf("bonds: %w", err))
	}
	if opt.BlueZDir != "" {
		a.bonds.copyBlueZ(opt.BlueZDir, a.mac, a.log)
	}
	if err := a.setup(); err != nil {
		return fail(fmt.Errorf("adapter %s: %w", name, err))
	}
	if a.source, err = sys.openMonitor(a.index); err != nil {
		return fail(fmt.Errorf("HCI monitor: %w", err))
	}
	go a.runSource(a.source)
	go a.supervise()
	return a, nil
}

// setup puts the controller in the state the proxy needs, at the start and whenever it comes back. It also
// loads the bonds, since the kernel forgets them with the controller.
func (a *Adapter) setup() error {
	steps := []struct {
		op    uint16
		param []byte
	}{
		{opSetLE, []byte{1}},
		{opSetBondable, []byte{1}},
		{opSetSecureConn, []byte{1}},
		{opSetIOCap, []byte{ioCapNoInputNoOutput}},
		{opLoadLTKs, a.bonds.ltkParam()},
		{opLoadIRKs, a.bonds.irkParam()},
		{opRemoveAdvMonitor, []byte{0, 0}}, // left over from an earlier run; the kernel may not have the command
		{opSetPowered, []byte{1}},
	}
	for _, s := range steps {
		if _, err := a.mgmt.Command(s.op, s.param, mgmtTimeout); err != nil {
			switch s.op {
			case opSetSecureConn, opLoadIRKs, opRemoveAdvMonitor: // not every controller or kernel has these
				a.log.Debug("optional setup step failed", "op", s.op, "err", err)
			default:
				return err
			}
		}
	}
	a.monitor = 0
	return nil
}

// Close stops the adapter. Connections stay up until their sockets close with the process.
func (a *Adapter) Close() {
	a.stopOnce.Do(func() {
		close(a.stop)
		<-a.stopped
		a.mu.Lock()
		s := a.source
		a.mu.Unlock()
		if s != nil {
			s.Close()
		}
		a.mgmt.Close()
	})
}

// MAC implements ble.Adapter.
func (a *Adapter) MAC() uint64 { return a.mac }

func (a *Adapter) kickSupervisor() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// event handles what the management socket reports. It runs on the socket's reader, so a command sent from
// here would wait for an answer only that reader can deliver.
func (a *Adapter) event(op uint16, data []byte) {
	switch op {
	case evIndexRemoved:
		a.mu.Lock()
		a.present = false
		a.discovering = false
		a.mu.Unlock()
		a.log.Warn("adapter removed")
		a.scan.SetState(ble.ScanFailed)
		a.kickSupervisor()
	case evIndexAdded:
		a.mu.Lock()
		a.present, a.needSetup = true, true
		a.mu.Unlock()
		a.log.Info("adapter is back")
		a.kickSupervisor()
	case evDiscovering:
		if len(data) >= 2 {
			a.mu.Lock()
			a.discovering = data[1] != 0
			a.mu.Unlock()
			a.kickSupervisor()
		}
	case evNewSettings:
		if len(data) >= 4 && data[0]&settingPowered == 0 {
			a.mu.Lock()
			a.needSetup = true
			a.mu.Unlock()
			a.kickSupervisor()
		}
	case evNewLTK:
		if len(data) == 1+ltkSize && data[0] != 0 {
			key := append([]byte(nil), data[1:]...)
			if err := a.bonds.putLTK(key); err != nil {
				a.log.Warn("bond not saved", "addr", describe(key), "err", err)
			}
		}
	case evNewIRK:
		if len(data) == 7+irkSize && data[0] != 0 {
			key := append([]byte(nil), data[7:]...)
			if err := a.bonds.putIRK(key); err != nil {
				a.log.Warn("bond not saved", "addr", describe(key), "err", err)
			}
		}
	case evUserConfirm:
		if len(data) >= 7 { // Just Works: the proxy has nothing to compare, like the BlueZ agent
			addr := append([]byte(nil), data[:7]...)
			go func() {
				if _, err := a.mgmt.Command(opUserConfirm, addr, mgmtTimeout); err != nil {
					a.log.Warn("pairing confirmation refused", "addr", describe(addr), "err", err)
				}
			}()
		}
	}
}

// runSource passes on advertisements from the monitor, which outlives scans and reads every controller event.
// A socket that fails is opened again by the supervisor.
func (a *Adapter) runSource(s advSource) {
	err := s.Run(a.advert, a.activeScan.Load)
	if err != nil {
		a.log.Warn("HCI monitor failed", "err", err)
		a.mu.Lock()
		if a.source == s {
			a.source = nil
		}
		a.mu.Unlock()
		a.kickSupervisor()
	}
}

// advert delivers one advertisement. The callback runs without a.mu: the API server calls Scan and stop with
// its own lock held, which the callback takes too.
func (a *Adapter) advert(ad ble.Advert) {
	a.lastAdvert.Store(time.Now().UnixNano())
	a.mu.Lock()
	fn := a.scanFn
	a.mu.Unlock()
	if fn != nil {
		fn(ad)
	}
}

// Scan implements ble.Adapter.
func (a *Adapter) Scan(fn func(ble.Advert)) (func(), error) {
	a.mu.Lock()
	a.scanFn = fn
	a.mu.Unlock()
	a.lastAdvert.Store(time.Now().UnixNano())
	a.scan.SetState(ble.ScanStarting)
	a.kickSupervisor()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.scanFn = nil
			a.mu.Unlock()
			a.kickSupervisor()
		})
	}, nil
}

// SetScanMode implements ble.ModeScanner.
func (a *Adapter) SetScanMode(m ble.ScanMode) {
	a.scan.SetMode(m)
	a.kickSupervisor()
}

// ScanStatus implements ble.ModeScanner.
func (a *Adapter) ScanStatus() ble.ScanStatus { return a.scan.Status() }

// OnScanStatus implements ble.ModeScanner.
func (a *Adapter) OnScanStatus(fn func(ble.ScanStatus)) { a.scan.OnStatus(fn) }

// AdvertSource implements ble.AdvertSourcer.
func (a *Adapter) AdvertSource() string { return "HCI monitor" }

// ScanSetting implements ble.ScanSettinger.
func (a *Adapter) ScanSetting() string { return a.scan.Setting() }

// Recoveries implements ble.RecoveryCounter: the power cycles and USB resets the supervisor did.
func (a *Adapter) Recoveries() uint64 { return a.recoveries.Load() }

// supervise brings the controller to the state wanted, whenever something changes and on a timer.
func (a *Adapter) supervise() {
	defer close(a.stopped)
	t := time.NewTicker(a.opt.tick)
	defer t.Stop()
	stack := time.NewTicker(a.opt.stackCheck)
	defer stack.Stop()
	for {
		a.reconcile()
		select {
		case <-a.stop:
			return
		case <-a.kick:
		case <-t.C:
		case <-stack.C:
			a.checkStack()
		}
	}
}

// checkStack reports a bluetoothd that started after the proxy, which open cannot refuse. Nothing here can stop it,
// and exiting would only take the proxy away too, so this leaves the choice to whoever reads the error.
func (a *Adapter) checkStack() {
	running := a.sys.stackRunning()
	if running && !a.stackSeen {
		a.log.Error("bluetoothd started and now drives the adapter too, which can break scanning and connections: stop it, or restart the proxy with -backend bluez")
	}
	a.stackSeen = running
}

func (a *Adapter) reconcile() {
	a.mu.Lock()
	present, needSetup, want := a.present, a.needSetup, a.scanFn != nil
	noSource := a.source == nil
	a.mu.Unlock()
	connecting := a.pause.Connecting()
	paused, release := a.pause.Take()
	defer release() // once scanning is stopped, or cannot be
	if !present {
		return
	}
	if noSource {
		if s, err := a.sys.openMonitor(a.index); err != nil {
			a.log.Warn("HCI monitor", "err", err)
		} else {
			a.mu.Lock()
			a.source = s
			a.mu.Unlock()
			go a.runSource(s)
		}
	}
	if needSetup {
		if err := a.setup(); err != nil {
			a.log.Warn("adapter setup failed", "err", err)
			a.noteTimeout(err)
			a.scan.SetState(ble.ScanFailed)
			if want {
				a.checkStall(true)
			}
			return
		}
		a.mu.Lock()
		a.needSetup, a.discovering = false, false
		a.mu.Unlock()
	}
	err := a.applyScan(want, connecting, paused)
	a.noteTimeout(err)
	switch {
	case err != nil:
		a.log.Warn("scan", "err", err)
		a.scan.SetState(ble.ScanFailed)
		if want {
			a.checkStall(true)
		}
	case want:
		a.scan.SetState(ble.ScanRunning)
		a.checkStall(false)
	default:
		a.stallStep = 0
		a.scan.SetState(ble.ScanIdle) // nothing is wrong: no client wants scanning, or it is switched off
	}
}

func (a *Adapter) noteTimeout(err error) {
	if errors.Is(err, errMgmtTimeout) {
		a.mu.Lock()
		a.wedged = true
		a.mu.Unlock()
	}
}

// applyScan registers or removes the monitor, and starts or stops discovery. Discovery does not start during a
// connect, which the kernel refuses or fails.
//
// The monitor goes too while scanning is paused for a connect (see ble.ScanPause). The kernel stops scanning to
// create a connection, but while a monitor exists it scans again within milliseconds of the link coming up. A
// controller busy scanning right then can miss the first six connection events, and the link fails with HCI
// reason 0x3e. Connecting does not need the monitor: the kernel scans for the device it connects to by itself.
func (a *Adapter) applyScan(want, connecting, paused bool) error {
	wantMonitor := want && !paused
	if wantMonitor && a.monitor == 0 && !a.scan.ActiveOnly() {
		rsp, err := a.mgmt.Command(opAddAdvMonitor, quietMonitor(), mgmtTimeout)
		switch {
		case isStatus(err, mgmtStatusUnknownCommand):
			a.scan.SetActiveOnly(true)
			a.log.Warn("this kernel has no advertisement monitors, scanning actively only")
		case err != nil:
			return err
		case len(rsp) >= 2:
			a.monitor = uint16(rsp[0]) | uint16(rsp[1])<<8
		}
	}
	if !wantMonitor && a.monitor != 0 {
		h := a.monitor
		a.monitor = 0
		if _, err := a.mgmt.Command(opRemoveAdvMonitor, []byte{byte(h), byte(h >> 8)}, mgmtTimeout); err != nil {
			return err
		}
	}
	active := want && a.scan.Effective() == ble.ScanActive
	a.mu.Lock()
	discovering := a.discovering
	a.mu.Unlock()
	a.activeScan.Store(active)
	switch {
	case discovering && (!active || connecting):
		_, err := a.mgmt.Command(opStopDiscovery, []byte{addrTypeLEAll}, mgmtTimeout)
		a.mu.Lock()
		a.discovering = false
		a.mu.Unlock()
		if err != nil && !isStatus(err, mgmtStatusRejected) {
			return err
		}
	case !discovering && active && !connecting:
		_, err := a.mgmt.Command(opStartDiscovery, []byte{addrTypeLEAll}, mgmtTimeout)
		if isStatus(err, mgmtStatusBusy) { // the kernel is still ending the last one; the next pass gets there
			return nil
		}
		if err != nil {
			return err
		}
		a.mu.Lock()
		a.discovering = true
		a.mu.Unlock()
	}
	return nil
}

// checkStall brings scanning back after a silence: first by registering the scan again, then by power-cycling
// the controller, then by resetting its USB device, waiting longer after each round. An empty neighbourhood
// looks the same, and so a live connection is not worth risking: with one, only the first step runs. failing says
// that the commands of this pass failed: the controller is the problem then, so the connection does not hold back
// recovery, though the steps are still spaced out.
func (a *Adapter) checkStall(failing bool) {
	if a.opt.StallTimeout <= 0 {
		return
	}
	now := time.Now()
	last := time.Unix(0, a.lastAdvert.Load())
	a.mu.Lock()
	wedged := a.wedged
	a.wedged = false
	connected := len(a.conns) > 0
	a.mu.Unlock()
	if !failing && !wedged && now.Sub(last) < a.opt.StallTimeout {
		a.stallStep, a.stallWait = 0, 0
		return
	}
	if (failing || !wedged) && now.Before(a.stallNext) {
		return
	}
	if a.stallWait == 0 {
		a.stallWait = a.opt.StallTimeout
	}
	a.log.Warn("no advertisements or failing commands, trying to bring scanning back",
		"silent", now.Sub(last).Round(time.Second), "step", a.stallStep, "failing", failing)
	step := a.stallStep
	if connected && !failing {
		step = 0
	}
	switch step {
	case 0:
		a.rebuildScan()
	case 1:
		a.powerCycle()
	default:
		a.resetUSB()
	}
	a.stallStep++
	if a.stallStep > 2 {
		a.stallStep = 0
		a.stallWait = min(a.stallWait*2, a.opt.maxStallBackoff)
	}
	a.stallNext = time.Now().Add(a.stallWait)
}

// rebuildScan drops the monitor and any discovery; the next pass registers them again.
func (a *Adapter) rebuildScan() {
	if a.monitor != 0 {
		h := a.monitor
		a.monitor = 0
		if _, err := a.mgmt.Command(opRemoveAdvMonitor, []byte{byte(h), byte(h >> 8)}, mgmtTimeout); err != nil {
			a.log.Warn("removing the monitor", "err", err)
		}
	}
	a.mu.Lock()
	discovering := a.discovering
	a.discovering = false
	a.mu.Unlock()
	if discovering {
		if _, err := a.mgmt.Command(opStopDiscovery, []byte{addrTypeLEAll}, mgmtTimeout); err != nil {
			a.log.Warn("stopping discovery", "err", err)
		}
	}
	a.kickSupervisor()
}

func (a *Adapter) powerCycle() {
	a.recoveries.Add(1)
	a.log.Warn("power-cycling the adapter")
	if _, err := a.mgmt.Command(opSetPowered, []byte{0}, mgmtTimeout); err != nil {
		a.log.Warn("powering the adapter off", "err", err)
	}
	a.mu.Lock()
	a.needSetup = true
	a.mu.Unlock()
	a.kickSupervisor()
}

func (a *Adapter) resetUSB() {
	if !a.opt.USBReset || a.opt.resetUSB == nil {
		a.powerCycle()
		return
	}
	a.recoveries.Add(1)
	a.log.Warn("resetting the adapter's USB device")
	if err := a.opt.resetUSB(a.name); err != nil {
		a.log.Warn("USB reset", "err", err)
	}
}

// ClearCache implements ble.Adapter. The proxy keeps no services across connections, so there is nothing to
// forget, and Home Assistant asks a device's services again on the next connection.
func (a *Adapter) ClearCache(uint64) error { return nil }

// Unpair implements ble.Adapter.
func (a *Adapter) Unpair(addr uint64) error {
	a.mu.Lock()
	c := a.conns[addr]
	a.mu.Unlock()
	if c != nil {
		c.silence()
	}
	types, err := a.bonds.forget(addr)
	if err != nil {
		a.log.Warn("bond file not updated", "addr", ble.FormatMAC(addr), "err", err)
	}
	if len(types) == 0 {
		types = []byte{bdAddrLEPublic, bdAddrLERandom}
	}
	var failed error
	for _, t := range types {
		// The parameter's last byte asks the kernel to drop any connection with the bond.
		_, err := a.mgmt.Command(opUnpairDevice, append(addrInfo(addr, t), 1), mgmtTimeout)
		if err != nil && !isStatus(err, mgmtStatusNotPaired) {
			failed = err
		}
	}
	if c != nil {
		c.close()
	}
	return failed
}

// Connect implements ble.Adapter. Nothing is discovered here: Home Assistant usually has its own copy of the
// services, and every round trip on a new link is a chance to lose it.
func (a *Adapter) Connect(addr uint64, addrType uint32, ev ble.Events) (ble.Conn, error) {
	defer a.pause.Begin()()
	a.dialMu.Lock()
	defer a.dialMu.Unlock()
	if ev.Withdrawn != nil && ev.Withdrawn() {
		return nil, errors.New("connect withdrawn")
	}
	var c *conn
	err := a.retry.Run(a.log.With("addr", ble.FormatMAC(addr)), func() error {
		var err error
		c, err = a.dial(addr, addrType, ev)
		return err
	}, retryable, ev.Withdrawn)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// dial makes one connection attempt, up to and including the MTU exchange that proves the link works.
func (a *Adapter) dial(addr uint64, addrType uint32, ev ble.Events) (*conn, error) {
	s, err := a.sys.dialATT(a.mac, addr, addrType, a.bonds.has(addr), ev.Withdrawn)
	if err != nil {
		return nil, err
	}
	c := &conn{a: a, addr: addr, bdType: bdType(addrType), sock: s, ev: ev, up: make(chan struct{})}
	c.attClient = newATTClient(s, ev.Notification)
	c.raise = s.Secure
	c.timeout = attTimeout
	go c.loop()
	saved := c.timeout
	c.timeout = mtuTimeout
	err = c.exchangeMTU(maxMTU)
	c.timeout = saved
	var ae *ATTError
	switch {
	case errors.As(err, &ae): // the device has no MTU exchange: the link works, at the default MTU
	case err != nil:
		return nil, c.abandon(err)
	}
	// The loop removes a link that ended, so one that ended before this point must not be added after it.
	a.mu.Lock()
	select {
	case <-c.done:
		a.mu.Unlock()
		return nil, c.abandon(c.linkErr())
	default:
	}
	a.conns[addr] = c
	a.mu.Unlock()
	close(c.up)
	return c, nil
}

// abandon ends a link Connect will not hand over, and releases its loop without reporting anything.
func (c *conn) abandon(err error) error {
	c.silence()
	c.close()
	close(c.up)
	return fmt.Errorf("%w: %v", errLinkLost, err)
}

// conn implements ble.Conn: ATT and GATT come from attClient.
type conn struct {
	*attClient
	a      *Adapter
	addr   uint64
	bdType byte
	sock   socket
	ev     ble.Events
	up     chan struct{} // closed when Connect hands the connection over
	quiet  atomic.Bool   // the end of the link is not reported
	once   sync.Once
}

// loop runs the ATT client until the link ends, and reports that unless the proxy ended it. Events.Disconnected
// must not arrive for a link Connect is still deciding about.
func (c *conn) loop() {
	reason := c.run()
	c.a.mu.Lock()
	if c.a.conns[c.addr] == c {
		delete(c.a.conns, c.addr)
	}
	c.a.mu.Unlock()
	c.close()
	if c.quiet.Load() {
		return
	}
	select {
	case <-c.up:
	case <-c.a.stop:
		return
	}
	if !c.quiet.Load() && c.ev.Disconnected != nil {
		c.ev.Disconnected(reason)
	}
}

func (c *conn) silence() { c.quiet.Store(true) }

func (c *conn) close() { c.once.Do(func() { c.sock.Close() }) }

// Pair implements ble.Conn.
func (c *conn) Pair() error {
	param := append(addrInfo(c.addr, c.bdType), ioCapNoInputNoOutput)
	_, err := c.a.mgmt.Command(opPairDevice, param, pairTimeout)
	if isStatus(err, mgmtStatusAlreadyPaired) { // a bond exists, but this link may not be using it yet
		err = c.sock.Secure()
	}
	if err != nil {
		return err
	}
	c.raiseMu.Lock()
	c.raised = true
	c.raiseMu.Unlock()
	return nil
}

// Disconnect implements ble.Conn. Closing the socket alone leaves the link up for some seconds, in case another
// socket wants it.
func (c *conn) Disconnect() error {
	c.silence()
	c.a.mu.Lock()
	if c.a.conns[c.addr] == c {
		delete(c.a.conns, c.addr)
	}
	c.a.mu.Unlock()
	_, err := c.a.mgmt.Command(opDisconnect, addrInfo(c.addr, c.bdType), mgmtTimeout)
	c.close()
	if isStatus(err, mgmtStatusNotConnected) {
		return nil
	}
	return err
}

func isWindows() bool { return runtime.GOOS == "windows" }
