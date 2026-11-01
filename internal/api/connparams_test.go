package api

import (
	"errors"
	"sync"
	"testing"

	"btproxy/internal/ble"
)

// paramsAdapter is a testAdapter that can change connection parameters; it records what it was asked.
type paramsAdapter struct {
	testAdapter
	err error

	mu  sync.Mutex
	got []ble.ConnParams
}

func (a *paramsAdapter) SetConnParams(addr uint64, p ble.ConnParams) error {
	if addr != testAddr {
		return errors.New("wrong address")
	}
	a.mu.Lock()
	a.got = append(a.got, p)
	a.mu.Unlock()
	return a.err
}

func newParamsAdapter(err error) *paramsAdapter {
	return &paramsAdapter{err: err, testAdapter: testAdapter{
		connect: func(ble.Events) (ble.Conn, error) { return &testConn{}, nil }}}
}

func (tc *testClient) setConnParams(minIv, maxIv, latency, timeout uint64) []field {
	var e enc
	e.uint(1, testAddr)
	e.uint(2, minIv)
	e.uint(3, maxIv)
	e.uint(4, latency)
	e.uint(5, timeout)
	tc.send(msgBTSetConnParamsRequest, e.b)
	return tc.expect(msgBTSetConnParamsResponse)
}

func checkParamsResult(t *testing.T, fs []field, code int32) {
	t.Helper()
	if fieldValue(fs, 1) != testAddr || int32(fieldValue(fs, 2)) != code {
		t.Fatalf("got %+v, want error=%d", fs, code)
	}
}

// TestConnParamsFeature advertises the feature only for adapters that implement ble.ConnParamsSetter.
func TestConnParamsFeature(t *testing.T) {
	for _, tt := range []struct {
		a    ble.Adapter
		want bool
	}{{testAdapter{}, false}, {newParamsAdapter(nil), true}} {
		tc := startServer(t, tt.a)
		tc.send(msgDeviceInfoRequest, nil)
		if got := fieldValue(tc.expect(msgDeviceInfoResponse), 15)&featureConnParams != 0; got != tt.want {
			t.Errorf("%T: connection params advertised %v", tt.a, got)
		}
	}
}

func TestConnParams(t *testing.T) {
	a := newParamsAdapter(nil)
	tc := startServer(t, a)
	if r := tc.connect(); fieldValue(r, 2) != 1 {
		t.Fatalf("not connected: %+v", r)
	}
	checkParamsResult(t, tc.setConnParams(24, 40, 0, 500), 0)
	checkParamsResult(t, tc.setConnParams(0x10006, 0x1ffff, 1, 3200), 0) // clamped to 16 bits
	a.mu.Lock()
	defer a.mu.Unlock()
	want := []ble.ConnParams{{MinInterval: 24, MaxInterval: 40, Timeout: 500},
		{MinInterval: 0xffff, MaxInterval: 0xffff, Latency: 1, Timeout: 3200}}
	if len(a.got) != 2 || a.got[0] != want[0] || a.got[1] != want[1] {
		t.Errorf("backend got %+v, want %+v", a.got, want)
	}
}

func TestConnParamsNotConnected(t *testing.T) {
	a := newParamsAdapter(nil)
	tc := startServer(t, a)
	checkParamsResult(t, tc.setConnParams(24, 40, 0, 500), gattNotConnected)
	if len(a.got) != 0 {
		t.Errorf("backend called: %+v", a.got)
	}
}

func TestConnParamsFails(t *testing.T) {
	tc := startServer(t, newParamsAdapter(errors.New("HCI status 0x12")))
	if r := tc.connect(); fieldValue(r, 2) != 1 {
		t.Fatalf("not connected: %+v", r)
	}
	checkParamsResult(t, tc.setConnParams(24, 40, 0, 500), gattError)
}

// TestConnParamsUnsupported answers a client that ignores the feature flags with an error.
func TestConnParamsUnsupported(t *testing.T) {
	tc := startServer(t, testAdapter{connect: func(ble.Events) (ble.Conn, error) { return &testConn{}, nil }})
	if r := tc.connect(); fieldValue(r, 2) != 1 {
		t.Fatalf("not connected: %+v", r)
	}
	checkParamsResult(t, tc.setConnParams(24, 40, 0, 500), gattError)
}
