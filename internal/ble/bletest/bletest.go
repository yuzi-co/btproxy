// Package bletest holds helpers the backends' tests share.
package bletest

import (
	"testing"
	"time"
)

// WaitFor polls cond until it holds, and fails the test if it does not within five seconds. what names the
// condition in the failure.
func WaitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
