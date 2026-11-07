package ble

import (
	"errors"
	"fmt"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
)

// ConnectBudget is how long after the first attempt a connect retry may still start. A client gives a connect
// about 20 s, so 10 s fits the doubling delays of the default retries and leaves the last attempt as much again.
const ConnectBudget = 10 * time.Second

// ConnectRetry retries a connect attempt that failed with an error the radio may not repeat, such as HCI
// reason 0x3e (connection failed to be established): the link failed in its first connection events, and a
// second try a moment later often gets through. Only failures that came fast are retried, as a slow one is a
// timeout, so retrying stays within the time a client gives a connect. A link that came up and dropped before
// the connection was handed over (ErrLinkLost) is retried however long it took: the attempt lasted as long as
// the link, which says nothing about the next one. The delay doubles with each retry (Delay, 2*Delay,
// 4*Delay...), so a device that does not take a burst of attempts gets a longer breather each time.
type ConnectRetry struct {
	Retries int           // attempts after the first; 0 disables retrying
	Delay   time.Duration // wait before the first retry; each later one waits twice the previous
	Fast    time.Duration // only failures that came within this long of the attempt starting are retried
	Budget  time.Duration // no retry starts later than this after the first attempt

	sleep func(time.Duration) // nil is time.Sleep; the tests record the delays instead
}

// Retry is the ConnectRetry for the options, with the budget every backend shares. fast is the backend's
// cut-off for a failure to count as the radio's (see ConnectRetry.Fast).
func (o ConnectOptions) Retry(fast time.Duration) ConnectRetry {
	return ConnectRetry{Retries: max(o.ConnectRetries, 0), Delay: o.ConnectRetryDelay, Fast: fast, Budget: ConnectBudget}
}

// Do calls attempt until it succeeds, fails slower than r.Fast without the link having been lost, runs out of
// retries or budget, fails with an error retryable does not accept, or withdrawn (may be nil) returns true
// after the delay. retryable is asked last, only about a failure that would be retried, so it may set the
// next attempt up. onRetry (may be nil) is called right before each retry with its number, from 1, and the
// error that caused it. Do returns the number of retries made and the last attempt's error.
func (r ConnectRetry) Do(attempt func() error, retryable func(error) bool, withdrawn func() bool,
	onRetry func(n int, err error)) (retries int, err error) {
	sleep := r.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	first := time.Now()
	for {
		start := time.Now()
		err = attempt()
		// The delay before retry n (from 1) is Delay << (n-1).
		delay := r.Delay << retries
		if err == nil || retries >= r.Retries || time.Since(start) > r.Fast && !errors.Is(err, ErrLinkLost) ||
			time.Since(first)+delay > r.Budget || !retryable(err) {
			return retries, err
		}
		sleep(delay)
		if withdrawn != nil && withdrawn() {
			return retries, err
		}
		retries++
		if onRetry != nil {
			onRetry(retries, err)
		}
	}
}

// Run is Do for a backend's Connect: log gets a line per retry and one when the connect succeeded after
// retries, and the error of a failed connect says how many retries it took ("connect, 2 retries: ...").
func (r ConnectRetry) Run(log *logx.Logger, attempt func() error, retryable func(error) bool, withdrawn func() bool) error {
	retries, err := r.Do(attempt, retryable, withdrawn, func(n int, err error) {
		log.Debug("connect failed, retrying", "retry", n, "err", err)
	})
	switch {
	case err != nil && retries > 0:
		return fmt.Errorf("connect, %d retries: %w", retries, err)
	case err != nil:
		return fmt.Errorf("connect: %w", err)
	case retries > 0:
		log.Info("connected after retries", "retries", retries)
	}
	return nil
}
