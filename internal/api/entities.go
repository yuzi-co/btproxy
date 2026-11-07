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

// entity is one entity of the proxy's device, of any kind. Each kind encodes its own ListEntities message
// and state message; the loops over the entities do not know the kinds.
type entity interface {
	header() *entityHeader
	// info returns the ListEntities message that describes the entity.
	info() (typ uint32, payload []byte)
	// state returns the entity's state message, or false for a kind without a state.
	state() (typ uint32, payload []byte, ok bool)
}

// entityHeader is what every kind of entity carries: object id, name and icon.
type entityHeader struct {
	id, name, icon string
}

func (h *entityHeader) header() *entityHeader { return h }

// key is the entity key, an FNV-1 hash of the object id as ESPHome computes it, so it stays the same across
// restarts and versions.
func (h *entityHeader) key() uint32 { return entityKey(h.id) }

// binarySensor is a state that is on or off.
type binarySensor struct {
	entityHeader
	class string
	value func() bool
}

// textSensor is a state in words.
type textSensor struct {
	entityHeader
	value func() string
}

// scanSwitch is the scanning switch, whose state is the server's scanOff under its lock.
type scanSwitch struct {
	entityHeader
	s *Server
}

// button runs press in its own goroutine, since it may wait for the radio.
type button struct {
	entityHeader
	press func(c *client)
}

// entityKey is the key of an entity, an FNV-1 hash of the object id as ESPHome computes it.
func entityKey(id string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(id); i++ {
		h *= 16777619
		h ^= uint32(id[i])
	}
	return h
}

// initEntities lists the entities that are not sensors, after them. The restart buttons are disabled by
// default in Home Assistant, because without an encryption key any host that can reach the API could press
// them.
func (s *Server) initEntities() {
	s.entities = append(s.entities,
		&binarySensor{entityHeader: entityHeader{"bluetooth_stack", "Bluetooth stack", "mdi:bluetooth"},
			class: "running", value: s.stackOK},
		&textSensor{entityHeader: entityHeader{"adapter_address", "Adapter address", "mdi:bluetooth-settings"},
			value: func() string { return ble.FormatMAC(s.adapter.MAC()) }},
	)
	if as, ok := s.adapter.(ble.AdvertSourcer); ok {
		s.entities = append(s.entities, &textSensor{
			entityHeader: entityHeader{"advertisement_source", "Advertisement source", "mdi:access-point"},
			value:        as.AdvertSource})
	}
	if ss, ok := s.adapter.(ble.ScanSettinger); ok {
		s.entities = append(s.entities, &textSensor{
			entityHeader: entityHeader{"scan_setting", "Scan setting", "mdi:radar"}, value: ss.ScanSetting})
	}
	if s.cfg.LastProblem != nil {
		s.entities = append(s.entities, &textSensor{
			entityHeader: entityHeader{"last_problem", "Last problem", "mdi:alert-circle-outline"},
			value: func() string {
				if p := s.cfg.LastProblem(); p != "" {
					return p
				}
				return "none"
			}})
	}
	s.entities = append(s.entities, &scanSwitch{entityHeader{scanSwitchID, "Scanning", "mdi:bluetooth-audio"}, s})
	if s.cfg.Restart != nil {
		s.entities = append(s.entities, &button{entityHeader{"restart_proxy", "Restart proxy", "mdi:restart"},
			s.restartProxy})
	}
	if sr, ok := s.adapter.(ble.StackRestarter); ok && sr.StackRestartable() {
		s.entities = append(s.entities, &button{
			entityHeader{"restart_bluetooth", "Restart Bluetooth stack", "mdi:bluetooth-off"},
			func(c *client) {
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
func (c *client) switchCommand(f Frame) bool {
	fs, err := fields(f.Payload)
	if err != nil {
		return c.bad(f.Type, err)
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
	return true
}

// buttonCommand handles ButtonCommandRequest; an unknown key is ignored. The press runs in its own goroutine,
// so false means too many requests are running, which closes the client.
func (c *client) buttonCommand(f Frame) bool {
	fs, err := fields(f.Payload)
	if err != nil {
		return c.bad(f.Type, err)
	}
	var key uint32
	for _, f := range fs {
		if f.num == 1 {
			key = uint32(f.v)
		}
	}
	for _, e := range c.s.entities {
		if b, ok := e.(*button); ok && b.key() == key {
			return c.spawn(func() { b.press(c) })
		}
	}
	c.log.Debug("command for an unknown button", "key", key)
	return true
}

// info encodes ListEntitiesBinarySensorResponse.
func (e *binarySensor) info() (uint32, []byte) {
	var m enc
	m.str(1, e.id)
	m.fixed32(2, e.key())
	m.str(3, e.name)
	m.str(5, e.class)
	m.str(8, e.icon)
	m.uint(9, entityDiagnostic)
	return msgListEntitiesBinarySensorResponse, m.b
}

// state encodes BinarySensorStateResponse.
func (e *binarySensor) state() (uint32, []byte, bool) {
	return msgBinarySensorStateResponse, encKeyBool(e.key(), e.value()), true
}

// info encodes ListEntitiesTextSensorResponse.
func (e *textSensor) info() (uint32, []byte) {
	var m enc
	m.str(1, e.id)
	m.fixed32(2, e.key())
	m.str(3, e.name)
	m.str(5, e.icon)
	m.uint(7, entityDiagnostic)
	return msgListEntitiesTextSensorResponse, m.b
}

// state encodes TextSensorStateResponse.
func (e *textSensor) state() (uint32, []byte, bool) {
	var m enc
	m.fixed32(1, e.key())
	m.str(2, stateText(e.value()))
	return msgTextSensorStateResponse, m.b, true
}

// info encodes ListEntitiesSwitchResponse.
func (e *scanSwitch) info() (uint32, []byte) {
	var m enc
	m.str(1, e.id)
	m.fixed32(2, e.key())
	m.str(3, e.name)
	m.str(5, e.icon)
	m.uint(8, entityConfig)
	return msgListEntitiesSwitchResponse, m.b
}

// state encodes SwitchStateResponse.
func (e *scanSwitch) state() (uint32, []byte, bool) {
	e.s.mu.Lock()
	on := !e.s.scanOff
	e.s.mu.Unlock()
	return msgSwitchStateResponse, encKeyBool(e.key(), on), true
}

// info encodes ListEntitiesButtonResponse.
func (e *button) info() (uint32, []byte) {
	var m enc
	m.str(1, e.id)
	m.fixed32(2, e.key())
	m.str(3, e.name)
	m.str(5, e.icon)
	m.bool(6, true) // disabled_by_default
	m.uint(7, entityConfig)
	m.str(8, "restart")
	return msgListEntitiesButtonResponse, m.b
}

// state: a button has none.
func (e *button) state() (uint32, []byte, bool) { return 0, nil, false }

// encKeyBool encodes the state of a binary sensor or a switch: key, then state.
func encKeyBool(key uint32, on bool) []byte {
	var m enc
	m.fixed32(1, key)
	m.bool(2, on)
	return m.b
}
