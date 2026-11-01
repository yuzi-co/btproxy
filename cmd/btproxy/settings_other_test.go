//go:build !windows

package main

import (
	"bufio"
	"os"
	"os/exec"
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
			cmd := exec.Command(os.Args[0], "-backend", "fake", "-mdns=false", "-name", "proxy",
				"-mac", "02:00:00:00:00:01", "-listen", c.host+":"+freePort(t))
			cmd.Env = append(os.Environ(), "BTPROXY_RUN_MAIN=1", "BTPROXY_ENCRYPTION_KEY="+c.key)
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
			got := false
			for s := bufio.NewScanner(stderr); s.Scan(); {
				got = got || strings.Contains(s.Text(), "API is not encrypted")
				if strings.Contains(s.Text(), "btproxy started") {
					break
				}
			}
			if got != c.want {
				t.Errorf("warning: got %v, want %v", got, c.want)
			}
		})
	}
}
