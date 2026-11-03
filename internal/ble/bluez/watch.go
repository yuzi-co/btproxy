//go:build linux

package bluez

import (
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

// Timings, variables so the tests can shorten them. scanRetry is how often the supervisor retries when the
// adapter cannot scan (powered off, scan start refused or unanswered) and checks for a stalled scan; recovery
// from a bluetoothd restart or an adapter coming back is driven by signals instead.
var (
	scanRetry      = 10 * time.Second
	restartEvery   = 10 * time.Minute // at most one bluetoothd restart this often
	restartTimeout = 30 * time.Second
	// manualRestartEvery is how soon after any restart someone may ask for another. Shorter than restartEvery:
	// a person who asks has looked, and may have just seen the last restart fail to help.
	manualRestartEvery = time.Minute
	// pingEvery is how often the supervisor checks that bluetoothd still answers. Advertisements are no proof
	// once they come from the HCI monitor, which keeps reading the controller while bluetoothd hangs.
	pingEvery = time.Minute
)

// restartAfter is how many stalls or unanswered calls in a row make the supervisor start on the recovery steps.
const restartAfter = 2

// The recovery steps, in the order they are tried, each only when the one before did not bring scanning back.
const (
	stepPowerCycle = iota // the adapter's power, through BlueZ: needs no setup
	stepRestart           // restartCmd
	stepUSB               // the stick's USB device, which also loads its firmware again: only with resetUSB
)

// scanState is the scanner part of Adapter. One goroutine, supervise, makes every scan-related D-Bus call,
// so mode switches, recovery and scan start and stop never race. Fields are guarded by Adapter.mu except
// those marked as the supervisor's.
type scanState struct {
	log      *logx.Logger
	kick     chan struct{}
	mode     ble.ScanMode // requested
	status   ble.ScanStatus
	statusFn func(ble.ScanStatus)

	gone      bool         // bluetoothd or the adapter is not on the bus
	off       bool         // the adapter is powered off
	epoch     int          // bumped whenever BlueZ may have dropped our discovery session or monitor
	noMonitor bool         // the monitor API is missing or failed: scan actively until bluetoothd restarts, or retryMonitorAt
	monitorUp bool         // the monitor is (being) registered; a Release then means BlueZ dropped it
	agentLog  *logx.Logger // set once RegisterAgent was called

	retryMonitorAt  time.Time     // when to try the monitors again after BlueZ dropped them; zero for never
	releaseWait     time.Duration // the wait until retryMonitorAt
	lastRelease     time.Time     // when BlueZ last dropped the monitors
	ownReleaseUntil time.Time     // until when a Release comes from our own UnregisterMonitor
	adverts         int           // advertisements delivered, for the stall watchdog

	connecting   int             // connects in progress, which pause scanning (see pause.go)
	pauseWaiters []chan struct{} // connects waiting for the supervisor to stop scanning

	// Set by Open.
	stallTimeout   time.Duration
	restartCmd     string
	resetUSB       func() error // nil turns the USB step off
	pauseOnConnect bool

	// The supervisor's own.
	armed          bool
	armedMode      ble.ScanMode
	armedEpoch     int
	armedMons      bool        // monitors registered next to active discovery (see dupFilterOff)
	quietMons      atomic.Bool // the monitors registered match nothing (see quiet); read by the monitor objects
	lastErr        string
	seen           int       // adverts when reconcile last looked
	quietSince     time.Time // when seen last changed or the scan was armed
	strikes        int       // stalls and unanswered calls since bluetoothd last answered, or the last step
	step           int       // the recovery step to try next
	lastStrike     time.Time
	lastPowerCycle time.Time
	lastUSBReset   time.Time
	lastPing       time.Time     // when bluetoothd was last checked; zero forces a check
	unanswered     atomic.Int32  // calls from connections that bluetoothd did not answer, for the supervisor to count
	pausedAt       time.Time     // when scanning was stopped for a connect; zero when not paused
	restarts       atomic.Uint64 // bluetoothd restarts started, for Restarts
	recoveries     atomic.Uint64 // power cycles and USB resets done, for Recoveries
}

func (a *Adapter) initScan() error {
	a.kick = make(chan struct{}, 1)
	if err := a.exportMonitors(); err != nil {
		return err
	}
	// NameOwnerChanged tells when bluetoothd exits or starts; adapter objects coming and going arrive as
	// InterfacesAdded/Removed, which Open already subscribes to.
	if err := a.bus.AddMatchSignal(dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, busName)); err != nil {
		return err
	}
	// dispatch has not started, so NameOwnerChanged cannot overtake this. No owner is an error: bluetoothd is
	// not running, and NameOwnerChanged will tell.
	if err := a.bus.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, busName).Store(&a.owner); err != nil {
		a.owner = ""
	}
	go a.supervise()
	return nil
}

