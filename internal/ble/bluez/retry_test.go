//go:build linux

package bluez

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/bletest"
)

func TestRadioFailure(t *testing.T) {
	failed := func(body ...any) error {
		return fmt.Errorf("wrapped: %w", dbus.Error{Name: "org.bluez.Error.Failed", Body: body})
	}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{failed("le-connection-abort-by-local"), true},
		{failed(""), true},
		{dbus.Error{Name: "org.bluez.Failed", Body: []any{""}}, true}, // what BlueZ 5.66 sends
		{dbus.Error{Name: "org.bluez.Failed", Body: []any{"Operation already in progress"}}, false},
		{failed("Page Timeout"), false},
		{failed("le-connection-abort-by-remote"), false},
		{failed(), false},
		{failed(42), false},
		{dbus.Error{Name: "org.bluez.Error.DoesNotExist", Body: []any{"Does Not Exist"}}, false},
		{dbus.Error{Name: "org.bluez.Error.DoesNotExist", Body: []any{""}}, false},
		{dbus.Error{Name: "org.freedesktop.DBus.Error.UnknownObject", Body: []any{""}}, false},
		{context.DeadlineExceeded, false},
		{fmt.Errorf("connect: %w", context.Canceled), false},
	} {
		if got := radioFailure(tc.err); got != tc.want {
			t.Errorf("radioFailure(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// openForRetries opens an adapter that knows the fake device, which loses the link on its first drops connects.
func openForRetries(t *testing.T, retries, drops int) (*Adapter, *fakeBlueZ) {
	t.Helper()
	bz := startFakeBlueZ(t, startBus(t), false)
	t.Cleanup(func() { bz.bus.Close() })
	a, err := Open("hci0", Options{RawAdverts: RawOff, ConnectRetries: retries})
	if err != nil {
		t.Fatal(err)
	}
	bz.advertise()
	bletest.WaitFor(t, "device", func() bool { _, ok := rssiOf(a, fakeDevice); return ok })
	bz.mu.Lock()
	bz.dropLinks = drops
	bz.mu.Unlock()
	return a, bz
}

var retryEvents = ble.Events{Notification: func(uint16, []byte) {}, Disconnected: func(error) {}}

// A link can come up and drop a few seconds later, before the services are resolved. On a router next to its
// Wi-Fi radio that was the usual way for a connect to fail, and the client gets the same answer from a new try.
func TestLinkLostWhileResolvingIsRetried(t *testing.T) {
	a, bz := openForRetries(t, 4, 2)
	c, err := a.Connect(0xAABBCCDDEEFF, ble.AddrPublic, retryEvents)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect()
	if n := len(bz.connectCalls()); n != 3 {
		t.Errorf("%d connect calls, want 3", n)
	}
}

func TestLinkLostWhileResolvingUsesTheRetryLimit(t *testing.T) {
	a, bz := openForRetries(t, 1, 5)
	_, err := a.Connect(0xAABBCCDDEEFF, ble.AddrPublic, retryEvents)
	if err == nil || !strings.Contains(err.Error(), "link lost while resolving services") {
		t.Fatalf("error %v", err)
	}
	if n := len(bz.connectCalls()); n != 2 {
		t.Errorf("%d connect calls, want 2", n)
	}
	a.mu.Lock()
	left := len(a.conns)
	a.mu.Unlock()
	if left != 0 {
		t.Errorf("%d connections left behind", left)
	}
}

func TestLinkLostWhileResolvingNotRetriedWhenWithdrawn(t *testing.T) {
	a, bz := openForRetries(t, 4, 5)
	ev := retryEvents
	ev.Withdrawn = func() bool { return true }
	if _, err := a.Connect(0xAABBCCDDEEFF, ble.AddrPublic, ev); err == nil {
		t.Fatal("connect did not fail")
	}
	if n := len(bz.connectCalls()); n != 1 {
		t.Errorf("%d connect calls, want 1", n)
	}
}

// A radio failure that took long is not the quick failure retries are for, unlike a link that was up for a while.
func TestSlowRadioFailureIsNotRetried(t *testing.T) {
	a, bz := openForRetries(t, 4, 0)
	hold := make(chan struct{})
	bz.mu.Lock()
	bz.connectFail = true
	bz.hold = hold
	bz.mu.Unlock()
	time.AfterFunc(retryFast+200*time.Millisecond, func() { close(hold) })
	if _, err := a.Connect(0xAABBCCDDEEFF, ble.AddrPublic, retryEvents); err == nil {
		t.Fatal("connect did not fail")
	}
	if n := len(bz.connectCalls()); n != 1 {
		t.Errorf("%d connect calls, want 1", n)
	}
}

// Services resolved on a link that then dropped say nothing about the next attempt's link.
func TestRearmForgetsResolvedServices(t *testing.T) {
	a, _ := openForRetries(t, 0, 0)
	c := &conn{a: a, path: fakeDevice, resolved: make(chan struct{}), lost: make(chan struct{})}
	c.markResolved()
	c.markResolved() // a second signal must not close it twice
	if err := c.rearm(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		a.mu.Lock()
		delete(a.conns, fakeDevice)
		a.mu.Unlock()
		c.unwatch()
	}()
	select {
	case <-c.resolvedCh():
		t.Fatal("still resolved after rearm")
	default:
	}
	c.markResolved()
	select {
	case <-c.resolvedCh():
	default:
		t.Fatal("not resolved by the new attempt")
	}
}

func TestConnectRetryOptions(t *testing.T) {
	if r := connectRetry(-1, time.Second); r.Retries != 0 {
		t.Errorf("negative retries not clamped: %+v", r)
	}
	// Two retries with their doubling delays (1 s, then 2 s) must fit the budget.
	if r := connectRetry(2, time.Second); r.Retries != 2 || r.Delay != time.Second || r.Budget < 3*r.Delay ||
		r.Fast >= connectTimeout || r.Budget >= connectTimeout {
		t.Errorf("bad options %+v", r)
	}
}
