package ble

import (
	"errors"
	"slices"
	"testing"
	"time"
)

var (
	errRadio = errors.New("le-connection-abort-by-local")
	errOther = errors.New("not found")
)

func isRadio(err error) bool { return errors.Is(err, errRadio) }

// attempts returns an attempt func that returns errs in turn, then nil, and counts its calls.
func attempts(n *int, errs ...error) func() error {
	return func() error {
		*n++
		if *n <= len(errs) {
			return errs[*n-1]
		}
		return nil
	}
}

func testRetry(retries int) ConnectRetry {
	return ConnectRetry{Retries: retries, Delay: time.Millisecond, Fast: time.Second, Budget: time.Second}
}

func TestConnectRetry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		retries     int
		errs        []error
		wantCalls   int
		wantRetries int
		wantErr     error
	}{
		{"success", 2, nil, 1, 0, nil},
		{"radio once", 2, []error{errRadio}, 2, 1, nil},
		{"radio twice", 2, []error{errRadio, errRadio}, 3, 2, nil},
		{"radio too often", 2, []error{errRadio, errRadio, errRadio}, 3, 2, errRadio},
		{"disabled", 0, []error{errRadio}, 1, 0, errRadio},
		{"other error", 2, []error{errOther}, 1, 0, errOther},
		{"radio then other", 2, []error{errRadio, errOther}, 2, 1, errOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, notified int
			retries, err := testRetry(tc.retries).Do(attempts(&calls, tc.errs...), isRadio, nil,
				func(n int, err error) {
					notified++
					if n != notified || !isRadio(err) {
						t.Errorf("onRetry(%d, %v), want retry %d after a radio failure", n, err, notified)
					}
				})
			if calls != tc.wantCalls || retries != tc.wantRetries || notified != retries || err != tc.wantErr {
				t.Errorf("calls %d retries %d notified %d err %v, want calls %d retries %d err %v",
					calls, retries, notified, err, tc.wantCalls, tc.wantRetries, tc.wantErr)
			}
		})
	}
}

// TestConnectRetrySlowFailure does not retry a failure that took longer than Fast: a timeout, not the radio.
func TestConnectRetrySlowFailure(t *testing.T) {
	r := ConnectRetry{Retries: 2, Delay: time.Millisecond, Fast: 10 * time.Millisecond, Budget: time.Second}
	calls := 0
	_, err := r.Do(func() error {
		calls++
		time.Sleep(20 * time.Millisecond)
		return errRadio
	}, isRadio, nil, nil)
	if calls != 1 || err != errRadio {
		t.Errorf("calls %d err %v, want 1 call and the error", calls, err)
	}
}

// TestConnectRetryBudget starts no retry that would begin after Budget.
func TestConnectRetryBudget(t *testing.T) {
	r := ConnectRetry{Retries: 10, Delay: 10 * time.Millisecond, Fast: time.Second, Budget: 25 * time.Millisecond}
	calls := 0
	retries, err := r.Do(func() error { calls++; return errRadio }, isRadio, nil, nil)
	if err != errRadio || calls != retries+1 || retries < 1 || retries > 2 {
		t.Errorf("calls %d retries %d err %v, want 1-2 retries within the budget", calls, retries, err)
	}
}

// TestConnectRetryWithdrawn stops retrying once the connect is withdrawn.
func TestConnectRetryWithdrawn(t *testing.T) {
	calls := 0
	retries, err := testRetry(5).Do(func() error { calls++; return errRadio }, isRadio,
		func() bool { return calls >= 2 }, nil)
	if calls != 2 || retries != 1 || err != errRadio {
		t.Errorf("calls %d retries %d err %v, want 2 calls, 1 retry and the error", calls, retries, err)
	}
}

// recordSleeps replaces sleep for the test and returns where the slept durations are collected.
func recordSleeps(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	saved := sleep
	sleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { sleep = saved })
	return &slept
}

// TestConnectRetryBackoff doubles the delay before each retry: Delay, 2*Delay, 4*Delay.
func TestConnectRetryBackoff(t *testing.T) {
	slept := recordSleeps(t)
	r := ConnectRetry{Retries: 3, Delay: 10 * time.Millisecond, Fast: time.Second, Budget: time.Second}
	calls := 0
	retries, err := r.Do(func() error { calls++; return errRadio }, isRadio, nil, nil)
	if calls != 4 || retries != 3 || err != errRadio {
		t.Fatalf("calls %d retries %d err %v, want 4 calls, 3 retries and the error", calls, retries, err)
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	if !slices.Equal(*slept, want) {
		t.Errorf("slept %v, want %v", *slept, want)
	}
}

// TestConnectRetryBackoffBudget stops at the retry whose doubled delay would start it after Budget, even though
// the base delay would still fit.
func TestConnectRetryBackoffBudget(t *testing.T) {
	slept := recordSleeps(t)
	r := ConnectRetry{Retries: 10, Delay: 10 * time.Millisecond, Fast: time.Second, Budget: 30 * time.Millisecond}
	calls := 0
	retries, err := r.Do(func() error { calls++; return errRadio }, isRadio, nil, nil)
	// Nothing really sleeps, so hardly any time passes: the third retry's 40 ms alone overruns the 30 ms budget,
	// while the base 10 ms would have fit.
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	if calls != 3 || retries != 2 || err != errRadio || !slices.Equal(*slept, want) {
		t.Errorf("calls %d retries %d err %v slept %v, want 3 calls, 2 retries, the error and %v",
			calls, retries, err, *slept, want)
	}
}

// TestConnectRetryDisabledSleepsNever makes one attempt and never sleeps with Retries 0.
func TestConnectRetryDisabledSleepsNever(t *testing.T) {
	slept := recordSleeps(t)
	r := ConnectRetry{Retries: 0, Delay: 10 * time.Millisecond, Fast: time.Second, Budget: time.Second}
	calls := 0
	retries, err := r.Do(func() error { calls++; return errRadio }, isRadio, nil, nil)
	if calls != 1 || retries != 0 || err != errRadio || len(*slept) != 0 {
		t.Errorf("calls %d retries %d err %v slept %v, want 1 call, no retry, the error and no sleep",
			calls, retries, err, *slept)
	}
}
