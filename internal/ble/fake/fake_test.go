package fake

import (
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
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

// TestNotifyRestart turns notifications off and on again before the first one is sent: one loop must run,
// not two, and turning them off stops it.
func TestNotifyRestart(t *testing.T) {
	old := notifyEvery
	notifyEvery = 10 * time.Millisecond
	t.Cleanup(func() { notifyEvery = old })
	a := &Adapter{}
	got := make(chan byte, 64)
	c, err := a.Connect(FakeAddr, 1, ble.Events{Notification: func(_ uint16, d []byte) { got <- d[0] },
		Disconnected: func(error) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect()
	for _, on := range []bool{true, false, true} {
		if err := c.Notify(HandleEcho, on); err != nil {
			t.Fatal(err)
		}
	}
	var seen []byte
	deadline := time.After(2 * time.Second)
	for len(seen) < 5 {
		select {
		case v := <-got:
			seen = append(seen, v)
		case <-deadline:
			t.Fatalf("only %v within the deadline", seen)
		}
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[i-1]+1 {
			t.Fatalf("notifications %v, want one counter", seen)
		}
	}

	if err := c.Notify(HandleEcho, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * notifyEvery) // one may have been on its way
	for len(got) > 0 {
		<-got
	}
	time.Sleep(5 * notifyEvery)
	if n := len(got); n != 0 {
		t.Fatalf("%d notifications after turning them off", n)
	}
}
