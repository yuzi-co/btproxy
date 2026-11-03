package api

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/fake"
	"github.com/yuzi-co/btproxy/internal/logx"
)

func startServerConfig(t *testing.T, cfg Config, a ble.Adapter) *testClient {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	cfg.Name = "test"
	go NewServer(cfg, a, logx.Discard()).Serve(l)
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &testClient{t: t, c: c, r: bufio.NewReader(c)}
}

// listed is one entity from ListEntities.
type listed struct {
	typ uint32
	fs  []field
}

// listAll sends ListEntitiesRequest and returns every entity, by object id.
func (tc *testClient) listAll() map[string]listed {
	tc.t.Helper()
	tc.send(msgListEntitiesRequest, nil)
	out := map[string]listed{}
	tc.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		f, err := ReadFrame(tc.r)
		if err != nil {
			tc.t.Fatal(err)
		}
		switch f.Type {
		case msgListEntitiesDoneResponse:
			return out
		case msgListEntitiesSensorResponse, msgListEntitiesBinarySensorResponse, msgListEntitiesTextSensorResponse,
			msgListEntitiesSwitchResponse, msgListEntitiesButtonResponse:
			fs, err := fields(f.Payload)
			if err != nil {
				tc.t.Fatal(err)
			}
			for _, x := range fs {
				if x.num == 1 {
					out[string(x.data)] = listed{f.Type, fs}
				}
			}
		}
	}
}

// expectBool reads state messages of type typ until one for id has the wanted value.
func (tc *testClient) expectBool(typ uint32, id string, want bool) {
	tc.t.Helper()
	key := entityKey(id)
	for {
		fs := tc.expect(typ)
		if uint32(fieldValue(fs, 1)) == key && (fieldValue(fs, 2) != 0) == want {
			return
		}
	}
}

// textOf reads TextSensorStateResponses until the one for id arrives.
func (tc *testClient) textOf(id string) string {
	tc.t.Helper()
	key := entityKey(id)
	for {
		fs := tc.expect(msgTextSensorStateResponse)
		if uint32(fieldValue(fs, 1)) != key {
			continue
		}
		for _, f := range fs {
			if f.num == 2 {
				return string(f.data)
			}
		}
		return ""
	}
}

func (tc *testClient) setSwitch(id string, on bool) {
	var e enc
	e.fixed32(1, entityKey(id))
	e.bool(2, on)
	tc.send(msgSwitchCommandRequest, e.b)
}

func (tc *testClient) pressButton(id string) {
	var e enc
	e.fixed32(1, entityKey(id))
	tc.send(msgButtonCommandRequest, e.b)
}

// TestEntityKeysAreUnique covers every kind of entity, since Home Assistant keys entities across kinds.
func TestEntityKeysAreUnique(t *testing.T) {
	tc := startServerConfig(t, Config{Restart: func() {}, LastProblem: func() string { return "" }},
		&stackAdapter{available: true})
	seen := map[uint64]string{}
	for id, e := range tc.listAll() {
		key := fieldValue(e.fs, 2)
		if uint32(key) != entityKey(id) {
			t.Errorf("%s: key %#x, want %#x", id, key, entityKey(id))
		}
		if other, dup := seen[key]; dup {
			t.Errorf("%s and %s share key %#x", id, other, key)
		}
		seen[key] = id
	}
}

func TestStackSensorFollowsTheScanner(t *testing.T) {
	a := &fake.Adapter{}
	tc := startServer(t, a)
	if e, ok := tc.listAll()["bluetooth_stack"]; !ok || e.typ != msgListEntitiesBinarySensorResponse {
		t.Fatalf("bluetooth_stack listed as %+v", e)
	} else if fieldValue(e.fs, 9) != entityDiagnostic {
		t.Errorf("entity category %d", fieldValue(e.fs, 9))
	}
	tc.subscribeAdverts()
	tc.send(msgSubscribeStatesRequest, nil)
	tc.expectBool(msgBinarySensorStateResponse, "bluetooth_stack", true)

	// Both changes must arrive at once, not at the next periodic push.
	a.Lose()
	tc.expectBool(msgBinarySensorStateResponse, "bluetooth_stack", false)
	a.Restore()
	tc.expectBool(msgBinarySensorStateResponse, "bluetooth_stack", true)
}

