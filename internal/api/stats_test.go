package api

import (
	"hash/fnv"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"btproxy/internal/ble"
	"btproxy/internal/logx"
)

// restartAdapter is a testAdapter that reports bluetoothd restarts.
type restartAdapter struct{ testAdapter }

func (restartAdapter) Restarts() uint64 { return 7 }

// TestSensorKeys checks that keys are ESPHome's FNV-1 hash of the object id, so they never change, and
// unique.
func TestSensorKeys(t *testing.T) {
	s := NewServer(Config{Name: "test"}, restartAdapter{}, logx.Discard())
	seen := map[uint32]string{}
	for _, e := range s.sensors {
		h := fnv.New32()
		h.Write([]byte(e.id))
		if e.key() != h.Sum32() {
			t.Errorf("%s: key %#x, want %#x", e.id, e.key(), h.Sum32())
		}
		if other, ok := seen[e.key()]; ok {
			t.Errorf("%s and %s share key %#x", e.id, other, e.key())
		}
		seen[e.key()] = e.id
	}
}

// listEntities sends ListEntitiesRequest and returns the sensors listed, by object id.
func (tc *testClient) listEntities() map[string][]field {
	tc.t.Helper()
	tc.send(msgListEntitiesRequest, nil)
	out := map[string][]field{}
	tc.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		f, err := ReadFrame(tc.r)
		if err != nil {
			tc.t.Fatal(err)
		}
		switch f.Type {
		case msgListEntitiesDoneResponse:
			return out
		case msgListEntitiesSensorResponse:
			fs, err := fields(f.Payload)
			if err != nil {
				tc.t.Fatal(err)
			}
			for _, x := range fs {
				if x.num == 1 {
					out[string(x.data)] = fs
				}
			}
		}
	}
}

func TestListSensors(t *testing.T) {
	tc := startServer(t, testAdapter{})
	got := tc.listEntities()
	for _, id := range []string{"uptime", "connection_attempts", "connections_made", "connection_success_rate",
		"advertisement_rate", "advertisements_dropped", "active_connections", "free_connection_slots"} {
		fs, ok := got[id]
		if !ok {
			t.Errorf("sensor %s not listed", id)
			continue
		}
		if c := fieldValue(fs, 13); c != entityDiagnostic {
			t.Errorf("%s: entity category %d", id, c)
		}
		if e := (&sensor{id: id}); uint32(fieldValue(fs, 2)) != e.key() {
			t.Errorf("%s: key %#x", id, fieldValue(fs, 2))
		}
	}
	if _, ok := got["bluetoothd_restarts"]; ok {
		t.Error("restart count listed for an adapter that does not report it")
	}

	tc = startServer(t, restartAdapter{})
	if _, ok := tc.listEntities()["bluetoothd_restarts"]; !ok {
		t.Error("restart count not listed")
	}
}

// stateOf reads SensorStateResponses until the one for id arrives and returns its value, or NaN for a
// missing state.
func (tc *testClient) stateOf(id string) float32 {
	tc.t.Helper()
	key := (&sensor{id: id}).key()
	for {
		fs := tc.expect(msgSensorStateResponse)
		if uint32(fieldValue(fs, 1)) != key {
			continue
		}
		if fieldValue(fs, 3) != 0 {
			return float32(math.NaN())
		}
		return math.Float32frombits(uint32(fieldValue(fs, 2)))
	}
}

