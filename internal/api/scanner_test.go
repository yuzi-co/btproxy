package api

import (
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/fake"
)

func (tc *testClient) subscribeAdverts() {
	var e enc
	e.uint(1, 1) // raw advertisements
	tc.send(msgSubscribeBTAdvertisements, e.b)
}

func (tc *testClient) setMode(m ble.ScanMode) {
	var e enc
	e.uint(1, uint64(m))
	tc.send(msgBTScannerSetModeRequest, e.b)
}

// expectState reads scanner state reports until one matches.
func (tc *testClient) expectState(state ble.ScanState, mode, configured ble.ScanMode) {
	tc.t.Helper()
	for {
		fs := tc.expect(msgBTScannerStateResponse)
		if fieldValue(fs, 1) == uint64(state) && fieldValue(fs, 2) == uint64(mode) &&
			fieldValue(fs, 3) == uint64(configured) {
			return
		}
	}
}

// TestScannerStateActiveOnly checks that an adapter without mode support reports active scanning and
// keeps doing so when asked for passive.
func TestScannerStateActiveOnly(t *testing.T) {
	tc := startServer(t, testAdapter{})
	tc.subscribeAdverts()
	tc.expectState(ble.ScanRunning, ble.ScanActive, ble.ScanActive)
	tc.setMode(ble.ScanPassive)
	tc.expectState(ble.ScanRunning, ble.ScanActive, ble.ScanActive)
}

// TestScannerModeAndLoss switches modes, then loses the adapter with a device connected: the scanner must
// report the failure, the connection must be reported down and its slot freed, and scanning must resume
// when the adapter returns.
func TestScannerModeAndLoss(t *testing.T) {
	a := &fake.Adapter{}
	tc := startServer(t, a) // configured passive (the zero Config)
	tc.subscribeAdverts()
	tc.expectState(ble.ScanRunning, ble.ScanPassive, ble.ScanPassive)
	tc.setMode(ble.ScanActive)
	tc.expectState(ble.ScanRunning, ble.ScanActive, ble.ScanPassive)

	var e enc
	e.uint(1, fake.FakeAddr)
	e.uint(2, reqConnectV3WithCache)
	tc.send(msgBTDeviceRequest, e.b)
	if r := tc.expect(msgBTDeviceConnectionResponse); fieldValue(r, 2) != 1 {
		t.Fatalf("not connected: %+v", r)
	}

	a.Lose()
	if r := tc.expect(msgBTDeviceConnectionResponse); fieldValue(r, 1) != fake.FakeAddr || fieldValue(r, 2) != 0 {
		t.Fatalf("connection not reported down: %+v", r)
	}
	if free := tc.expect(msgBTConnectionsFreeResponse); fieldValue(free, 1) != 3 {
		t.Errorf("slot not freed: %+v", free)
	}
	tc.expectState(ble.ScanFailed, ble.ScanActive, ble.ScanPassive)

	a.Restore()
	tc.expectState(ble.ScanRunning, ble.ScanActive, ble.ScanPassive)
	tc.expect(msgBTRawAdvertisementsResponse)
}
