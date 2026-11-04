package main

import (
	"errors"
	"testing"
)

func TestChooseAuto(t *testing.T) {
	noMonitor := errors.New("operation not permitted")
	for _, tc := range []struct {
		name      string
		running   bool
		err       error
		haveBluez bool
		want      string
	}{
		{"nothing else on the controller", false, nil, true, "kernel"},
		{"bluetoothd running", true, nil, true, "bluez"},
		// The kernel backend cannot work without the monitor, and BlueZ may well be there.
		{"cannot tell", false, noMonitor, true, "bluez"},
		// A build without BlueZ has one choice, and its error says what is wrong.
		{"no BlueZ in this build", false, nil, false, "kernel"},
		{"no BlueZ in this build, bluetoothd running", true, nil, false, "kernel"},
		{"no BlueZ in this build, cannot tell", false, noMonitor, false, "kernel"},
	} {
		if got, why := chooseAuto(tc.running, tc.err, tc.haveBluez); got != tc.want || why == "" {
			t.Errorf("%s: %q (%q), want %q", tc.name, got, why, tc.want)
		}
	}
}
