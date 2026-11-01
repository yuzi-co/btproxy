package winrt

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"btproxy/internal/ble"
	"btproxy/internal/logx"
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

func newTestAdapter(t *testing.T, pause bool) (*Adapter, *fakeRadio) {
	t.Helper()
	r := &fakeRadio{}
	a := &Adapter{log: logx.Discard(), pauseOnConnect: pause, mode: ble.ScanActive, conns: map[uint64]*conn{},
		uncached: map[uint64]bool{}, newScanner: r.start}
	stop, err := a.Scan(func(ble.Advert) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return a, r
}

func shorten(t *testing.T, v *time.Duration, d time.Duration) {
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
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

func TestPauseScan(t *testing.T) {
	a, r := newTestAdapter(t, true)
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
	a, r := newTestAdapter(t, false)
	a.pauseScan()()
	if r.watchers()[0].closed.Load() || a.ScanStatus().State != ble.ScanRunning {
		t.Fatal("scanning paused although pausing is off")
	}
}

func TestPauseLimit(t *testing.T) {
	shorten(t, &pauseLimit, 50*time.Millisecond)
	a, r := newTestAdapter(t, true)
	resume := a.pauseScan()
	waitState(t, a, ble.ScanIdle)
	waitState(t, a, ble.ScanRunning) // the hung connect no longer holds scanning
	resume()
	if n := len(r.watchers()); n != 2 {
		t.Fatalf("%d watchers", n)
	}
}

func TestResumeRetriesFailedStart(t *testing.T) {
	shorten(t, &scanRetry, 50*time.Millisecond)
	a, r := newTestAdapter(t, true)
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
	a := &Adapter{log: logx.Discard(), pauseOnConnect: true, mode: ble.ScanActive, conns: map[uint64]*conn{},
		uncached: map[uint64]bool{}, newScanner: r.start}
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
	shorten(t, &scanCheck, 20*time.Millisecond)
	shorten(t, &scanRetry, 20*time.Millisecond)
	a, r := newTestAdapter(t, true)
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
