package api

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"btproxy/internal/ble"
)

const testAddr = 0xAA_BB_CC_DD_EE_02

// testAdapter is a ble.Adapter whose Connect is supplied by the test.
type testAdapter struct {
	connect func(ev ble.Events) (ble.Conn, error)
}

func (testAdapter) MAC() uint64                           { return 1 }
func (testAdapter) Scan(func(ble.Advert)) (func(), error) { return func() {}, nil }
func (testAdapter) ClearCache(uint64) error               { return nil }
func (a testAdapter) Connect(_ uint64, _ uint32, ev ble.Events) (ble.Conn, error) {
	return a.connect(ev)
}

// testConn records writes; delay, if set, makes each write take that long.
type testConn struct {
	delay func(i int) time.Duration

	mu     sync.Mutex
	writes [][]byte
}

func (c *testConn) MTU() uint32                           { return 23 }
func (c *testConn) Services() ([]ble.Service, error)      { return nil, nil }
func (c *testConn) Read(uint16) ([]byte, error)           { return nil, errors.New("not readable") }
func (c *testConn) ReadDescriptor(uint16) ([]byte, error) { return nil, errors.New("not readable") }
func (c *testConn) WriteDescriptor(uint16, []byte) error  { return nil }
func (c *testConn) Notify(uint16, bool) error             { return nil }
func (c *testConn) Disconnect() error                     { return nil }

func (c *testConn) Write(_ uint16, data []byte, _ bool) error {
	c.mu.Lock()
	i := len(c.writes)
	c.mu.Unlock()
	if c.delay != nil {
		time.Sleep(c.delay(i))
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
	go NewServer(Config{Name: "test"}, a, slog.New(slog.NewTextHandler(io.Discard, nil))).Serve(l)
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
