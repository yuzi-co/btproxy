package ble

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
)

// testPause is a gate whose Kick counts its calls and, if stop is set, releases the waiters as a supervisor
// that stopped scanning would.
func testPause(pause bool, wait, limit time.Duration, stop bool) (*ScanPause, *atomic.Int32, *bytes.Buffer) {
	var kicks atomic.Int32
	var logs bytes.Buffer
	p := &ScanPause{Pause: pause, Wait: wait, Limit: limit, Log: logx.New(&logs, logx.LevelWarn, nil)}
	p.Kick = func() {
		kicks.Add(1)
		if stop {
			_, release := p.Take()
			release()
		}
	}
	return p, &kicks, &logs
}

// TestScanPauseCounts: connects are counted whether or not they pause scanning, ending one counts it out
// once however often end is called, and scanning is to be paused only with the pause on.
func TestScanPauseCounts(t *testing.T) {
	for _, pause := range []bool{false, true} {
		p, kicks, _ := testPause(pause, 0, 0, false)
		end := p.Begin()
		if !p.Connecting() || p.Paused() != pause || kicks.Load() != 1 {
			t.Fatalf("pause %v: connecting %v paused %v kicks %d after Begin", pause, p.Connecting(), p.Paused(), kicks.Load())
		}
		if paused, release := p.Take(); paused != pause {
			t.Fatalf("pause %v: Take says paused %v", pause, paused)
		} else {
			release()
		}
		end()
		end()
		if p.Connecting() || p.Paused() || kicks.Load() != 2 {
			t.Fatalf("pause %v: connecting %v paused %v kicks %d after end", pause, p.Connecting(), p.Paused(), kicks.Load())
		}
	}
}

// TestScanPauseOverlap: overlapping connects share the pause, which ends with the last of them.
func TestScanPauseOverlap(t *testing.T) {
	p, _, _ := testPause(true, 0, 0, false)
	endA := p.Begin()
	endB := p.Begin()
	endA()
	if !p.Paused() {
		t.Fatal("pause ended with a connect still in progress")
	}
	endB()
	if p.Paused() {
		t.Fatal("pause did not end with the last connect")
	}
}

// TestScanPauseWaitsForTheSupervisor: Begin returns once the supervisor took the waiter and released it.
func TestScanPauseWaitsForTheSupervisor(t *testing.T) {
	p, _, logs := testPause(true, time.Second, 0, true)
	start := time.Now()
	p.Begin()()
	if took := time.Since(start); took > 500*time.Millisecond || logs.Len() != 0 {
		t.Fatalf("Begin took %v, logged %q", took, logs.String())
	}
}

// TestScanPauseGoesAheadLate: a supervisor that does not stop scanning in time holds the connect for Wait only.
func TestScanPauseGoesAheadLate(t *testing.T) {
	p, _, logs := testPause(true, 30*time.Millisecond, 0, false)
	start := time.Now()
	end := p.Begin()
	defer end()
	if took := time.Since(start); took < 30*time.Millisecond || took > time.Second ||
		!strings.Contains(logs.String(), "scanning not paused in time") {
		t.Fatalf("Begin took %v, logged %q", took, logs.String())
	}
	if _, release := p.Take(); release == nil {
		t.Fatal("no release")
	}
}

// TestScanPauseLimit: a connect that hangs holds the gate for Limit only, and ending it later changes nothing.
func TestScanPauseLimit(t *testing.T) {
	for _, pause := range []bool{true, false} {
		p, kicks, logs := testPause(pause, 0, 30*time.Millisecond, false)
		end := p.Begin()
		deadline := time.Now().Add(2 * time.Second)
		for p.Connecting() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if p.Connecting() || kicks.Load() != 2 || !strings.Contains(logs.String(), "connect still in progress") {
			t.Fatalf("pause %v: connecting %v kicks %d logged %q after the limit", pause, p.Connecting(), kicks.Load(), logs.String())
		}
		end()
		if p.Connecting() || kicks.Load() != 2 {
			t.Fatalf("pause %v: connecting %v kicks %d after a late end", pause, p.Connecting(), kicks.Load())
		}
	}
}