func (a *Adapter) kickScan() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// Scan delivers advertisements until stop is called. Starting, switching modes and recovering happen in
// the background; ScanStatus tells how that goes.
func (a *Adapter) Scan(fn func(ble.Advert)) (func(), error) {
	a.mu.Lock()
	a.scanFn = fn
	a.mu.Unlock()
	a.setStatus(ble.ScanStarting, a.effectiveMode())
	a.kickScan()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.scanFn = nil
			a.mu.Unlock()
			a.kickScan()
		})
	}, nil
}

func (a *Adapter) SetScanMode(m ble.ScanMode) {
	a.mu.Lock()
	a.mode = m
	a.mu.Unlock()
	a.kickScan()
}

func (a *Adapter) ScanStatus() ble.ScanStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (a *Adapter) OnScanStatus(fn func(ble.ScanStatus)) {
	a.mu.Lock()
	a.statusFn = fn
	a.mu.Unlock()
}

// effectiveMode is the requested mode unless passive scanning is known not to work.
func (a *Adapter) effectiveMode() ble.ScanMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.noMonitor {
		return ble.ScanActive
	}
	return a.mode
}

func (a *Adapter) setStatus(state ble.ScanState, mode ble.ScanMode) {
	st := ble.ScanStatus{State: state, Mode: mode}
	a.mu.Lock()
	changed := a.status != st
	a.status = st
	fn := a.statusFn
	a.mu.Unlock()
	if changed {
		a.log.Debug("scanner status", "state", state, "mode", mode)
		if fn != nil {
			fn(st)
		}
	}
}

func (a *Adapter) supervise() {
	t := time.NewTicker(scanRetry)
	defer t.Stop()
	for {
		select {
		case <-a.kick:
		case <-t.C:
		}
		a.reconcile()
	}
}

