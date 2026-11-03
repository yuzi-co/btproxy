package api

import (
	"bytes"
	"math"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"

	"google.golang.org/protobuf/encoding/protowire"
)

// The proxy reports its own statistics as ESPHome sensor entities, so Home Assistant shows them as diagnostic
// entities of the device. Counters are atomics updated where things happen; the values are read when states
// are sent: on SubscribeStatesRequest, every statsInterval, and when a connection is made, ends or fails.

// statsInterval is how often states go to subscribed clients. A variable, so the tests can shorten it.
var statsInterval = time.Minute

// connWindow is how many of the latest connection attempts the success rate covers.
const connWindow = 20

// Sensor state classes and the diagnostic entity category, with ESPHome's values.
const (
	stateMeasurement     = 1
	stateTotalIncreasing = 2
	entityDiagnostic     = 2
)

// stats holds the counters behind the sensors. The fields after the atomics are guarded by Server.mu.
type stats struct {
	start      time.Time
	connTried  atomic.Uint64 // connects handed to the adapter
	connMade   atomic.Uint64 // of which connected
	adverts    atomic.Uint64 // advertisements received from the adapter
	advDropped atomic.Uint64 // advertisements not queued for a client that was falling behind

	recent  [connWindow]bool // outcomes of the latest connects, a ring
	recentN int              // connects recorded, at most connWindow count
	recentI int              // where the next outcome goes

	rateN   uint64    // adverts at the last rate sample
	rateT   time.Time // and when it was taken
	advRate float32   // advertisements per second between the last two samples
	hasRate bool
}

// sensor is one entity. value returns the current state, or false while there is none.
type sensor struct {
	id, name, icon, unit, class string // object_id, name, icon, unit_of_measurement, device_class
	accuracy                    int32
	stateClass                  uint32
	value                       func() (float32, bool)
}

// key is the entity key.
func (e *sensor) key() uint32 { return entityKey(e.id) }

// initStats starts the counters and builds the sensor list. The bluetoothd restart count is listed only
// for adapters that report it, the memory only where /proc/self/statm is readable (Linux).
func (s *Server) initStats() {
	s.stats.start = time.Now()
	s.stats.rateT = s.stats.start
	count := func(v *atomic.Uint64) func() (float32, bool) {
		return func() (float32, bool) { return float32(v.Load()), true }
	}
	st := &s.stats
	s.sensors = []sensor{
		{id: "uptime", name: "Uptime", icon: "mdi:timer-outline", unit: "s", class: "duration",
			stateClass: stateTotalIncreasing, value: func() (float32, bool) {
				return float32(math.Floor(time.Since(st.start).Seconds())), true
			}},
		{id: "connection_attempts", name: "Connection attempts", icon: "mdi:bluetooth-connect",
			stateClass: stateTotalIncreasing, value: count(&st.connTried)},
		{id: "connections_made", name: "Connections made", icon: "mdi:bluetooth-connect",
			stateClass: stateTotalIncreasing, value: count(&st.connMade)},
		{id: "connection_success_rate", name: "Connection success rate", icon: "mdi:percent", unit: "%",
			stateClass: stateMeasurement, value: s.connSuccessRate},
		{id: "advertisement_rate", name: "Advertisement rate", icon: "mdi:access-point", unit: "adv/s",
			accuracy: 1, stateClass: stateMeasurement, value: s.advertRate},
		{id: "advertisements_dropped", name: "Advertisements dropped", icon: "mdi:alert-circle-outline",
			stateClass: stateTotalIncreasing, value: count(&st.advDropped)},
		{id: "active_connections", name: "Active connections", icon: "mdi:bluetooth-connect",
			stateClass: stateMeasurement, value: func() (float32, bool) {
				n, _ := s.slots()
				return float32(n), true
			}},
		{id: "free_connection_slots", name: "Free connection slots", icon: "mdi:bluetooth-settings",
			stateClass: stateMeasurement, value: func() (float32, bool) {
				_, free := s.slots()
				return float32(free), true
			}},
	}
	if rc, ok := s.adapter.(ble.RestartCounter); ok {
		s.sensors = append(s.sensors, sensor{id: "bluetoothd_restarts", name: "bluetoothd restarts",
			icon: "mdi:restart", stateClass: stateTotalIncreasing,
			value: func() (float32, bool) { return float32(rc.Restarts()), true }})
	}
	if _, ok := rssMiB(); ok {
		s.sensors = append(s.sensors, sensor{id: "memory", name: "Memory", icon: "mdi:memory", unit: "MiB",
			class: "data_size", accuracy: 1, stateClass: stateMeasurement, value: rssMiB})
	}
}

// recordConnect adds a connect's outcome to the counters.
func (s *Server) recordConnect(ok bool) {
	if ok {
		s.stats.connMade.Add(1)
	}
	s.mu.Lock()
	st := &s.stats
	st.recent[st.recentI] = ok
	st.recentI = (st.recentI + 1) % connWindow
	st.recentN = min(st.recentN+1, connWindow)
	s.mu.Unlock()
}