// TestSensorStates subscribes to states, then connects once successfully and once not: states are sent on
// subscribe and after each connect.
func TestSensorStates(t *testing.T) {
	var fail atomic.Bool
	tc := startServer(t, restartAdapter{testAdapter{connect: func(ble.Events) (ble.Conn, error) {
		if fail.Load() {
			return nil, ble.ErrNotConnected
		}
		return &testConn{}, nil
	}}})
	tc.send(msgSubscribeStatesRequest, nil)
	if v := tc.stateOf("connection_success_rate"); !math.IsNaN(float64(v)) {
		t.Errorf("success rate before any connect: %v", v)
	}
	if v := tc.stateOf("bluetoothd_restarts"); v != 7 {
		t.Errorf("restarts %v", v)
	}

	tc.connect()
	if v := tc.stateOf("connection_attempts"); v != 1 {
		t.Errorf("attempts %v, want 1", v)
	}
	if v := tc.stateOf("connections_made"); v != 1 {
		t.Errorf("made %v, want 1", v)
	}
	if v := tc.stateOf("connection_success_rate"); v != 100 {
		t.Errorf("success rate %v, want 100", v)
	}
	if v := tc.stateOf("active_connections"); v != 1 {
		t.Errorf("active %v, want 1", v)
	}

	var e enc
	e.uint(1, testAddr)
	e.uint(2, reqDisconnect)
	tc.send(msgBTDeviceRequest, e.b)
	tc.expect(msgBTDeviceConnectionResponse)
	fail.Store(true)
	tc.connect()
	if v := tc.stateOf("connection_attempts"); v != 2 {
		t.Errorf("attempts %v, want 2", v)
	}
	if v := tc.stateOf("connection_success_rate"); v != 50 {
		t.Errorf("success rate %v, want 50", v)
	}
	if v := tc.stateOf("active_connections"); v != 0 {
		t.Errorf("active %v, want 0", v)
	}
}

// TestConnSuccessWindow checks that the success rate covers only the latest connWindow connects.
func TestConnSuccessWindow(t *testing.T) {
	s := NewServer(Config{Name: "test"}, testAdapter{}, logx.Discard())
	for range connWindow {
		s.recordConnect(false)
	}
	for range connWindow / 2 {
		s.recordConnect(true)
	}
	if v, ok := s.connSuccessRate(); !ok || v != 50 {
		t.Fatalf("rate %v %v, want 50", v, ok)
	}
}

// TestAdvertStats counts advertisements, their rate, and the ones dropped for a client that is behind.
func TestAdvertStats(t *testing.T) {
	s := NewServer(Config{Name: "test"}, testAdapter{}, logx.Discard())
	c := &client{s: s, c: discardConn{}, log: s.log, out: make(chan outMsg, outQueue), advSubscribed: true}
	for range dropLimit { // a writer that does not run: the queue stays full
		c.out <- outMsg{}
	}
	s.clients[c] = true
	for range advBatchSize {
		s.onAdvert(ble.Advert{Addr: 1, Data: []byte{2, 1, 6}})
	}
	if n := s.stats.advDropped.Load(); n != advBatchSize {
		t.Errorf("dropped %d, want %d", n, advBatchSize)
	}
	s.sampleAdvertRate(s.stats.rateT.Add(2 * time.Second))
	if v, ok := s.advertRate(); !ok || v != advBatchSize/2 {
		t.Errorf("rate %v %v, want %v", v, ok, advBatchSize/2)
	}
}

func TestMemorySensor(t *testing.T) {
	if _, ok := rssMiB(); !ok {
		t.Skip("no /proc/self/statm")
	}
	tc := startServer(t, testAdapter{})
	if _, ok := tc.listEntities()["memory"]; !ok {
		t.Fatal("memory not listed")
	}
	tc.send(msgSubscribeStatesRequest, nil)
	if v := tc.stateOf("memory"); !(v > 0) {
		t.Errorf("memory %v MiB", v)
	}
}

func TestEncSensorState(t *testing.T) {
	fs, err := fields(encSensorState(0x12345678, 1.5, true))
	if err != nil {
		t.Fatal(err)
	}
	if fieldValue(fs, 1) != 0x12345678 || math.Float32frombits(uint32(fieldValue(fs, 2))) != 1.5 ||
		fieldValue(fs, 3) != 0 {
		t.Errorf("fields %+v", fs)
	}
	fs, _ = fields(encSensorState(1, 0, false))
	if fieldValue(fs, 3) != 1 {
		t.Errorf("missing state not set: %+v", fs)
	}
}
