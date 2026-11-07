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

// testAdapter is an Adapter scanning on a fake radio, not yet scanning.
func testAdapter(pause bool, tm timings, newScanner func(ble.ScanMode) (scanWatcher, error)) *Adapter {
	a := &Adapter{log: logx.Discard(), tm: tm, conns: map[uint64]*conn{}, uncached: map[uint64]bool{},
		newScanner: newScanner}
	a.initScan(ble.ScanActive, pause)
	return a
}

// newTestAdapter is testAdapter with a scan running.
func newTestAdapter(t *testing.T, pause bool, tm timings) (*Adapter, *fakeRadio) {
	t.Helper()
	r := &fakeRadio{}
	a := testAdapter(pause, tm, r.start)
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

// Stopping the scan must not wait for the watcher to close: the API server stops it holding the lock that the
// advertisement callback, which closing may wait for, takes.
func TestStopDoesNotWaitForClose(t *testing.T) {
	release := make(chan struct{})
	closed := make(chan struct{})
	a := testAdapter(false, testTimings, func(ble.ScanMode) (scanWatcher, error) { return blockingWatcher{release, closed}, nil })
	stop, err := a.Scan(func(ble.Advert) {})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop waits for the watcher to close")
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher not closed")
	}
}

type blockingWatcher struct{ release, closed chan struct{} }

func (w blockingWatcher) close()      { <-w.release; close(w.closed) }
func (w blockingWatcher) alive() bool { return true }

func TestPauseScan(t *testing.T) {
	a, r := newTestAdapter(t, true, testTimings)
	waitState(t, a, ble.ScanRunning)
	resume1 := a.pauseScan()
	if !r.watchers()[0].closed.Load() {
		t.Fatal("watcher not stopped for the connect")
	}
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
	a := testAdapter(true, testTimings, r.start)
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
	if !r.watchers()[0].closed.Load() {
		t.Fatal("aborted watcher not closed")
	}
}
