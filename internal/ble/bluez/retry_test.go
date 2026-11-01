//go:build linux

package bluez

import (
	"context"
	"fmt"
	"testing"

	"github.com/godbus/dbus/v5"
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

func TestConnectRetryOptions(t *testing.T) {
	if r := connectRetry(-1); r.Retries != 0 {
		t.Errorf("negative retries not clamped: %+v", r)
	}
	if r := connectRetry(2); r.Retries != 2 || r.Fast >= connectTimeout || r.Budget >= connectTimeout {
		t.Errorf("bad options %+v", r)
	}
}
