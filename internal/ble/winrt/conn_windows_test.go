package winrt

import (
	"errors"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/winrt/gen/windows/devices/bluetooth"
)

func TestATTError(t *testing.T) {
	if s := attError(0x05).Error(); s != "ATT error 0x05 (insufficient authentication)" {
		t.Errorf("named code: %q", s)
	}
	if s := attError(0x80).Error(); s != "ATT error 0x80" {
		t.Errorf("application code: %q", s)
	}
	if !errors.Is(attError(0x0a), errProtocol) {
		t.Error("an ATT error is not an errProtocol")
	}
	if errors.Is(attError(0x0a), errAccessDenied) || errors.Is(errProtocol, attError(0x0a)) {
		t.Error("an ATT error matches more than errProtocol")
	}
	// The HRESULT form of an ATT error, FACILITY_BLUETOOTH_ATT, is the same type.
	var e attError
	if err := hresultError(0x80650005); !errors.As(err, &e) || e != 0x05 {
		t.Errorf("HRESULT 0x80650005: %v", err)
	}
	if !radioFailure(errUnreachable) || radioFailure(attError(0x0e)) {
		t.Error("an ATT error counts as a radio failure")
	}
}

// The address is claimed once; a second connect to it fails until the first releases.
func TestClaimAddress(t *testing.T) {
	a := testAdapter(t)
	c := testConn(t, a, 1)
	if _, err := a.claim(&conn{a: a, addr: 1}); err == nil {
		t.Fatal("claimed twice")
	}
	a.uncached[2] = true
	if mode, err := a.claim(&conn{a: a, addr: 2}); err != nil || mode != bluetooth.BluetoothCacheModeUncached {
		t.Fatalf("uncached claim: mode %d, err %v", mode, err)
	}
	a.clearUncached(2)
	if a.uncached[2] {
		t.Fatal("still uncached")
	}
	if !c.release() {
		t.Fatal("first release did not tear down")
	}
	if a.connTo(1) != nil {
		t.Fatal("address still taken after release")
	}
	if c.release() {
		t.Fatal("second release tore down again")
	}
	// Another connection to the address is not unclaimed by the old one's release.
	c2 := testConn(t, a, 1)
	a.unclaim(c)
	if a.connTo(1) != c2 {
		t.Fatal("new connection unclaimed by the old one")
	}
}

// A released connection refuses new calls, is not retried, and does not start another connect attempt.
func TestReleasedConnRefusesCalls(t *testing.T) {
	a := testAdapter(t)
	c := testConn(t, a, 1)
	if !c.reset() {
		t.Fatal("reset of a live connection says not to retry")
	}
	if err := c.begin(); err != nil {
		t.Fatal(err)
	}
	c.end()
	c.release()
	if c.reset() {
		t.Fatal("reset of a released connection says to retry")
	}
	if err := c.begin(); !errors.Is(err, ble.ErrNotConnected) {
		t.Fatalf("begin after release: %v", err)
	}
	if err := c.attempt(ble.AddrPublic, 0, time.Now().Add(time.Second)); !errors.Is(err, errClosed) {
		t.Fatalf("attempt after release: %v", err)
	}
	if _, err := c.Read(1); !errors.Is(err, ble.ErrNotConnected) {
		t.Fatalf("Read after release: %v", err)
	}
	if c.MTU() != 23 {
		t.Fatal("MTU after release")
	}
	if c.live() {
		t.Fatal("live after release")
	}
	// Events from the device are ignored once released: neither reaches the WinRT objects, which are gone.
	c.statusChanged()
	c.valueChanged(1, nil)
}

// release waits for a call in flight, and the call sees the connection closed only once it is done.
func TestReleaseWaitsForCalls(t *testing.T) {
	a := testAdapter(t)
	c := testConn(t, a, 1)
	if err := c.begin(); err != nil {
		t.Fatal(err)
	}
	released := make(chan bool)
	go func() { released <- c.release() }()
	select {
	case <-released:
		t.Fatal("released during a call")
	case <-time.After(50 * time.Millisecond):
	}
	if !c.isClosed() {
		t.Fatal("not closed while release waits")
	}
	c.end()
	if !<-released {
		t.Fatal("release did not tear down")
	}
}
