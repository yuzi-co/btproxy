package winrt

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

type fakeWatcher struct {
	closed atomic.Bool
	dead   atomic.Bool
}

func (w *fakeWatcher) close()      { w.closed.Store(true) }
func (w *fakeWatcher) alive() bool { return !w.dead.Load() }

// fakeRadio hands out fake watchers and records them.
type fakeRadio struct {
	mu   sync.Mutex
	ws   []*fakeWatcher
	fail int // starts that fail before one succeeds
}

func (r *fakeRadio) start(ble.ScanMode) (scanWatcher, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail > 0 {
		r.fail--
		return nil, errors.New("aborted at start")
	}
	w := &fakeWatcher{}
	r.ws = append(r.ws, w)
	return w, nil
}

func (r *fakeRadio) watchers() []*fakeWatcher {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*fakeWatcher(nil), r.ws...)
}

// testTimings are short enough for the tests to see the scanner retry and check, and long enough for a pause
// to outlast a test unless it shortens pauseLimit.
var testTimings = timings{scanRetry: 50 * time.Millisecond, scanCheck: 20 * time.Millisecond, pauseLimit: time.Minute}

// fakeAdapter is an Adapter on a fake radio, not yet scanning.
func fakeAdapter(pause bool, tm timings, newScanner func(ble.ScanMode) (scanWatcher, error)) *Adapter {
	a := &Adapter{log: logx.Discard(), tm: tm, conns: map[uint64]*conn{}, uncached: map[uint64]bool{},
		newScanner: newScanner}
	a.initScan(ble.ScanActive, pause)
	return a
}

