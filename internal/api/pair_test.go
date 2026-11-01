package api

import (
	"errors"
	"sync/atomic"
	"testing"

	"btproxy/internal/ble"
)

func (a testAdapter) Unpair(addr uint64) error {
	if a.unpair == nil {
		return nil
	}
	return a.unpair(addr)
}

func (c *testConn) Pair() error { return c.pairErr }

func (tc *testClient) deviceRequest(kind uint32) {
	var e enc
	e.uint(1, testAddr)
	e.uint(2, uint64(kind))
	tc.send(msgBTDeviceRequest, e.b)
}

// checkResult checks the address, the success flag and the error code of a pairing or unpairing response.
func checkResult(t *testing.T, fs []field, ok bool, code int32) {
	t.Helper()
	if fieldValue(fs, 1) != testAddr || (fieldValue(fs, 2) == 1) != ok || int32(fieldValue(fs, 3)) != code {
		t.Fatalf("got %+v, want ok=%v error=%d", fs, ok, code)
	}
}

func TestPair(t *testing.T) {
	tc := startServer(t, testAdapter{connect: func(ble.Events) (ble.Conn, error) { return &testConn{}, nil }})
	if r := tc.connect(); fieldValue(r, 2) != 1 {
		t.Fatalf("not connected: %+v", r)
	}
	tc.deviceRequest(reqPair)
	checkResult(t, tc.expect(msgBTDevicePairingResponse), true, 0)
}

func TestPairNotConnected(t *testing.T) {
	tc := startServer(t, testAdapter{})
	tc.deviceRequest(reqPair)
	checkResult(t, tc.expect(msgBTDevicePairingResponse), false, gattNotConnected)
}

func TestPairFails(t *testing.T) {
	tconn := &testConn{pairErr: errors.New("authentication failed")}
	tc := startServer(t, testAdapter{connect: func(ble.Events) (ble.Conn, error) { return tconn, nil }})
	if r := tc.connect(); fieldValue(r, 2) != 1 {
		t.Fatalf("not connected: %+v", r)
	}
	tc.deviceRequest(reqPair)
	checkResult(t, tc.expect(msgBTDevicePairingResponse), false, gattError)
}

// TestUnpairConnected unpairs a connected device: the unpairing response must come before the disconnection,
// and the slot must be free afterwards. expect skips other messages, so a disconnection sent first would be
// skipped and the second expect would time out.
func TestUnpairConnected(t *testing.T) {
	var unpaired atomic.Uint64
	tc := startServer(t, testAdapter{
		connect: func(ble.Events) (ble.Conn, error) { return &testConn{}, nil },
		unpair:  func(addr uint64) error { unpaired.Store(addr); return nil },
	})
	if r := tc.connect(); fieldValue(r, 2) != 1 {
		t.Fatalf("not connected: %+v", r)
	}
	tc.deviceRequest(reqUnpair)
	checkResult(t, tc.expect(msgBTDeviceUnpairingResponse), true, 0)
	if r := tc.expect(msgBTDeviceConnectionResponse); fieldValue(r, 2) != 0 {
		t.Fatalf("still connected: %+v", r)
	}
	if a := unpaired.Load(); a != testAddr {
		t.Errorf("backend unpaired %x", a)
	}
	tc.send(msgSubscribeBTConnectionsFree, nil)
	if free := tc.expect(msgBTConnectionsFreeResponse); fieldValue(free, 1) != 3 {
		t.Errorf("slot not freed: %+v", free)
	}
}

// TestUnpairNotConnected unpairs a device without a connection, which ESPHome allows.
func TestUnpairNotConnected(t *testing.T) {
	tc := startServer(t, testAdapter{})
	tc.deviceRequest(reqUnpair)
	checkResult(t, tc.expect(msgBTDeviceUnpairingResponse), true, 0)
}

// TestUnpairFails reports a backend failure; a connection the request found is dropped all the same.
func TestUnpairFails(t *testing.T) {
	tconn := &testConn{stuck: make(chan struct{})}
	tc := startServer(t, testAdapter{
		connect: func(ble.Events) (ble.Conn, error) { return tconn, nil },
		unpair:  func(uint64) error { return errors.New("remove failed") },
	})
	if r := tc.connect(); fieldValue(r, 2) != 1 {
		t.Fatalf("not connected: %+v", r)
	}
	tc.deviceRequest(reqUnpair)
	checkResult(t, tc.expect(msgBTDeviceUnpairingResponse), false, gattError)
	if r := tc.expect(msgBTDeviceConnectionResponse); fieldValue(r, 2) != 0 {
		t.Fatalf("still connected: %+v", r)
	}
	select {
	case <-tconn.stuck:
	default:
		t.Error("connection not disconnected")
	}
}