// reconcile brings the scanner to the wanted state: powers the adapter on, (re)starts discovery or the
// monitor when the mode changed or BlueZ lost them, stops scanning when nobody listens, and restarts
// discovery that has stalled. First it updates the match rules, so after a fallback from raw advertisements
// BlueZ's flow before discovery restarts.
func (a *Adapter) reconcile() {
	if err := a.syncMatches(); err != nil {
		a.log.Warn("scanner", "op", "D-Bus match rule", "err", err)
	}
	paused, waiters := a.takePause()
	defer release(waiters) // once scanning is stopped, or cannot be
	a.retryMonitors()
	a.mu.Lock()
	want := a.scanFn != nil
	gone, off, epoch, adverts, raw := a.gone, a.off, a.epoch, a.adverts, a.rawProven()
	a.mu.Unlock()
	mode := a.effectiveMode()
	idle := func(fail ble.ScanState) {
		if want {
			a.setStatus(fail, mode)
		} else {
			a.setStatus(ble.ScanIdle, mode)
		}
	}
	if adverts != a.seen {
		a.seen, a.quietSince = adverts, time.Now()
		if !raw { // BlueZ's advertisements show that bluetoothd works; the HCI monitor's do not
			a.strikes = 0
		}
	}
	if n := a.unanswered.Swap(0); n > 0 {
		a.strike()
		a.lastPing = time.Time{} // check now whether bluetoothd answers anything at all
	}

	if gone {
		a.armed = false
		idle(ble.ScanFailed)
		return
	}
	if time.Since(a.lastPing) >= pingEvery {
		a.lastPing = time.Now()
		// An answer here does not clear earlier strikes: a hung bluetoothd has been seen to leave some calls
		// unanswered while others still work.
		if _, err := a.getProperty(a.path, ifAdapter+".Powered"); isTimeout(err) {
			a.fail("health check", err)
			a.strike()
		}
	}
	if off {
		if err := a.setProperty(a.path, ifAdapter+".Powered", true); err != nil {
			a.fail("power on", err)
			a.armed = false
			if isTimeout(err) {
				idle(ble.ScanFailed)
				a.strike()
			} else {
				idle(ble.ScanStopped)
			}
			return
		}
		a.log.Info("adapter powered on again")
		a.mu.Lock()
		a.off = false
		a.mu.Unlock()
	}
	if paused {
		// Reported as idle, as an ESP32 does while it connects. The stall watchdog does not run on a
		// disarmed scan.
		if a.armed {
			start := time.Now()
			a.disarm()
			a.log.Debug("scanning paused for a connect", "took", time.Since(start).Round(time.Millisecond))
		}
		if a.pausedAt.IsZero() {
			a.pausedAt = time.Now()
		}
		a.lastErr = ""
		a.setStatus(ble.ScanIdle, mode)
		return
	}
	if a.stalled(want) {
		a.log.Warn("no advertisements, restarting discovery", "for", time.Since(a.quietSince).Round(time.Second))
		a.disarm()
		a.setStatus(ble.ScanFailed, mode)
		a.strike()
		return // armed again on the next tick
	}
	if a.armed && (!want || a.armedMode != mode || a.armedEpoch != epoch) {
		a.disarm()
	}
	if want && !a.armed {
		start := time.Now()
		if err := a.arm(mode, epoch); err != nil {
			a.fail("start scan", err)
			a.setStatus(ble.ScanFailed, mode)
			if isTimeout(err) {
				a.strike()
			}
			return
		}
		mode = a.armedMode
		if !a.pausedAt.IsZero() {
			a.log.Debug("scanning resumed after a connect", "paused", start.Sub(a.pausedAt).Round(time.Millisecond),
				"took", time.Since(start).Round(time.Millisecond))
		}
	}
	a.pausedAt = time.Time{}
	a.lastErr = ""
	idle(ble.ScanRunning) // running if wanted, idle otherwise
}

// fail logs an error once, not on every retry.
func (a *Adapter) fail(what string, err error) {
	if msg := what + ": " + err.Error(); msg != a.lastErr {
		a.lastErr = msg
		a.log.Warn("scanner", "op", what, "err", err)
	}
}

// stalled tells whether active discovery has delivered no advertisement for stallTimeout, which happens when
// bluetoothd or the controller wedges without dropping the discovery session. Passive scanning is left alone:
// BlueZ reports a device there only when its advertisement changes, so long silences are normal.
func (a *Adapter) stalled(want bool) bool {
	return want && a.armed && a.armedMode == ble.ScanActive && a.stallTimeout > 0 &&
		time.Since(a.quietSince) >= a.stallTimeout
}

// strike counts a stall or a call bluetoothd did not answer. After restartAfter in a row, each within
// restartEvery of the one before, every further strike takes a recovery step. Every step drops the connections,
// so none runs while a connect is in progress, and the strike stays counted for the next time.
func (a *Adapter) strike() {
	if time.Since(a.lastStrike) > restartEvery { // strikes far apart are unrelated
		a.strikes, a.step = 0, stepPowerCycle
	}
	a.lastStrike = time.Now()
	a.strikes++
	if a.strikes < restartAfter {
		return
	}
	a.mu.Lock()
	connecting := a.connecting > 0
	a.mu.Unlock()
	if !connecting {
		a.takeStep()
	}
}

