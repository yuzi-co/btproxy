package winrt

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRadioFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("service discovery: %w", errUnreachable), true},
		{fmt.Errorf("service discovery: characteristics of x: %w", errUnreachable), true},
		{errLinkLost, true},
		{fmt.Errorf("service discovery: %w", hresultError(hrGenFailure)), true},
		{hresultError(hrDeviceNotAvailable), true},
		{hresultError(0x80070005), false}, // E_ACCESSDENIED
		{hresultError(0x80070490), false}, // ERROR_NOT_FOUND
		{hresultError(0x800704c7), false}, // ERROR_CANCELLED
		{hresultError(0x80650003), false}, // ATT error
		{errAccessDenied, false},
		{errProtocol, false},
		{errTimeout, false},
		{errors.New("device not found"), false},
		{errors.New("no connection in time"), false},
		{errors.New("async operation ended with status 2"), false}, // cancelled
	} {
		if got := radioFailure(tc.err); got != tc.want {
			t.Errorf("radioFailure(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestConnectRetryDefaults(t *testing.T) {
	r := connectRetry(-1, time.Second)
	if r.Retries != 0 {
		t.Errorf("negative retries: %d", r.Retries)
	}
	r = connectRetry(2, time.Second)
	// Two retries with their doubling delays (1 s, then 2 s) must fit the budget, which must leave a client's
	// 30 s connect most of its time.
	if r.Delay != time.Second || r.Budget < 3*r.Delay || r.Budget > 10*time.Second || r.Fast > r.Budget {
		t.Errorf("odd timings: %+v", r)
	}
}
