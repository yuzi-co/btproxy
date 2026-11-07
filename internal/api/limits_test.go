package api

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

// Connections over the limit are closed, and a slot that comes free can be used again.
func TestClientLimit(t *testing.T) {
	l := newPipeListener()
	s := NewServer(Config{Name: "test"}, testAdapter{}, logx.Discard())
	go s.Serve(l)
	t.Cleanup(func() { l.Close() })
	first := l.dial(t)
	for range maxClients - 1 {
		l.dial(t)
	}
	waitFor(t, "all clients", func() bool { return clientCount(s) == maxClients })

	over := l.dial(t)
	over.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := over.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("over the limit: want the connection closed, got %v", err)
	}
	first.Close()
	waitFor(t, "a free slot", func() bool { return clientCount(s) == maxClients-1 })
	l.dial(t)
	waitFor(t, "the slot used again", func() bool { return clientCount(s) == maxClients })
}

// A client that says nothing is dropped after helloTimeout, well before setupTimeout, so connections that never
// set up cannot hold the client slots for long; one that has sent Hello may stay silent.
func TestSetupTimeout(t *testing.T) {
	l := newPipeListener()
	s := NewServer(Config{Name: "test"}, testAdapter{}, logx.Discard())
	s.helloTimeout, s.setupTimeout = 100*time.Millisecond, 2*time.Second
	go s.Serve(l)
	t.Cleanup(func() { l.Close() })

	silent := l.dial(t)
	start := time.Now()
	silent.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := silent.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("silent client: want the connection closed, got %v", err)
	}
	if d := time.Since(start); d >= s.setupTimeout/2 {
		t.Fatalf("silent client dropped after %v, want about %v", d, s.helloTimeout)
	}

	hello := l.dial(t)
	if _, err := hello.Write(AppendFrame(nil, msgHelloRequest, nil)); err != nil {
		t.Fatal(err)
	}
	hello.SetReadDeadline(time.Now().Add(5 * time.Second))
	if f, err := ReadFrame(bufio.NewReader(hello)); err != nil || f.Type != msgHelloResponse {
		t.Fatalf("hello: %v %v", f, err)
	}
	time.Sleep(4 * s.helloTimeout)
	if n := clientCount(s); n != 1 {
		t.Fatalf("client that said hello: %d clients", n)
	}
}

// Writes over the queue's limits fail at once, and those already queued still wait their turn.
func TestGATTQueueLimit(t *testing.T) {
	for _, c := range []struct {
		name string
		size int
		fits int // writes that fit the queue, after the one the stuck worker holds
	}{
		{"count", 1, maxGATTQueue},
		{"bytes", 30 << 10, maxGATTQueueBytes / (30 << 10)}, // a frame holds at most 32 KB
	} {
		t.Run(c.name, func(t *testing.T) {
			conn := &testConn{stuck: make(chan struct{})}
			tc := startServer(t, testAdapter{connect: func(ble.Events) (ble.Conn, error) { return conn, nil }})
			t.Cleanup(func() { conn.Disconnect() })
			tc.connect()
			write := func(handle uint64) {
				var e enc
				e.uint(1, testAddr)
				e.uint(2, handle)
				e.bytes(4, bytes.Repeat([]byte{1}, c.size))
				tc.send(msgBTGATTWriteRequest, e.b)
			}
			for i := range c.fits + 1 {
				write(uint64(i + 1))
				if i == 0 { // let the worker take the first one off the queue
					time.Sleep(50 * time.Millisecond)
				}
			}
			const over = 0x7777
			write(over)
			if fs := tc.expect(msgBTGATTErrorResponse); fieldValue(fs, 2) != over {
				t.Fatalf("error for handle %d, want the write over the limit", fieldValue(fs, 2))
			}
		})
	}
}

func TestWorkerLimit(t *testing.T) {
	c := &client{log: logx.Discard()}
	release := make(chan struct{})
	for range maxClientWorkers {
		if !c.spawn(func() { <-release }) {
			t.Fatal("refused under the limit")
		}
	}
	if c.spawn(func() { t.Error("ran over the limit") }) {
		t.Fatal("accepted over the limit")
	}
	close(release)
	waitFor(t, "workers to finish", func() bool { return c.workers.Load() == 0 })
	if !c.spawn(func() {}) {
		t.Fatal("refused after the workers finished")
	}
}
