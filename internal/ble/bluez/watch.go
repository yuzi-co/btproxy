//go:build linux

package bluez

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"btproxy/internal/ble"
)

// scanRetry is how often the supervisor retries when the adapter cannot scan (powered off, scan start
// refused); recovery from a bluetoothd restart or an adapter coming back is driven by signals instead.
const scanRetry = 10 * time.Second

// scanState is the scanner part of Adapter. One goroutine, supervise, makes every scan-related D-Bus call,
// so mode switches, recovery and scan start and stop never race. Fields are guarded by Adapter.mu except
// those marked as the supervisor's.
type scanState struct {
	log      *slog.Logger
	kick     chan struct{}
	mode     ble.ScanMode // requested
	status   ble.ScanStatus
	statusFn func(ble.ScanStatus)

	gone      bool // bluetoothd or the adapter is not on the bus
	off       bool // the adapter is powered off
	epoch     int  // bumped whenever BlueZ may have dropped our discovery session or monitor
	noMonitor bool // the monitor API is missing or failed: scan actively until bluetoothd restarts
	monitorUp bool // the monitor is (being) registered; a Release then means BlueZ dropped it
	agentLog  *slog.Logger // set once RegisterAgent was called

	// The supervisor's own.
	armed      bool
	armedMode  ble.ScanMode
	armedEpoch int
	lastErr    string
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
// monitor when the mode changed or BlueZ lost them, stops scanning when nobody listens.
func (a *Adapter) reconcile() {
	a.mu.Lock()
	want := a.scanFn != nil
	gone, off, epoch := a.gone, a.off, a.epoch
	a.mu.Unlock()
	mode := a.effectiveMode()
	idle := func(fail ble.ScanState) {
		if want {
			a.setStatus(fail, mode)
		} else {
			a.setStatus(ble.ScanIdle, mode)
		}
	}

	if gone {
		a.armed = false
		idle(ble.ScanFailed)
		return
	}
	if off {
		if err := a.obj(a.path).SetProperty(ifAdapter+".Powered", dbus.MakeVariant(true)); err != nil {
			a.fail("power on", err)
			a.armed = false
			idle(ble.ScanStopped)
			return
		}
		a.log.Info("adapter powered on again")
		a.mu.Lock()
		a.off = false
		a.mu.Unlock()
	}
	if a.armed && (!want || a.armedMode != mode || a.armedEpoch != epoch) {
		a.disarm()
	}
	if want && !a.armed {
		if err := a.arm(mode, epoch); err != nil {
			a.fail("start scan", err)
			a.setStatus(ble.ScanFailed, mode)
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

func (a *Adapter) arm(mode ble.ScanMode, epoch int) error {
	if mode == ble.ScanPassive {
		err := a.registerMonitors()
		if err == nil {
			a.armed, a.armedMode, a.armedEpoch = true, ble.ScanPassive, epoch
			a.log.Info("scanning passively")
			return nil
		}
		var de dbus.Error
		if errors.As(err, &de) && (de.Name == "org.freedesktop.DBus.Error.ServiceUnknown" ||
			de.Name == "org.freedesktop.DBus.Error.NoReply") {
			return err // bluetoothd is gone or stuck, which says nothing about the monitor API
		}
		a.log.Warn("passive scanning unavailable (bluetoothd needs --experimental), scanning actively", "err", err)
		a.mu.Lock()
		a.noMonitor = true
		a.mu.Unlock()
	}
	filter := map[string]any{"Transport": "le", "DuplicateData": true}
	if err := a.obj(a.path).Call(ifAdapter+".SetDiscoveryFilter", 0, filter).Err; err != nil {
		return err
	}
	if err := a.startDiscovery(); err != nil {
		return err
	}
	a.armed, a.armedMode, a.armedEpoch = true, ble.ScanActive, epoch
	a.log.Info("scanning actively")
	return nil
}

// disarm stops the current scan. Errors are ignored: after a restart BlueZ has forgotten it anyway.
func (a *Adapter) disarm() {
	if a.armedMode == ble.ScanPassive {
		a.unregisterMonitors()
	} else {
		a.obj(a.path).Call(ifAdapter+".StopDiscovery", 0)
	}
	a.armed = false
}

// emit reports a device's cached advertisement. Called with a.mu held.
func (a *Adapter) emit(d *device) {
	if a.scanFn != nil && d.rssi != 0 && d.addr != 0 {
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
			a.epoch++ // powering off ends discovery sessions
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
	v, err := a.obj(a.path).GetProperty(ifAdapter + ".Powered")
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
	a.epoch++
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
