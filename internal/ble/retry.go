package ble

import "time"

// ConnectRetry retries a connect attempt that failed fast with an error the radio may not repeat, such as
// HCI reason 0x3e (connection failed to be established): the link failed in its first connection events,
// and a second try a moment later often gets through. Slow failures (timeouts) are not retried, so retrying
// stays within the time a client gives a connect.
type ConnectRetry struct {
	Retries int           // attempts after the first; 0 disables retrying
	Delay   time.Duration // wait before each retry
	Fast    time.Duration // only failures that came within this long of the attempt starting are retried
	Budget  time.Duration // no retry starts later than this after the first attempt
}

// Do calls attempt until it succeeds, fails with an error retryable does not accept, fails slower than
// r.Fast, runs out of retries or budget, or withdrawn (may be nil) returns true after the delay. onRetry (may
// be nil) is called right before each retry with its number, from 1, and the error that caused it. Do returns the number of
// retries made and the last attempt's error.
func (r ConnectRetry) Do(attempt func() error, retryable func(error) bool, withdrawn func() bool,
	onRetry func(n int, err error)) (retries int, err error) {
	first := time.Now()
	for {
		start := time.Now()
		err = attempt()
		if err == nil || retries >= r.Retries || !retryable(err) || time.Since(start) > r.Fast ||
			time.Since(first)+r.Delay > r.Budget {
			return retries, err
		}
		time.Sleep(r.Delay)
		if withdrawn != nil && withdrawn() {
			return retries, err
		}
		retries++
		if onRetry != nil {
			onRetry(retries, err)
		}
	}
}