// takeStep takes the next recovery step. Each is limited to once per restartEvery, the restart command for all
// adapters together (see bluetoothdRestarts); the recovery from a bluetoothd restart or an adapter coming back
// does the rest.
func (a *Adapter) takeStep() {
	if a.step == stepPowerCycle {
		a.step = stepRestart
		if a.powerCycle() {
			return
		}
	}
	if a.step == stepRestart {
		if a.restartCmd != "" {
			if !bluetoothdRestarts.claim() {
				return
			}
			a.step = stepUSB
			a.log.Warn("bluetoothd not responding, restarting it")
			if err := a.runRestart(); err != nil {
				a.log.Warn("bluetoothd restart failed", "err", err)
			}
			return
		}
		a.step = stepUSB
	}
	a.step, a.strikes = stepPowerCycle, 0
	if a.resetUSB == nil || !a.lastUSBReset.IsZero() && time.Since(a.lastUSBReset) < restartEvery {
		return
	}
	a.lastUSBReset = time.Now()
	a.log.Warn("still no scanning, resetting the USB device")
	if err := a.resetUSB(); err != nil {
		a.log.Warn("USB reset failed", "err", err)
		return
	}
	a.recoveries.Add(1)
}

// powerCycle turns the adapter off and on through BlueZ, and tells whether it did. A bluetoothd that does not
// answer cannot do this, so it says no at the first timeout and the caller goes on to restarting it.
func (a *Adapter) powerCycle() bool {
	if !a.lastPowerCycle.IsZero() && time.Since(a.lastPowerCycle) < restartEvery {
		return false
	}
	a.lastPowerCycle = time.Now()
	a.log.Warn("no scanning, power-cycling the adapter")
	if err := a.setProperty(a.path, ifAdapter+".Powered", false); err != nil {
		a.log.Warn("power-cycling the adapter failed", "err", err)
		return false
	}
	// If this fails the adapter stays off, and reconcile keeps trying to power it on.
	if err := a.setProperty(a.path, ifAdapter+".Powered", true); err != nil {
		a.log.Warn("power-cycling the adapter failed", "err", err)
		return false
	}
	a.recoveries.Add(1)
	return true
}

// runRestart runs restartCmd and waits for it, up to restartTimeout.
func (a *Adapter) runRestart() error {
	a.restarts.Add(1)
	// os.StartProcess rather than os/exec, which adds 128 KB to the binary. The command's output goes to
	// ours, so nothing waits on a pipe that a daemon it starts may keep open.
	p, err := os.StartProcess("/bin/sh", []string{"sh", "-c", a.restartCmd},
		&os.ProcAttr{Env: restartEnv(os.Environ()), Files: []*os.File{nil, os.Stderr, os.Stderr}})
	if err != nil {
		return err
	}
	t := time.AfterFunc(restartTimeout, func() { p.Kill() })
	st, err := p.Wait()
	t.Stop()
	if err == nil && !st.Success() {
		err = errors.New(st.String())
	}
	return err
}

// StackRestartable implements ble.StackRestarter: a restart needs the command.
func (a *Adapter) StackRestartable() bool { return a.restartCmd != "" }

// RestartStack implements ble.StackRestarter. It counts for the watchdog's limit too, and the recovery from a
// bluetoothd restart does the rest.
func (a *Adapter) RestartStack() error {
	if !bluetoothdRestarts.claimAfter(manualRestartEvery) {
		return errors.New("bluetoothd was restarted less than a minute ago")
	}
	return a.runRestart()
}

// restartEnv is the environment for the restart command: btproxy's own variables hold the encryption key and
// the settings page's password and token.
func restartEnv(env []string) []string {
	return slices.DeleteFunc(env, func(v string) bool { return strings.HasPrefix(v, "BTPROXY_") })
}

