package api

import (
	"bufio"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

const testAddr = 0xAA_BB_CC_DD_EE_02

// testAdapter is a ble.Adapter whose Connect (and Unpair, if set) is supplied by the test.
type testAdapter struct {
	connect func(ev ble.Events) (ble.Conn, error)
	unpair  func(addr uint64) error
}

func (testAdapter) MAC() uint64                           { return 1 }
func (testAdapter) Scan(func(ble.Advert)) (func(), error) { return func() {}, nil }
func (testAdapter) ClearCache(uint64) error               { return nil }
func (a testAdapter) Connect(_ uint64, _ uint32, ev ble.Events) (ble.Conn, error) {
	return a.connect(ev)
}

// testConn records writes; delay, if set, makes each write take that long, and stuck, if set, holds every
// write until Disconnect. Pair returns pairErr.
type testConn struct {
	delay   func(i int) time.Duration
	stuck   chan struct{}
	once    sync.Once
	pairErr error

	mu     sync.Mutex
	writes [][]byte
}

func (c *testConn) MTU() uint32                           { return 23 }
func (c *testConn) Services() ([]ble.Service, error)      { return nil, nil }
func (c *testConn) Read(uint16) ([]byte, error)           { return nil, errors.New("not readable") }
func (c *testConn) ReadDescriptor(uint16) ([]byte, error) { return nil, errors.New("not readable") }
func (c *testConn) WriteDescriptor(uint16, []byte) error  { return nil }
func (c *testConn) Notify(uint16, bool) error             { return nil }
func (c *testConn) Disconnect() error {
	if c.stuck != nil {
		c.once.Do(func() { close(c.stuck) })
	}
	return nil
}

func (c *testConn) Write(_ uint16, data []byte, _ bool) error {
	c.mu.Lock()
	i := len(c.writes)
	c.mu.Unlock()
	if c.delay != nil {
		time.Sleep(c.delay(i))
	}
	if c.stuck != nil {
		<-c.stuck
		return ble.ErrNotConnected
	}
	c.mu.Lock()
	c.writes = append(c.writes, data)
	c.mu.Unlock()
	return nil
}

// testClient speaks the plaintext API to a server started for one test.
type testClient struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func startServer(t *testing.T, a ble.Adapter) *testClient {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go NewServer(Config{Name: "test"}, a, logx.Discard()).Serve(l)
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &testClient{t: t, c: c, r: bufio.NewReader(c)}
}

func (tc *testClient) send(typ uint32, payload []byte) {
	if _, err := tc.c.Write(AppendFrame(nil, typ, payload)); err != nil {
		tc.t.Fatal(err)
	}
}

// expect reads frames until one of the given type arrives and returns its fields.
func (tc *testClient) expect(typ uint32) []field {
	tc.t.Helper()
	tc.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		f, err := ReadFrame(tc.r)
		if err != nil {
			tc.t.Fatalf("waiting for message %d: %v", typ, err)
		}
		if f.Type == typ {
			fs, err := fields(f.Payload)
			if err != nil {
				tc.t.Fatal(err)
			}
			return fs
		}
	}
}

func fieldValue(fs []field, num int) uint64 {
	for _, f := range fs {
		if int(f.num) == num {
			return f.v
		}
	}
	return 0
}

func (tc *testClient) connect() []field {
	var e enc
	e.uint(1, testAddr)
	e.uint(2, reqConnectV3WithCache)
	tc.send(msgBTDeviceRequest, e.b)
	return tc.expect(msgBTDeviceConnectionResponse)
}

// TestLinkLostWhileConnecting drops the link after the backend has connected but before the server has taken
// the connection: the slot must be freed and the client told it is not connected.
func TestLinkLostWhileConnecting(t *testing.T) {
	tc := startServer(t, testAdapter{connect: func(ev ble.Events) (ble.Conn, error) {
		ev.Disconnected(errors.New("link lost"))
		return &testConn{}, nil
	}})
	if r := tc.connect(); fieldValue(r, 2) != 0 {
		t.Fatalf("reported connected: %+v", r)
	}
	tc.send(msgSubscribeBTConnectionsFree, nil)
	if free := tc.expect(msgBTConnectionsFreeResponse); fieldValue(free, 1) != 3 {
		t.Errorf("slot not freed: %+v", free)
	}
}