// statusAdapter reports a fixed scanner state.
type statusAdapter struct {
	testAdapter
	state ble.ScanState
}

func (statusAdapter) SetScanMode(ble.ScanMode)          {}
func (a statusAdapter) ScanStatus() ble.ScanStatus      { return ble.ScanStatus{State: a.state} }
func (statusAdapter) OnScanStatus(func(ble.ScanStatus)) {}

func TestStackSensorStates(t *testing.T) {
	for state, want := range map[ble.ScanState]bool{
		ble.ScanIdle: true, ble.ScanStarting: true, ble.ScanRunning: true, ble.ScanStopping: true,
		ble.ScanFailed: false, ble.ScanStopped: false,
	} {
		s := NewServer(Config{Name: "test"}, statusAdapter{state: state}, logx.Discard())
		if got := s.stackOK(); got != want {
			t.Errorf("state %d: %v, want %v", state, got, want)
		}
	}
	if s := NewServer(Config{Name: "test"}, testAdapter{}, logx.Discard()); !s.stackOK() {
		t.Error("an adapter that reports no state must count as working")
	}
}

func TestAdapterAddressSensor(t *testing.T) {
	tc := startServer(t, testAdapter{})
	tc.send(msgSubscribeStatesRequest, nil)
	if got := tc.textOf("adapter_address"); got != ble.FormatMAC(1) {
		t.Errorf("address %q", got)
	}
}

// sourceAdapter says where its advertisements come from.
type sourceAdapter struct{ testAdapter }

func (sourceAdapter) AdvertSource() string { return "HCI monitor" }

func TestAdvertSourceSensor(t *testing.T) {
	if _, ok := startServer(t, testAdapter{}).listAll()["advertisement_source"]; ok {
		t.Error("listed for an adapter that does not report it")
	}
	tc := startServer(t, sourceAdapter{})
	tc.send(msgSubscribeStatesRequest, nil)
	if got := tc.textOf("advertisement_source"); got != "HCI monitor" {
		t.Errorf("source %q", got)
	}
}

func TestLastProblemSensor(t *testing.T) {
	if _, ok := startServer(t, testAdapter{}).listAll()["last_problem"]; ok {
		t.Error("listed without a source")
	}
	var problem atomic.Value
	problem.Store("")
	cfg := Config{LastProblem: func() string { return problem.Load().(string) }}
	tc := startServerConfig(t, cfg, testAdapter{})
	tc.send(msgSubscribeStatesRequest, nil)
	if got := tc.textOf("last_problem"); got != "none" {
		t.Errorf("before any problem: %q", got)
	}
	problem.Store(`level=WARN msg="scanning stopped"`)
	tc.send(msgSubscribeStatesRequest, nil)
	if got := tc.textOf("last_problem"); got != `level=WARN msg="scanning stopped"` {
		t.Errorf("problem %q", got)
	}
}

// Home Assistant refuses a state over 255 characters, and protobuf strings must be valid UTF-8.
func TestStateTextIsCutToFit(t *testing.T) {
	got := stateText(strings.Repeat("é", 200))
	if len(got) > maxStateText || !utf8.ValidString(got) {
		t.Errorf("%d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if got := stateText("short"); got != "short" {
		t.Errorf("%q", got)
	}
}

// scanCounter counts running scans.
type scanCounter struct {
	testAdapter
	mu      sync.Mutex
	running int
	starts  int
}

func (a *scanCounter) Scan(func(ble.Advert)) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.running++
	a.starts++
	return func() {
		a.mu.Lock()
		a.running--
		a.mu.Unlock()
	}, nil
}

func (a *scanCounter) counts() (running, starts int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running, a.starts
}