// Restarts implements ble.RestartCounter: the bluetoothd restarts the supervisor started.
func (a *Adapter) Restarts() uint64 { return a.restarts.Load() }

// Recoveries implements ble.RecoveryCounter: the power cycles and USB resets the supervisor did.
func (a *Adapter) Recoveries() uint64 { return a.recoveries.Load() }

// bluetoothdRestarts spaces bluetoothd restarts across the adapters of the process. There is one bluetoothd:
// a restart one adapter's supervisor asks for is a restart for all of them, and the others recover from it
// through NameOwnerChanged as from any restart.
var bluetoothdRestarts restartGate

type restartGate struct {
	mu   sync.Mutex
	last time.Time
}

// claim tells whether a restart may happen now, at most one per restartEvery, and if so records it.
func (g *restartGate) claim() bool { return g.claimAfter(restartEvery) }

// claimAfter is claim with another spacing.
func (g *restartGate) claimAfter(every time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() && time.Since(g.last) < every {
		return false
	}
	g.last = time.Now()
	return true
}

func (a *Adapter) arm(mode ble.ScanMode, epoch int) error {
	quiet := a.quiet()
	logf := a.log.Info
	if !a.pausedAt.IsZero() { // after every connect: only worth a line when debugging
		logf = a.log.Debug
	}
	if mode == ble.ScanPassive {
		err := a.registerMonitors(quiet)
		if err == nil {
			a.armed, a.armedMode, a.armedEpoch, a.quietSince = true, ble.ScanPassive, epoch, time.Now()
			a.rawActive.Store(false)
			logf("scanning passively", "bluetoothd_reports_devices", !quiet)
			return nil
		}
		if unreachable(err) {
			return err // bluetoothd is gone or stuck, which says nothing about the monitor API
		}
		a.log.Warn("passive scanning unavailable (bluetoothd needs --experimental), scanning actively", "err", err)
		a.mu.Lock()
		a.noMonitor = true
		a.mu.Unlock()
	}
	// Repeated advertisements matter only when they are rebuilt from Device1 properties; with the HCI monitor,
	// leaving them out spares bluetoothd and the bus a signal per advertisement.
	a.mu.Lock()
	raw, noMonitor := a.raw != nil, a.noMonitor
	a.mu.Unlock()
	if raw && !noMonitor {
		if err := a.dupFilterOff(quiet); err != nil {
			return err
		}
	}
	filter := map[string]any{"Transport": "le", "DuplicateData": !raw}
	// The kernel drops the results of a discovery below the RSSI threshold before bluetoothd sees them, after
	// the HCI monitor has. Only with the monitor API, whose --experimental also brings ConnectDevice: without
	// Device objects for the devices around, that is how they are connected.
	quiet = quiet && a.armedMons
	if quiet {
		filter["RSSI"] = int16(quietRSSI)
	}
	err := a.call(a.path, ifAdapter+".SetDiscoveryFilter", filter).Err
	if err == nil {
		err = a.startDiscovery()
	}
	if err != nil {
		if a.armedMons {
			a.unregisterMonitors()
			a.armedMons = false
		}
		return err
	}
	a.armed, a.armedMode, a.armedEpoch, a.quietSince = true, ble.ScanActive, epoch, time.Now()
	a.rawActive.Store(true)
	logf("scanning actively", "bluetoothd_reports_devices", !quiet)
	return nil
}

// ScanSetting implements ble.ScanSettinger. The fallback gets its own wording because active scanning without
// monitors is the one that costs advertisements: the controller then filters duplicates (see dupFilterOff).
func (a *Adapter) ScanSetting() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.noMonitor:
		return "Active, passive unavailable"
	case a.mode == ble.ScanActive:
		return "Active"
	}
	return "Passive"
}

// quietRSSI is the discovery RSSI threshold that keeps every device from bluetoothd: BlueZ accepts at most
// 20 dBm, which no advertisement reaches.
const quietRSSI = 20