// newTestAdapter is fakeAdapter with a scan running.
func newTestAdapter(t *testing.T, pause bool, tm timings) (*Adapter, *fakeRadio) {
	t.Helper()
	r := &fakeRadio{}
	a := fakeAdapter(pause, tm, r.start)
	stop, err := a.Scan(func(ble.Advert) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return a, r
}

func waitState(t *testing.T, a *Adapter, want ble.ScanState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for a.ScanStatus().State != want {
		if time.Now().After(deadline) {
			t.Fatalf("state %d, want %d", a.ScanStatus().State, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitClosed waits for the watcher to be closed, which happens on the side.
func waitClosed(t *testing.T, w *fakeWatcher) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !w.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("watcher not closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stopping the watcher must not wait for it to close, on any path: the API server stops the scan holding the
// lock that the advertisement callback, which closing may wait for, takes, and a connect pauses the scan
// while that callback may be running.
func TestStopDoesNotWaitForClose(t *testing.T) {
	for name, stopper := range map[string]func(a *Adapter, stop func()){
		"stop":   func(_ *Adapter, stop func()) { stop() },
		"pause":  func(a *Adapter, _ func()) { a.pauseScan() },
		"mode":   func(a *Adapter, _ func()) { a.SetScanMode(ble.ScanPassive) },
		"failed": func(a *Adapter, _ func()) { a.mu.Lock(); a.failedLocked(); a.mu.Unlock() },
	} {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			closed := make(chan struct{})
			first := true
			a := fakeAdapter(true, testTimings, func(ble.ScanMode) (scanWatcher, error) {
				if first { // the watcher a restart (mode, failed) replaces it with closes at once
					first = false
					return blockingWatcher{release, closed}, nil
				}
				return &fakeWatcher{}, nil
			})
			stop, err := a.Scan(func(ble.Advert) {})
			if err != nil {
				t.Fatal(err)
			}
			var released sync.Once
			t.Cleanup(stop)                                              // after the watcher is released, else a
			t.Cleanup(func() { released.Do(func() { close(release) }) }) // failed test hangs on the lock
			done := make(chan struct{})
			go func() { stopper(a, stop); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("waits for the watcher to close")
			}
			released.Do(func() { close(release) })
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("watcher not closed")
			}
		})
	}
}

// A scan whose watcher cannot start, e.g. with the radio off when Home Assistant subscribes, is not an error:
// it is retried like a watcher that stops later, and the status reports the failure meanwhile.
func TestScanStartFailureRetries(t *testing.T) {
	tm := testTimings
	tm.scanRetry = 20 * time.Millisecond
	r := &fakeRadio{fail: 2}
	a := fakeAdapter(false, tm, r.start)
	var states []ble.ScanState
	var mu sync.Mutex
	a.OnScanStatus(func(st ble.ScanStatus) {
		mu.Lock()
		states = append(states, st.State)
		mu.Unlock()
	})
	stop, err := a.Scan(func(ble.Advert) {})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if st := a.ScanStatus().State; st != ble.ScanFailed {
		t.Fatalf("state %d after a failed start, want %d", st, ble.ScanFailed)
	}
	waitState(t, a, ble.ScanRunning)
	if n := len(r.watchers()); n != 1 {
		t.Fatalf("%d watchers", n)
	}
	mu.Lock()
	seen := append([]ble.ScanState(nil), states...)
	mu.Unlock()
	failed := 0
	for _, st := range seen {
		if st == ble.ScanFailed {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("states %v: the two failed starts should be reported", seen)
	}
	stop()
	waitState(t, a, ble.ScanIdle)
	waitClosed(t, r.watchers()[0])
	if a.retry != nil || a.check != nil {
		t.Fatal("timers still armed after stop")
	}
}

type blockingWatcher struct{ release, closed chan struct{} }

func (w blockingWatcher) close()      { <-w.release; close(w.closed) }
func (w blockingWatcher) alive() bool { return true }

func TestPauseScan(t *testing.T) {
	a, r := newTestAdapter(t, true, testTimings)
	waitState(t, a, ble.ScanRunning)
	resume1 := a.pauseScan()
	waitClosed(t, r.watchers()[0]) // stopped for the connect
	waitState(t, a, ble.ScanIdle)
	resume2 := a.pauseScan() // overlapping connect
	a.SetScanMode(ble.ScanPassive)
	resume1()
	resume1() // a second call does nothing
	if n := len(r.watchers()); n != 1 {
		t.Fatalf("scanning resumed while a connect is still in progress (%d watchers)", n)
	}
	waitState(t, a, ble.ScanIdle)
	resume2()
	waitState(t, a, ble.ScanRunning)
	if st := a.ScanStatus(); st.Mode != ble.ScanPassive || len(r.watchers()) != 2 {
		t.Fatalf("after the pause: %+v, %d watchers", st, len(r.watchers()))
	}
}

func TestPauseScanOff(t *testing.T) {
	a, r := newTestAdapter(t, false, testTimings)
	a.pauseScan()()
	if r.watchers()[0].closed.Load() || a.ScanStatus().State != ble.ScanRunning {
		t.Fatal("scanning paused although pausing is off")
	}
}

func TestPauseLimit(t *testing.T) {
	tm := testTimings
	tm.pauseLimit = 50 * time.Millisecond
	a, r := newTestAdapter(t, true, tm)
	resume := a.pauseScan()
	waitState(t, a, ble.ScanIdle)
	waitState(t, a, ble.ScanRunning) // the hung connect no longer holds scanning
	resume()
	if n := len(r.watchers()); n != 2 {
		t.Fatalf("%d watchers", n)
	}
}

func TestResumeRetriesFailedStart(t *testing.T) {
	a, r := newTestAdapter(t, true, testTimings)
	resume := a.pauseScan()
	r.mu.Lock()
	r.fail = 1
	r.mu.Unlock()
	resume()
	waitState(t, a, ble.ScanFailed)
	waitState(t, a, ble.ScanRunning)
}

func TestScanWhilePaused(t *testing.T) {
	r := &fakeRadio{}
	a := fakeAdapter(true, testTimings, r.start)
	resume := a.pauseScan()
	stop, err := a.Scan(func(ble.Advert) {})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if a.ScanStatus().State != ble.ScanIdle || len(r.watchers()) != 0 {
		t.Fatal("watcher started during a pause")
	}
	resume()
	waitState(t, a, ble.ScanRunning)
}

func TestDeadWatcherRestarts(t *testing.T) {
	a, r := newTestAdapter(t, true, testTimings)
	r.watchers()[0].dead.Store(true) // aborted without a Stopped event
	deadline := time.Now().Add(2 * time.Second)
	for len(r.watchers()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("aborted watcher not replaced")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitState(t, a, ble.ScanRunning)
	waitClosed(t, r.watchers()[0]) // the aborted one
}
