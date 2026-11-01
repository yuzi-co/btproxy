//go:build linux

package hci

import (
	"testing"
	"time"

	"btproxy/internal/ble"
)

// TestOpen opens the monitor where the kernel has Bluetooth and the test CAP_NET_RAW, which checks the socket
// options and the filter the kernel accepts, and that Close stops Run.
func TestOpen(t *testing.T) {
	m, err := Open(0)
	if err != nil {
		t.Skip("no HCI monitor here:", err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Run(func(ble.Advert) {}, func() bool { return false }) }()
	time.Sleep(50 * time.Millisecond)
	m.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * readWake):
		t.Fatal("Run did not return after Close")
	}
}
