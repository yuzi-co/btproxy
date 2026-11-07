package ble

import (
	"sync"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
)

// ScanPause gates scanning around connects. Every backend counts the connects in progress: the kernel refuses
// to start discovery during one, and bluez holds its recovery steps off, since each would end the connect.
// With ConnectOptions.PauseOnConnect the count also pauses scanning, as on an ESP32: a controller that scans
// with a full duty cycle right when a link is being made can miss its first connection events, and the link
// fails with HCI reason 0x3e (connection failed to be established). Connecting does not need scanning, as
// every stack scans for the device it connects to by itself. Scanning stops when the first connect begins and
// resumes when the last one ends, or Limit after a connect began, should it hang beyond its own timeouts.
//
// The backend does the stopping and resuming in Kick, from the gate's state: Paused (or Take, for a backend
// whose supervisor stops scanning on its own goroutine and must then release the connects waiting for that).
type ScanPause struct {
	// Pause is ConnectOptions.PauseOnConnect; without it the gate only counts connects.
	Pause bool
	// Wait is how long Begin waits for the supervisor to report scanning stopped, through Take, before the
	// connect goes ahead anyway; 0 does not wait, for a backend that stops scanning in Kick itself.
	Wait time.Duration
	// Limit bounds how long one connect holds the gate; 0 does not bound it.
	Limit time.Duration
	// Kick tells the backend that the count changed. It runs without the gate's lock, from Begin, End and the
	// limit timer.
	Kick func()
	// Log receives the warnings; nil discards them.
	Log *logx.Logger

	mu         sync.Mutex
	connecting int
	waiters    []chan struct{}
}

// Begin counts a connect in and, with the pause on and a Wait, returns once the supervisor has stopped
// scanning, or after Wait. It returns the function that counts the connect out, which may be called more
// than once.
func (p *ScanPause) Begin() (end func()) {
	var stopped chan struct{}
	p.mu.Lock()
	p.connecting++
	if p.Pause && p.Wait > 0 {
		stopped = make(chan struct{})
		p.waiters = append(p.waiters, stopped)
	}
	p.mu.Unlock()
	p.Kick()

	var once sync.Once
	finish := func(late bool) {
		once.Do(func() {
			if late && p.Pause {
				p.warn("connect still in progress, resuming scanning", "after", p.Limit)
			} else if late {
				p.warn("connect still in progress, no longer counting it", "after", p.Limit)
			}
			p.mu.Lock()
			p.connecting--
			p.mu.Unlock()
			p.Kick()
		})
	}
	end = func() { finish(false) }
	if p.Limit > 0 {
		limit := time.AfterFunc(p.Limit, func() { finish(true) })
		end = func() {
			limit.Stop()
			finish(false)
		}
	}
	if stopped == nil {
		return end
	}
	t := time.NewTimer(p.Wait)
	defer t.Stop()
	select {
	case <-stopped:
	case <-t.C:
		p.warn("scanning not paused in time, connecting anyway", "after", p.Wait)
	}
	return end
}

// Connecting tells whether a connect is in progress.
func (p *ScanPause) Connecting() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connecting > 0
}

// Paused tells whether scanning is to be paused: the pause is on and a connect is in progress.
func (p *ScanPause) Paused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Pause && p.connecting > 0
}

// Take is Paused for a supervisor, which also takes over the connects waiting for scanning to stop, to
// release once it has acted on the answer, whether or not it could stop scanning.
func (p *ScanPause) Take() (paused bool, release func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	waiters := p.waiters
	p.waiters = nil
	return p.Pause && p.connecting > 0, func() {
		for _, w := range waiters {
			close(w)
		}
	}
}

func (p *ScanPause) warn(msg string, kv ...any) {
	if p.Log != nil {
		p.Log.Warn(msg, kv...)
	}
}
