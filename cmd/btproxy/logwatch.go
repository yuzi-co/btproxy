package main

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
)

// notEncryptedWarning is logged at every start of an API without a key. It reports a choice rather than a
// fault, so problemWatch leaves it out.
const notEncryptedWarning = "API is not encrypted: any host that can reach it can use the proxy and its bonded devices; " +
	"see -encryption-key-file"

// bluezDeprecatedWarning is logged at every start on the bluez backend, and left out for the same reason.
const bluezDeprecatedWarning = "the bluez backend is deprecated and will be removed: see Backends in the README to move to the kernel backend"

// problemWatch passes log lines through and remembers the latest warning or error, for the tray and the
// settings page.
type problemWatch struct {
	w    io.Writer // nil drops the lines
	mu   sync.Mutex
	last string
	at   time.Time
}

func (p *problemWatch) Write(b []byte) (int, error) {
	problem := bytes.Contains(b, []byte(" level=ERROR ")) || bytes.Contains(b, []byte(" level=WARN "))
	if problem && !bytes.Contains(b, []byte(notEncryptedWarning)) && !bytes.Contains(b, []byte(bluezDeprecatedWarning)) {
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