// quiet tells whether bluetoothd can be told to report no devices, as raw advertisements have proven
// themselves (see rawProven). Every advertisement bluetoothd reports costs it, the bus and the proxy a
// D-Bus message with the device's properties, or a DeviceFound call, and a Device object for every new
// random address, while the proxy forwards the monitor's copy anyway. Not when the controller matches the
// advertisement monitors' patterns itself ("controller-patterns"): then a monitor that matches nothing
// would keep the advertisements from the HCI monitor too.
func (a *Adapter) quiet() bool {
	a.mu.Lock()
	proven := a.rawProven()
	a.mu.Unlock()
	if !proven {
		return false
	}
	v, err := a.getProperty(a.path, ifMonitorManager+".SupportedFeatures")
	if err != nil {
		return false // no monitor API, or bluetoothd is gone: arming fails or scans as before
	}
	features, _ := v.Value().([]string)
	for _, f := range features {
		if f == "controller-patterns" {
			a.log.Info("the controller filters advertisements for the monitors, bluetoothd keeps reporting devices")
			return false
		}
	}
	return true
}

// dupFilterOff registers the advertisement monitors before active discovery starts, when advertisements come
// from the HCI monitor. The kernel scans for discovery with the controller's duplicate filter on, so a
// controller reports a device about once per scan, unless an advertisement monitor is registered: then it
// turns the filter off, and the HCI monitor sees every advertisement, which presence detection needs. Without
// the monitor API (bluetoothd without --experimental) discovery runs as before.
func (a *Adapter) dupFilterOff(quiet bool) error {
	err := a.registerMonitors(quiet)
	if err == nil {
		a.armedMons = true
		return nil
	}
	if unreachable(err) {
		return err
	}
	a.log.Info("advertisement monitors unavailable, the controller filters duplicate advertisements", "err", err)
	a.mu.Lock()
	a.noMonitor = true
	a.mu.Unlock()
	return nil
}

// unreachable tells whether a call failed because bluetoothd is gone or stuck.
func unreachable(err error) bool {
	var de dbus.Error
	return isTimeout(err) || errors.As(err, &de) && (de.Name == "org.freedesktop.DBus.Error.ServiceUnknown" ||
		de.Name == "org.freedesktop.DBus.Error.NoReply")
}

// disarm stops the current scan. Errors are ignored: after a restart BlueZ has forgotten it anyway.
func (a *Adapter) disarm() {
	if a.armedMode == ble.ScanPassive {
		a.unregisterMonitors()
	} else {
		a.call(a.path, ifAdapter+".StopDiscovery")
		if a.armedMons {
			a.unregisterMonitors()
			a.armedMons = false
		}
	}
	a.armed = false
	a.rawActive.Store(false)
}

// pending is an advertisement on its way to the scan callback.
type pending struct {
	fn func(ble.Advert) // nil: nothing to deliver
	ad ble.Advert
}

// deliver calls the scan callback. Never with a.mu held: the API server calls Scan and stop with its own
// lock held, and the callback takes that lock, so the two would wait for each other.
func (p pending) deliver() {
	if p.fn != nil {
		p.fn(p.ad)
	}
}

// emit prepares a device's cached advertisement for delivery, unless the HCI monitor delivers them. Called
// with a.mu held; the caller delivers the result after releasing it.
func (a *Adapter) emit(d *device) pending {
	if a.scanFn == nil || d.rssi == 0 || d.addr == 0 || a.rawHandles() {
		return pending{}
	}
	a.adverts++
	return pending{a.scanFn, ble.Advert{Addr: d.addr, AddrType: d.addrType, RSSI: int8(d.rssi), Data: d.adData()}}
}