// TestWritesInOrder sends 50 writes without response back to back; they must reach the device in order even
// when the backend takes a varying time per write.
func TestWritesInOrder(t *testing.T) {
	tconn := &testConn{delay: func(i int) time.Duration { return time.Duration(i%7) * time.Millisecond }}
	tc := startServer(t, testAdapter{connect: func(ble.Events) (ble.Conn, error) { return tconn, nil }})
	if r := tc.connect(); fieldValue(r, 2) != 1 {
		t.Fatalf("not connected: %+v", r)
	}
	const n = 50
	for i := range n + 1 {
		var e enc
		e.uint(1, testAddr)
		e.uint(2, 0x0c)
		e.bool(3, i == n) // the last one with response, to know when all are done
		e.bytes(4, []byte{byte(i), 0xff})
		tc.send(msgBTGATTWriteRequest, e.b)
	}
	tc.expect(msgBTGATTWriteResponse)
	tconn.mu.Lock()
	defer tconn.mu.Unlock()
	if len(tconn.writes) != n+1 {
		t.Fatalf("%d writes arrived, want %d", len(tconn.writes), n+1)
	}
	for i, w := range tconn.writes {
		if int(w[0]) != i {
			t.Fatalf("write %d carried %d", i, w[0])
		}
	}
}

// TestDisconnectNotBlocked checks that a disconnect goes through while a GATT request hangs on the device.
func TestDisconnectNotBlocked(t *testing.T) {
	tconn := &testConn{stuck: make(chan struct{})}
	tc := startServer(t, testAdapter{connect: func(ble.Events) (ble.Conn, error) { return tconn, nil }})
	if r := tc.connect(); fieldValue(r, 2) != 1 {
		t.Fatalf("not connected: %+v", r)
	}
	for range 2 { // the second write waits in the queue behind the first
		var e enc
		e.uint(1, testAddr)
		e.uint(2, 0x0c)
		e.bool(3, true)
		e.bytes(4, []byte{1})
		tc.send(msgBTGATTWriteRequest, e.b)
	}
	var e enc
	e.uint(1, testAddr)
	e.uint(2, reqDisconnect)
	tc.send(msgBTDeviceRequest, e.b)
	// The disconnect reply and the two write errors may arrive in any order.
	errs, disconnected := 0, false
	tc.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for errs < 2 || !disconnected {
		f, err := ReadFrame(tc.r)
		if err != nil {
			t.Fatalf("disconnected=%v, %d write errors: %v", disconnected, errs, err)
		}
		switch f.Type {
		case msgBTGATTErrorResponse:
			errs++
		case msgBTDeviceConnectionResponse:
			fs, _ := fields(f.Payload)
			if fieldValue(fs, 2) != 0 {
				t.Fatalf("still connected: %+v", fs)
			}
			disconnected = true
		}
	}
}

// TestLastClientLeavesWhileConnecting closes the only client while a connect is still running: the connection
// must be dropped when it completes, not kept on a slot nobody will free.
func TestLastClientLeavesWhileConnecting(t *testing.T) {
	release, disconnected := make(chan struct{}), make(chan struct{})
	tconn := &testConn{stuck: disconnected}
	tc := startServer(t, testAdapter{connect: func(ble.Events) (ble.Conn, error) {
		<-release
		return tconn, nil
	}})
	var e enc
	e.uint(1, testAddr)
	e.uint(2, reqConnectV3WithCache)
	tc.send(msgBTDeviceRequest, e.b)
	time.Sleep(100 * time.Millisecond) // let the server start the connect
	tc.c.Close()
	time.Sleep(100 * time.Millisecond) // let it notice the client is gone
	close(release)
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("connection completed after the last client left was not disconnected")
	}
}

// TestConnectWithdrawn checks what Events.Withdrawn tells a backend that retries: false while the connect is
// wanted, true once a disconnect is requested.
func TestConnectWithdrawn(t *testing.T) {
	asked, answer := make(chan struct{}), make(chan bool, 2)
	tc := startServer(t, testAdapter{connect: func(ev ble.Events) (ble.Conn, error) {
		answer <- ev.Withdrawn()
		<-asked
		answer <- ev.Withdrawn()
		return nil, errors.New("le-connection-abort-by-local")
	}})
	var e enc
	e.uint(1, testAddr)
	e.uint(2, reqConnectV3WithCache)
	tc.send(msgBTDeviceRequest, e.b)
	if <-answer {
		t.Fatal("withdrawn before any disconnect")
	}
	e = enc{}
	e.uint(1, testAddr)
	e.uint(2, reqDisconnect)
	tc.send(msgBTDeviceRequest, e.b)
	if r := tc.expect(msgBTDeviceConnectionResponse); fieldValue(r, 2) != 0 {
		t.Fatalf("reported connected: %+v", r)
	}
	close(asked)
	if !<-answer {
		t.Error("not withdrawn after a disconnect request")
	}
}
