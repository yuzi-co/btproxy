package main

import (
	"os"
	"path/filepath"
	"sync"
)

// logMaxSize is the size at which the log file is rotated; one older file (.1) is kept.
const logMaxSize = 1 << 20

// rotatingFile is an append-only log file that moves itself to path.1 when it would grow past max.
type rotatingFile struct {
	mu     sync.Mutex
	path   string
	max    int64
	f      *os.File
	size   int64
	onOpen func(*os.File) // called with each file opened, to point the process's stderr at it
}

func openRotating(path string, max int64, onOpen func(*os.File)) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	r := &rotatingFile{path: path, max: max, onOpen: onOpen}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	r.f, r.size = f, 0
	if st, err := f.Stat(); err == nil {
		r.size = st.Size()
	}
	if r.onOpen != nil {
		r.onOpen(f)
	}
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate moves the file to path.1, replacing the older one. If the rename fails (another program holds
// the file without sharing delete, e.g. the old proxy for a few seconds after a restart), logging goes on
// in the same file and the next attempt waits for another max bytes; the older file is kept meanwhile.
func (r *rotatingFile) rotate() {
	r.f.Close()
	renamed := os.Rename(r.path, r.path+".1") == nil // replaces path.1, so the older file is not removed first
	if err := r.open(); err != nil {
		// Reopening failed: drop lines until the next rotation tries again.
		r.f, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		r.size = 0
	} else if !renamed {
		r.size = 0 // the file is over max: count from here, not on every write
	}
}