func TestScanSwitch(t *testing.T) {
	a := &scanCounter{}
	tc := startServer(t, a)
	if e := tc.listAll()["scanning"]; e.typ != msgListEntitiesSwitchResponse {
		t.Fatalf("scanning listed as %+v", e)
	} else if fieldValue(e.fs, 8) != entityConfig {
		t.Errorf("entity category %d", fieldValue(e.fs, 8))
	}
	tc.send(msgSubscribeStatesRequest, nil)
	tc.expectBool(msgSwitchStateResponse, "scanning", true) // on after every start
	tc.subscribeAdverts()
	tc.expectState(ble.ScanRunning, ble.ScanActive, ble.ScanActive)

	tc.setSwitch("scanning", false)
	tc.expectBool(msgSwitchStateResponse, "scanning", false)
	tc.expectState(ble.ScanIdle, ble.ScanActive, ble.ScanActive)
	if running, _ := a.counts(); running != 0 {
		t.Errorf("%d scans running while switched off", running)
	}

	tc.setSwitch("scanning", true)
	tc.expectBool(msgSwitchStateResponse, "scanning", true)
	tc.expectState(ble.ScanRunning, ble.ScanActive, ble.ScanActive)
	if running, starts := a.counts(); running != 1 || starts != 2 {
		t.Errorf("running %d, starts %d, want 1 and 2", running, starts)
	}
}

// A client that subscribes while the switch is off must not restart the scan behind its back.
func TestSubscribingDoesNotOverrideTheSwitch(t *testing.T) {
	a := &scanCounter{}
	tc := startServer(t, a)
	tc.send(msgSubscribeStatesRequest, nil)
	tc.setSwitch("scanning", false)
	tc.expectBool(msgSwitchStateResponse, "scanning", false)
	tc.subscribeAdverts()
	tc.expectState(ble.ScanIdle, ble.ScanActive, ble.ScanActive)
	if _, starts := a.counts(); starts != 0 {
		t.Errorf("scan started %d times while switched off", starts)
	}
}

// Turning it on with nobody subscribed must not scan for no one.
func TestSwitchOnWithoutSubscribers(t *testing.T) {
	a := &scanCounter{}
	tc := startServer(t, a)
	tc.send(msgSubscribeStatesRequest, nil)
	tc.setSwitch("scanning", false)
	tc.expectBool(msgSwitchStateResponse, "scanning", false)
	tc.setSwitch("scanning", true)
	tc.expectBool(msgSwitchStateResponse, "scanning", true)
	if _, starts := a.counts(); starts != 0 {
		t.Errorf("scan started %d times with no subscriber", starts)
	}
}

func TestStackSensorStaysOnWhileSwitchedOff(t *testing.T) {
	tc := startServer(t, &fake.Adapter{})
	tc.subscribeAdverts()
	tc.send(msgSubscribeStatesRequest, nil)
	tc.setSwitch("scanning", false)
	tc.expectBool(msgSwitchStateResponse, "scanning", false)
	tc.send(msgSubscribeStatesRequest, nil)
	tc.expectBool(msgBinarySensorStateResponse, "bluetooth_stack", true)
}

// A command for an entity the proxy does not have is ignored, without closing the connection.
func TestUnknownEntityCommands(t *testing.T) {
	tc := startServer(t, testAdapter{})
	tc.setSwitch("no_such_switch", false)
	tc.pressButton("no_such_button")
	tc.send(msgPingRequest, nil)
	tc.expect(msgPingResponse)
}

func TestBadEntityCommandClosesTheClient(t *testing.T) {
	for _, typ := range []uint32{msgSwitchCommandRequest, msgButtonCommandRequest} {
		tc := startServer(t, testAdapter{})
		tc.send(typ, []byte{0xff})
		tc.c.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, err := ReadFrame(tc.r); err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					t.Fatalf("type %d: connection stayed open", typ)
				}
				break
			}
		}
	}
}

