package winrt

import (
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
	gatt "github.com/yuzi-co/btproxy/internal/ble/winrt/gen/windows/devices/bluetooth/genericattributeprofile"
	"github.com/yuzi-co/btproxy/internal/logx"
)

// testAdapter is an Adapter with no radio behind it, for tests of the bookkeeping around connections and
// adverts. Tests that need a scan running use newTestAdapter in pause_windows_test.go.
func testAdapter(t *testing.T) *Adapter {
	t.Helper()
	return &Adapter{log: logx.Discard(), mode: ble.ScanActive, conns: map[uint64]*conn{}, uncached: map[uint64]bool{}}
}

// testConn is a connection to addr registered with a, with no WinRT objects behind it: what Connect has
// before its first attempt.
func testConn(t *testing.T, a *Adapter, addr uint64) *conn {
	t.Helper()
	c := &conn{a: a, addr: addr, upCh: make(chan struct{}, 1), chars: map[uint16]*char{},
		descs: map[uint16]*gatt.GattDescriptor{}, cccd: map[uint16]uint16{}}
	if _, err := a.claim(c); err != nil {
		t.Fatal(err)
	}
	return c
}
