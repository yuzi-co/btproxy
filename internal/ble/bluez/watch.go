//go:build linux

package bluez

import (
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"btproxy/internal/ble"
)

// Timings, variables so the tests can shorten them. scanRetry is how often the supervisor retries when the
// adapter cannot scan (powered off, scan start refused or unanswered) and checks for a stalled scan; recovery
// from a bluetoothd restart or an adapter coming back is driven by signals instead.
var (
	scanRetry      = 10 * time.Second
	restartEvery   = 10 * time.Minute // at most one bluetoothd restart this often
	restartTimeout = 30 * time.Second
)

// restartAfter is how many stalls or unanswered calls in a row make the supervisor restart bluetoothd.
const restartAfter = 2

// scanState is the scanner part of Adapter. One goroutine, supervise, makes every scan-related D-Bus call,
// so mode switches, recovery and scan start and stop never race. Fields are guarded by Adapter.mu except
// those marked as the supervisor's.
type scanState struct {
	log      *slog.Logger
	kick     chan struct{}
	mode     ble.ScanMode // requested
	status   ble.ScanStatus
	statusFn func(ble.ScanStatus)

	gone      bool         // bluetoothd or the adapter is not on the bus
	off       bool         // the adapter is powered off
	epoch     int          // bumped whenever BlueZ may have dropped our discovery session or monitor
	noMonitor bool         // the monitor API is missing or failed: scan actively until bluetoothd restarts
	monitorUp bool         // the monitor is (being) registered; a Release then means BlueZ dropped it
	agentLog  *slog.Logger // set once RegisterAgent was called
	adverts   int          // advertisements delivered, for the stall watchdog

	// Set by Open.
	stallTimeout time.Duration
	restartCmd   string

	// The supervisor's own.
	armed       bool
	armedMode   ble.ScanMode
	armedEpoch  int
	lastErr     string
	seen        int       // adverts when reconcile last looked
	quietSince  time.Time // when seen last changed or the scan was armed
	strikes     int       // stalls and unanswered calls since the last advertisement or restart
	lastRestart time.Time
}

func (a *Adapter) initScan() error {
	a.log = slog.Default().With("adapter", string(a.path))
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
// discovery that has stalled.
func (a *Adapter) reconcile() {
	a.mu.Lock()
	want := a.scanFn != nil
	gone, off, epoch, adverts := a.gone, a.off, a.epoch, a.adverts
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
		a.seen, a.quietSince, a.strikes = adverts, time.Now(), 0
	}

	if gone {
		a.armed = false
		idle(ble.ScanFailed)
		return
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
		if err := a.arm(mode, epoch); err != nil {
			a.fail("start scan", err)
			a.setStatus(ble.ScanFailed, mode)
			if isTimeout(err) {
				a.strike()
			}
			return
		}
		mode = a.armedMode
	}
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

// strike counts a stall or a call bluetoothd did not answer. After restartAfter in a row it restarts
// bluetoothd with restartCmd, at most once per restartEvery; the recovery from a bluetoothd restart does the
// rest.
func (a *Adapter) strike() {
	a.strikes++
	if a.restartCmd == "" || a.strikes < restartAfter ||
		(!a.lastRestart.IsZero() && time.Since(a.lastRestart) < restartEvery) {
		return
	}
	a.strikes = 0
	a.lastRestart = time.Now()
	a.log.Warn("bluetoothd not responding, restarting it", "cmd", a.restartCmd)
	// os.StartProcess rather than os/exec, which adds 128 KB to the binary. The command's output goes to
	// ours, so nothing waits on a pipe that a daemon it starts may keep open.
	p, err := os.StartProcess("/bin/sh", []string{"sh", "-c", a.restartCmd},
		&os.ProcAttr{Files: []*os.File{nil, os.Stderr, os.Stderr}})
	if err != nil {
		a.log.Warn("bluetoothd restart failed", "err", err)
		return
	}
	t := time.AfterFunc(restartTimeout, func() { p.Kill() })
	st, err := p.Wait()
	t.Stop()
	if err == nil && !st.Success() {
		err = errors.New(st.String())
	}
	if err != nil {
		a.log.Warn("bluetoothd restart failed", "err", err)
	}
}

func (a *Adapter) arm(mode ble.ScanMode, epoch int) error {
	if mode == ble.ScanPassive {
		err := a.registerMonitors()
		if err == nil {
			a.armed, a.armedMode, a.armedEpoch, a.quietSince = true, ble.ScanPassive, epoch, time.Now()
			a.log.Info("scanning passively")
			return nil
		}
		var de dbus.Error
		if isTimeout(err) || errors.As(err, &de) && (de.Name == "org.freedesktop.DBus.Error.ServiceUnknown" ||
			de.Name == "org.freedesktop.DBus.Error.NoReply") {
			return err // bluetoothd is gone or stuck, which says nothing about the monitor API
		}
		a.log.Warn("passive scanning unavailable (bluetoothd needs --experimental), scanning actively", "err", err)
		a.mu.Lock()
		a.noMonitor = true
		a.mu.Unlock()
	}
	filter := map[string]any{"Transport": "le", "DuplicateData": true}
	if err := a.call(a.path, ifAdapter+".SetDiscoveryFilter", filter).Err; err != nil {
		return err
	}
	if err := a.startDiscovery(); err != nil {
		return err
	}
	a.armed, a.armedMode, a.armedEpoch, a.quietSince = true, ble.ScanActive, epoch, time.Now()
	a.log.Info("scanning actively")
	return nil
}

// disarm stops the current scan. Errors are ignored: after a restart BlueZ has forgotten it anyway.
func (a *Adapter) disarm() {
	if a.armedMode == ble.ScanPassive {
		a.unregisterMonitors()
	} else {
		a.call(a.path, ifAdapter+".StopDiscovery")
	}
	a.armed = false
}

// emit reports a device's cached advertisement. Called with a.mu held.
func (a *Adapter) emit(d *device) {
	if a.scanFn != nil && d.rssi != 0 && d.addr != 0 {
		a.adverts++
		a.scanFn(ble.Advert{Addr: d.addr, AddrType: d.addrType, RSSI: int8(d.rssi), Data: d.adData()})
	}
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
		if owner, _ := s.Body[2].(string); owner == "" {
			a.lost("bluetoothd stopped", true)
		} else {
			a.log.Info("bluetoothd started")
			a.mu.Lock()
			a.noMonitor = false // it may have been restarted with --experimental
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