// watch follows bluetoothd and the adapter: it is called for every signal before dispatch handles it.
func (a *Adapter) watch(s *dbus.Signal) {
	if len(s.Body) < 2 {
		return
	}
	switch s.Name {
	case "org.freedesktop.DBus.NameOwnerChanged":
		if len(s.Body) < 3 {
			return
		}
		if name, _ := s.Body[0].(string); name != busName {
			return
		}
		owner, _ := s.Body[2].(string)
		a.mu.Lock()
		a.owner = owner
		a.mu.Unlock()
		if owner == "" {
			a.lost("bluetoothd stopped", true)
		} else {
			a.log.Info("bluetoothd started")
			a.mu.Lock()
			a.noMonitor = false // it may have been restarted with --experimental
			a.retryMonitorAt = time.Time{}
			agentLog := a.agentLog
			a.mu.Unlock()
			go func() {
				if agentLog != nil { // BlueZ forgets agents when it exits
					if err := a.RegisterAgent(agentLog); err != nil {
						a.log.Warn("pairing agent not registered again", "err", err)
					}
				}
				a.found()
			}()
		}
	case ifObjManager + ".InterfacesAdded":
		if p, _ := s.Body[0].(dbus.ObjectPath); p == a.path {
			if ifs, _ := s.Body[1].(map[string]map[string]dbus.Variant); ifs[ifAdapter] != nil {
				go a.found()
			}
		}
	case ifObjManager + ".InterfacesRemoved":
		if p, _ := s.Body[0].(dbus.ObjectPath); p == a.path {
			ifs, _ := s.Body[1].([]string)
			for _, i := range ifs {
				if i == ifAdapter {
					a.lost("adapter removed", false)
				}
			}
		}
	case ifProps + ".PropertiesChanged":
		if s.Path != a.path {
			return
		}
		if iface, _ := s.Body[0].(string); iface != ifAdapter {
			return
		}
		changed, _ := s.Body[1].(map[string]dbus.Variant)
		if v, ok := changed["Powered"]; ok {
			on, _ := v.Value().(bool)
			a.mu.Lock()
			a.off = !on
			if !on {
				a.epoch++ // powering off ends discovery sessions; powering on has none to restart
			}
			a.mu.Unlock()
			if !on {
				a.log.Warn("adapter powered off")
			}
			a.kickScan()
		}
	}
}

// lost handles bluetoothd or the adapter going away: the scanner fails and every connection is reported as
// dropped, since BlueZ will not send the Connected=false signals any more.
func (a *Adapter) lost(why string, daemon bool) {
	a.mu.Lock()
	if a.gone {
		a.mu.Unlock()
		return
	}
	a.gone = true
	a.epoch++
	a.monitorUp = false
	a.devices = map[dbus.ObjectPath]*device{}
	conns := make([]*conn, 0, len(a.conns))
	for _, c := range a.conns {
		conns = append(conns, c)
	}
	a.mu.Unlock()
	a.log.Warn(why, "connections", len(conns))
	for _, c := range conns {
		c.deviceChanged(map[string]dbus.Variant{"Connected": dbus.MakeVariant(false)})
	}
	a.kickScan()
}

// found checks whether the adapter is back after bluetoothd started or the adapter object appeared, reloads
// the device cache and lets the supervisor power it on and restart scanning.
func (a *Adapter) found() {
	v, err := a.getProperty(a.path, ifAdapter+".Powered")
	if err != nil {
		return // not there yet: InterfacesAdded follows when it is
	}
	objs, err := a.managedObjects()
	if err != nil {
		return
	}
	a.mu.Lock()
	wasGone := a.gone
	a.gone = false
	a.off = v.Value() != true
	if wasGone {
		// Once only: a bluetoothd start brings both NameOwnerChanged and InterfacesAdded, and a second bump
		// made the supervisor stop the scan it had just started and start it again.
		a.epoch++
	}
	for p, ifs := range objs {
		if props, ok := ifs[ifDevice]; ok && a.underAdapter(p) && a.devices[p] == nil {
			a.updateDevice(p, props, false)
		}
	}
	a.mu.Unlock()
	if wasGone {
		a.log.Info("adapter back")
	}
	a.kickScan()
}
