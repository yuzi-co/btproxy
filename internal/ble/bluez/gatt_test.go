//go:build linux

package bluez

import (
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// TestReadEcho checks that the Value change BlueZ emits for our own ReadValue is not forwarded as a
// notification, whether it is seen during the read or after it, while real notifications still are.
// Value changes held during a read are capped, keeping the newest.
func TestHeldValuesCapped(t *testing.T) {
	const h = 0x0c
	p := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a/char000b")
	var got []byte
	c := &conn{notifying: map[uint16]bool{h: true}, reading: map[uint16]int{}, held: map[uint16][][]byte{},
		echoes: map[uint16][]echo{},
		ev:     ble.Events{Notification: func(_ uint16, d []byte) { got = append(got, d[0]) }}}
	c.readStarted(h)
	for i := range 2 * maxHeldValue {
		c.valueChanged(p, []byte{byte(i)})
	}
	c.readDone(h, nil, false)
	if len(got) != maxHeldValue || got[len(got)-1] != 2*maxHeldValue-1 {
		t.Fatalf("%d values, last %d", len(got), got[len(got)-1])
	}
}

func TestReadEcho(t *testing.T) {
	const h = 0x0c
	p := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a/char000b")
	var got []string
	c := &conn{notifying: map[uint16]bool{h: true}, reading: map[uint16]int{}, held: map[uint16][][]byte{},
		echoes: map[uint16][]echo{},
		ev: ble.Events{Notification: func(hh uint16, d []byte) {
			if hh != h {
				t.Errorf("notification for %#x", hh)
			}
			got = append(got, string(d))
		}}}

	// Echo seen while the read is in flight, next to a real notification.
	c.readStarted(h)
	c.valueChanged(p, []byte("n1"))
	c.valueChanged(p, []byte("r1"))
	c.readDone(h, []byte("r1"), true)
	// Echo seen after the read returned; a later notification with the same value is real.
	c.readStarted(h)
	c.readDone(h, []byte("r2"), true)
	c.valueChanged(p, []byte("n2"))
	c.valueChanged(p, []byte("r2"))
	c.valueChanged(p, []byte("r2"))
	// A failed read expects no echo.
	c.readStarted(h)
	c.readDone(h, nil, false)
	c.valueChanged(p, nil)

	want := []string{"n1", "n2", "r2", ""}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("notifications %q, want %q", got, want)
	}
}

// TestFirstNotificationBeforeTheReply: BlueZ's reply to StartNotify and the first Value change reach the
// adapter on different goroutines, so the value can be handled before Notify sees the reply. It must not be
// dropped, and a StartNotify that fails leaves notifications off.
func TestFirstNotificationBeforeTheReply(t *testing.T) {
	a, bz := openForRetries(t, 0, 0)
	bz.exportChar(fakeChar)
	got := make(chan []byte, 8)
	c := newTestConn(t, a, ble.Events{Notification: func(_ uint16, d []byte) { got <- d }})

	if err := c.Notify(fakeCharHandle, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the first notification was dropped")
	}
	if err := c.Notify(fakeCharHandle, false); err != nil {
		t.Fatal(err)
	}

	bz.mu.Lock()
	bz.notifyFail = true
	bz.mu.Unlock()
	if err := c.Notify(fakeCharHandle, true); err == nil {
		t.Fatal("StartNotify failure not reported")
	}
	if c.isNotifying(fakeCharHandle) {
		t.Fatal("notifying after a failed StartNotify")
	}
}
