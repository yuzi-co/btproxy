package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain runs the proxy itself when a test starts the test binary as a child (startProxy).
func TestMain(m *testing.M) {
	if os.Getenv("BTPROXY_RUN_MAIN") == "1" {
		os.Args = append([]string{"btproxy"}, os.Args[1:]...)
		main()
		return
	}
	os.Exit(m.Run())
}

// proxyProcess is a proxy running as a child process of the test binary.
type proxyProcess struct {
	cmd *exec.Cmd
	mu  sync.Mutex
	log []string // its stderr so far
}

// startProxy starts the test binary as the proxy with testChildArgs, then args (so args win), and env on top
// of the test's environment. The child is killed when the test ends. Its log is collected for waitLog, and
// copied to the test's stderr with -v.
func startProxy(t *testing.T, env []string, args ...string) *proxyProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], append(append([]string{}, testChildArgs...), args...)...)
	cmd.Env = append(append(os.Environ(), "BTPROXY_RUN_MAIN=1"), env...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	p := &proxyProcess{cmd: cmd}
	go func() {
		for s := bufio.NewScanner(stderr); s.Scan(); {
			if testing.Verbose() {
				fmt.Fprintln(os.Stderr, s.Text())
			}
			p.mu.Lock()
			p.log = append(p.log, s.Text())
			p.mu.Unlock()
		}
	}()
	return p
}

// waitLog waits for a log line containing want, and returns the log up to and including it.
func (p *proxyProcess) waitLog(t *testing.T, want string) []string {
	t.Helper()
	var got []string
	waitFor(t, "log line "+want, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		for i, l := range p.log {
			if strings.Contains(l, want) {
				got = append([]string{}, p.log[:i+1]...)
				return true
			}
		}
		return false
	})
	return got
}

// waitFor polls ok for up to 20 s: a restart on Windows takes a while.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for", what)
		}
	}
}
