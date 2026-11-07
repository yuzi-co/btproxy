//go:build !windows

package main

import (
	"strings"
	"testing"
)

func autostartFormValue() string { return "" }

// testChildArgs go to every proxy a test starts.
var testChildArgs []string

// The proxy warns at start when its API is plaintext and reachable from other hosts. Not run on Windows, where
// listening on all interfaces would bring up a firewall prompt.
func TestPlaintextWarning(t *testing.T) {
	const key = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	for _, c := range []struct {
		name, host, key string
		want            bool
	}{
		{"all interfaces, no key", "", "", true},
		{"loopback, no key", "127.0.0.1", "", false},
		{"all interfaces, key", "", key, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := startProxy(t, []string{"BTPROXY_ENCRYPTION_KEY=" + c.key}, "-backend", "fake", "-mdns=false",
				"-name", "proxy", "-mac", "02:00:00:00:00:01", "-listen", c.host+":"+freePort(t))
			got := false
			for _, l := range p.waitLog(t, "btproxy started") {
				got = got || strings.Contains(l, "API is not encrypted")
			}
			if got != c.want {
				t.Errorf("warning: got %v, want %v", got, c.want)
			}
		})
	}
}
