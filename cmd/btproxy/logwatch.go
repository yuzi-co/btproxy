package main

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
)

// problemWatch passes log lines through and remembers the latest warning or error, for the tray and the
// settings page.
type problemWatch struct {
	w    io.Writer // nil drops the lines
	mu   sync.Mutex
	last string
	at   time.Time
}

func (p *problemWatch) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte(" level=ERROR ")) || bytes.Contains(b, []byte(" level=WARN ")) {
		line := strings.TrimSpace(string(b))
		if i := strings.Index(line, " level="); i >= 0 {
			line = line[i+1:] // the time is kept separately
		}
		if len(line) > 300 {
			line = line[:300] + "..."
		}
		p.mu.Lock()
		p.last, p.at = line, time.Now()
		p.mu.Unlock()
	}
	if p.w == nil {
		return len(b), nil
	}
	return p.w.Write(b)
}

// lastProblem returns the latest warning or error line and when it was logged; "" if none.
func (p *problemWatch) lastProblem() (string, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last, p.at
}
