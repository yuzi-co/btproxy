package fake

import (
	"testing"

	"btproxy/internal/ble"
)

// TestUnpair checks that unpairing closes the connection to the fake device and that unpairing an unknown
// device succeeds, as with the bluez backend.
func TestUnpair(t *testing.T) {
	a := &Adapter{}
	if err := a.Unpair(0xAABBCCDDEE99); err != nil {
		t.Fatalf("unknown device: %v", err)
	}
	c, err := a.Connect(FakeAddr, 1, ble.Events{Notification: func(uint16, []byte) {}, Disconnected: func(error) {}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Unpair(FakeAddr); err != nil {
		t.Fatal(err)
	}
	if err := c.Disconnect(); err != ble.ErrNotConnected {
		t.Fatalf("connection still open after unpairing (Disconnect: %v)", err)
	}
}