func TestRestartButtonListing(t *testing.T) {
	if _, ok := startServer(t, testAdapter{}).listAll()["restart_proxy"]; ok {
		t.Error("listed with nothing to restart")
	}
	got := startServerConfig(t, Config{Restart: func() {}}, testAdapter{}).listAll()["restart_proxy"]
	if got.typ != msgListEntitiesButtonResponse {
		t.Fatalf("listed as %+v", got)
	}
	if fieldValue(got.fs, 6) != 1 {
		t.Error("restart buttons must be disabled by default: the API may be unauthenticated")
	}
	if fieldValue(got.fs, 7) != entityConfig {
		t.Errorf("entity category %d", fieldValue(got.fs, 7))
	}
}

// TestRestartButtonDropsConnectionsFirst checks the order: the bluetooth connection goes before the proxy.
func TestRestartButtonDropsConnectionsFirst(t *testing.T) {
	conn := &recordingConn{}
	restarted := make(chan bool, 1)
	cfg := Config{Restart: func() { restarted <- conn.disconnected.Load() }}
	tc := startServerConfig(t, cfg, testAdapter{connect: func(ble.Events) (ble.Conn, error) { return conn, nil }})
	tc.connect()
	tc.pressButton("restart_proxy")
	select {
	case dropped := <-restarted:
		if !dropped {
			t.Error("restarted before dropping the connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not restarted")
	}
}

// A stuck stack is the reason to press the button, so it must not hold up the restart.
func TestRestartButtonDoesNotWaitForAStuckDisconnect(t *testing.T) {
	old := restartDropTimeout
	restartDropTimeout = 50 * time.Millisecond
	t.Cleanup(func() { restartDropTimeout = old })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	conn := &recordingConn{hang: release}
	restarted := make(chan struct{})
	cfg := Config{Restart: func() { close(restarted) }}
	tc := startServerConfig(t, cfg, testAdapter{connect: func(ble.Events) (ble.Conn, error) { return conn, nil }})
	tc.connect()
	tc.pressButton("restart_proxy")
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("restart waited for the stuck disconnect")
	}
}

// recordingConn is a connection that records Disconnect, which hangs until hang closes if set.
type recordingConn struct {
	testConn
	disconnected atomic.Bool
	hang         chan struct{}
}

func (c *recordingConn) Disconnect() error {
	if c.hang != nil {
		<-c.hang
	}
	c.disconnected.Store(true)
	return nil
}

// stackAdapter can restart its Bluetooth stack.
type stackAdapter struct {
	testAdapter
	available bool
	err       error
	presses   atomic.Int32
	pressed   chan struct{}
}

func (a *stackAdapter) StackRestartable() bool { return a.available }
func (a *stackAdapter) RestartStack() error {
	a.presses.Add(1)
	if a.pressed != nil {
		a.pressed <- struct{}{}
	}
	return a.err
}

func TestStackButtonListing(t *testing.T) {
	if _, ok := startServer(t, testAdapter{}).listAll()["restart_bluetooth"]; ok {
		t.Error("listed for an adapter that cannot restart its stack")
	}
	if _, ok := startServer(t, &stackAdapter{}).listAll()["restart_bluetooth"]; ok {
		t.Error("listed with no restart command")
	}
	got := startServer(t, &stackAdapter{available: true}).listAll()["restart_bluetooth"]
	if got.typ != msgListEntitiesButtonResponse || fieldValue(got.fs, 6) != 1 {
		t.Errorf("listed as %+v", got)
	}
}

func TestStackButtonPress(t *testing.T) {
	for _, err := range []error{nil, errors.New("too soon")} {
		a := &stackAdapter{available: true, err: err, pressed: make(chan struct{}, 1)}
		tc := startServer(t, a)
		tc.pressButton("restart_bluetooth")
		select {
		case <-a.pressed:
		case <-time.After(5 * time.Second):
			t.Fatal("not restarted")
		}
		tc.send(msgPingRequest, nil) // a refusal must not end the session
		tc.expect(msgPingResponse)
	}
}