// connSuccessRate is the share of the latest connWindow connects that connected, in percent; none before
// the first connect.
func (s *Server) connSuccessRate() (float32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.stats
	if st.recentN == 0 {
		return 0, false
	}
	ok := 0
	for _, r := range st.recent[:st.recentN] {
		if r {
			ok++
		}
	}
	return float32(100 * ok / st.recentN), true
}

// sampleAdvertRate computes the advertisement rate since the last sample. The periodic push calls it, so
// the rate covers statsInterval.
func (s *Server) sampleAdvertRate(now time.Time) {
	n := s.stats.adverts.Load()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.stats
	if dt := now.Sub(st.rateT).Seconds(); dt > 0 {
		st.advRate, st.hasRate = float32(float64(n-st.rateN)/dt), true
	}
	st.rateN, st.rateT = n, now
}

// advertRate is the rate of the last sample, or until the first one the rate since start once a second has
// passed.
func (s *Server) advertRate() (float32, bool) {
	n := s.stats.adverts.Load()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.stats
	if st.hasRate {
		return st.advRate, true
	}
	if dt := time.Since(st.rateT).Seconds(); dt >= 1 {
		return float32(float64(n-st.rateN) / dt), true
	}
	return 0, false
}

// slots returns the established connections and the free connection slots.
func (s *Server) slots() (connected, free int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, bc := range s.conns {
		if bc.conn != nil {
			connected++
		}
	}
	return connected, s.cfg.MaxConnections - len(s.conns)
}

// rssMiB is the process's resident memory from /proc/self/statm, in MiB.
func rssMiB() (float32, bool) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := bytes.Fields(b)
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(string(f[1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return float32(pages*uint64(os.Getpagesize())) / (1 << 20), true
}

// sendEntities answers ListEntitiesRequest.
func (c *client) sendEntities() {
	s := c.s
	for i := range s.sensors {
		c.send(msgListEntitiesSensorResponse, encSensorInfo(&s.sensors[i]))
	}
	for i := range s.binarySensors {
		c.send(msgListEntitiesBinarySensorResponse, encBinarySensorInfo(&s.binarySensors[i]))
	}
	for i := range s.textSensors {
		c.send(msgListEntitiesTextSensorResponse, encTextSensorInfo(&s.textSensors[i]))
	}
	c.send(msgListEntitiesSwitchResponse, encScanSwitchInfo())
	for i := range s.buttons {
		c.send(msgListEntitiesButtonResponse, encButtonInfo(&s.buttons[i]))
	}
	c.send(msgListEntitiesDoneResponse, nil)
}

// stateMsg is one entity's state, ready to send.
type stateMsg struct {
	typ     uint32
	payload []byte
}

// states encodes the state of every entity. Values are read without Server.mu held, except the switch's.
func (s *Server) states() []stateMsg {
	out := make([]stateMsg, 0, len(s.sensors)+len(s.binarySensors)+len(s.textSensors)+1)
	for i := range s.sensors {
		v, ok := s.sensors[i].value()
		out = append(out, stateMsg{msgSensorStateResponse, encSensorState(s.sensors[i].key(), v, ok)})
	}
	for i := range s.binarySensors {
		e := &s.binarySensors[i]
		out = append(out, stateMsg{msgBinarySensorStateResponse, encBinarySensorState(entityKey(e.id), e.value())})
	}
	for i := range s.textSensors {
		e := &s.textSensors[i]
		out = append(out, stateMsg{msgTextSensorStateResponse, encTextSensorState(entityKey(e.id), e.value())})
	}
	s.mu.Lock()
	on := !s.scanOff
	s.mu.Unlock()
	return append(out, stateMsg{msgSwitchStateResponse, encSwitchState(entityKey(scanSwitchID), on)})
}

// pushStates sends every entity's state to the clients subscribed to states. They may be dropped for a
// client that is behind: the next push brings them again.
func (s *Server) pushStates() {
	msgs := s.states()
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		if c.statesSubscribed {
			for _, m := range msgs {
				c.sendDroppable(m.typ, m.payload)
			}
		}
	}
}

func (s *Server) statsLoop() {
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	for now := range t.C {
		s.sampleAdvertRate(now)
		s.pushStates()
	}
}

// encSensorInfo encodes ListEntitiesSensorResponse.
func encSensorInfo(e *sensor) []byte {
	var m enc
	m.str(1, e.id)
	m.fixed32(2, e.key())
	m.str(3, e.name)
	m.str(5, e.icon)
	m.str(6, e.unit)
	m.int32(7, e.accuracy)
	m.str(9, e.class)
	m.uint(10, uint64(e.stateClass))
	m.uint(13, entityDiagnostic)
	return m.b
}

// encSensorState encodes SensorStateResponse; ok false sends missing_state.
func encSensorState(key uint32, v float32, ok bool) []byte {
	var m enc
	m.fixed32(1, key)
	if ok && v != 0 {
		m.fixed32(2, math.Float32bits(v))
	}
	m.bool(3, !ok)
	return m.b
}

// fixed32 appends a fixed32 or float field, also when zero (ESPHome forces entity keys).
func (e *enc) fixed32(num protowire.Number, v uint32) {
	e.b = protowire.AppendTag(e.b, num, protowire.Fixed32Type)
	e.b = protowire.AppendFixed32(e.b, v)
}
