package api

import (
	"strings"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// Besides the sensors in stats.go the proxy reports its health, and lets Home Assistant turn scanning on and off
// and restart things. All of them work per adapter, as each adapter is its own ESPHome device.

// entityConfig is ESPHome's configuration entity category.
const entityConfig = 1

// maxStateText is the longest state Home Assistant accepts.
const maxStateText = 255

// scanSwitchID is the object id of the scanning switch.
const scanSwitchID = "scanning"

// restartDropTimeout bounds the wait for connections to drop before a restart. A variable, so the tests can
// shorten it. A stuck Bluetooth stack is the likely reason for the restart, so it must not be able to stop it.
var restartDropTimeout = 5 * time.Second

// binarySensor is a state that is on or off.
type binarySensor struct {
	id, name, icon, class string
	value                 func() bool
}

// textSensor is a state in words.
type textSensor struct {
	id, name, icon string
	value          func() string
}

// button runs press in its own goroutine, since it may wait for the radio.
type button struct {
	id, name, icon string
	press          func(c *client)
}

// entityKey is the key of an entity, an FNV-1 hash of the object id as ESPHome computes it, so it stays the
// same across restarts and versions.
func entityKey(id string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(id); i++ {
		h *= 16777619
		h ^= uint32(id[i])
	}
	return h
}

// initEntities builds the entities that are not sensors. The restart buttons are disabled by default in Home
// Assistant, because without an encryption key any host that can reach the API could press them.
func (s *Server) initEntities() {
	s.binarySensors = []binarySensor{
		{id: "bluetooth_stack", name: "Bluetooth stack", icon: "mdi:bluetooth", class: "running", value: s.stackOK},
	}
	s.textSensors = []textSensor{
		{id: "adapter_address", name: "Adapter address", icon: "mdi:bluetooth-settings",
			value: func() string { return ble.FormatMAC(s.adapter.MAC()) }},
	}
	if as, ok := s.adapter.(ble.AdvertSourcer); ok {
		s.textSensors = append(s.textSensors, textSensor{id: "advertisement_source", name: "Advertisement source",
			icon: "mdi:access-point", value: as.AdvertSource})
	}
	if ss, ok := s.adapter.(ble.ScanSettinger); ok {
		s.textSensors = append(s.textSensors, textSensor{id: "scan_setting", name: "Scan setting",
			icon: "mdi:radar", value: ss.ScanSetting})
	}
	if s.cfg.LastProblem != nil {
		s.textSensors = append(s.textSensors, textSensor{id: "last_problem", name: "Last problem",
			icon: "mdi:alert-circle-outline", value: func() string {
				if p := s.cfg.LastProblem(); p != "" {
					return p
				}
				return "none"
			}})
	}
	if s.cfg.Restart != nil {
		s.buttons = append(s.buttons, button{id: "restart_proxy", name: "Restart proxy", icon: "mdi:restart",
			press: s.restartProxy})
	}
	if sr, ok := s.adapter.(ble.StackRestarter); ok && sr.StackRestartable() {
		s.buttons = append(s.buttons, button{id: "restart_bluetooth", name: "Restart Bluetooth stack",
			icon: "mdi:bluetooth-off", press: func(c *client) {
				c.log.Info("Bluetooth stack restart requested through the API")
				if err := sr.RestartStack(); err != nil {
					c.log.Warn("Bluetooth stack not restarted", "err", err)
				}
			}})
	}
}

// stackOK reports whether the Bluetooth stack works: idle counts, as scanning may be paused or switched off.
// Adapters that report no scanner state count as working.
func (s *Server) stackOK() bool {
	ms, ok := s.adapter.(ble.ModeScanner)
	if !ok {
		return true
	}
	st := ms.ScanStatus().State
	return st != ble.ScanFailed && st != ble.ScanStopped
}

// stateText cuts a state to what Home Assistant accepts, leaving valid UTF-8.
func stateText(t string) string {
	if len(t) <= maxStateText {
		return t
	}
	return strings.ToValidUTF8(t[:maxStateText], "")
}

// restartProxy drops the connections, so the devices are free for whoever connects next, and restarts.
func (s *Server) restartProxy(c *client) {
	c.log.Info("restart requested through the API")
	done := make(chan struct{})
	go func() {
		s.disconnectAll()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(restartDropTimeout):
		c.log.Warn("restarting with connections still dropping")
	}
	s.cfg.Restart()
}

// setScanning handles the scanning switch. It holds for as long as the proxy runs: a proxy that comes back from
// a restart silently not scanning would be hard to diagnose.
func (s *Server) setScanning(c *client, on bool) {
	s.mu.Lock()
	s.scanOff = !on
	s.updateScan()
	s.mu.Unlock()
	c.log.Info("scanning switched", "on", on)
	s.pushStates()
	s.sendScannerState()
}

// switchCommand handles SwitchCommandRequest; an unknown key is ignored.
func (c *client) switchCommand(payload []byte) error {
	fs, err := fields(payload)
	if err != nil {
		return err
	}
	var key uint32
	var on bool
	for _, f := range fs {
		switch f.num {
		case 1:
			key = uint32(f.v)
		case 2:
			on = f.v != 0
		}
	}
	if key == entityKey(scanSwitchID) {
		c.s.setScanning(c, on)
	} else {
		c.log.Debug("command for an unknown switch", "key", key)
	}
	return nil
}

// buttonCommand handles ButtonCommandRequest; an unknown key is ignored. False means too many requests are
// running, which closes the client.
func (c *client) buttonCommand(payload []byte) (bool, error) {
	fs, err := fields(payload)
	if err != nil {
		return true, err
	}
	var key uint32
	for _, f := range fs {
		if f.num == 1 {
			key = uint32(f.v)
		}
	}
	for i := range c.s.buttons {
		if b := &c.s.buttons[i]; entityKey(b.id) == key {
			return c.spawn(func() { b.press(c) }), nil
		}
	}
	c.log.Debug("command for an unknown button", "key", key)
	return true, nil
}

func encBinarySensorInfo(e *binarySensor) []byte {
	var m enc
	m.str(1, e.id)
	m.fixed32(2, entityKey(e.id))
	m.str(3, e.name)
	m.str(5, e.class)
	m.str(8, e.icon)
	m.uint(9, entityDiagnostic)
	return m.b
}

func encBinarySensorState(key uint32, on bool) []byte {
	var m enc
	m.fixed32(1, key)
	m.bool(2, on)
	return m.b
}

func encTextSensorInfo(e *textSensor) []byte {
	var m enc
	m.str(1, e.id)
	m.fixed32(2, entityKey(e.id))
	m.str(3, e.name)
	m.str(5, e.icon)
	m.uint(7, entityDiagnostic)
	return m.b
}

func encTextSensorState(key uint32, text string) []byte {
	var m enc
	m.fixed32(1, key)
	m.str(2, stateText(text))
	return m.b
}

func encScanSwitchInfo() []byte {
	var m enc
	m.str(1, scanSwitchID)
	m.fixed32(2, entityKey(scanSwitchID))
	m.str(3, "Scanning")
	m.str(5, "mdi:bluetooth-audio")
	m.uint(8, entityConfig)
	return m.b
}

func encSwitchState(key uint32, on bool) []byte {
	var m enc
	m.fixed32(1, key)
	m.bool(2, on)
	return m.b
}

func encButtonInfo(e *button) []byte {
	var m enc
	m.str(1, e.id)
	m.fixed32(2, entityKey(e.id))
	m.str(3, e.name)
	m.str(5, e.icon)
	m.bool(6, true) // disabled_by_default
	m.uint(7, entityConfig)
	m.str(8, "restart")
	return m.b
}
