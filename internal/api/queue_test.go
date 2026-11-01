package api

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
)

// pipeListener hands the server connections made with net.Pipe, whose writes block until the other end
// reads: a peer that never reads is a client that has stalled completely.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error   { close(l.done); return nil }
func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// dial connects a new client and returns the client's end.
func (l *pipeListener) dial(t *testing.T) net.Conn {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	l.conns <- a
	return b
}

func clientCount(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStalledClient broadcasts to a client that reads everything and one that reads nothing. Broadcasts must
// not wait for the stalled client, which is closed once its queue overflows, and the other client must get
// every message, in order.
func TestStalledClient(t *testing.T) {
	l := newPipeListener()
	s := NewServer(Config{Name: "test"}, testAdapter{}, logx.Discard())
	go s.Serve(l)
	t.Cleanup(func() { l.Close() })
	l.dial(t) // never read
	good := l.dial(t)
	waitFor(t, "two clients", func() bool { return clientCount(s) == 2 })

	const n = 4 * outQueue
	got := make(chan uint32, n)
	go func() {
		r := bufio.NewReader(good)
		for {
			f, err := ReadFrame(r)
			if err != nil {
				close(got)
				return
			}
			got <- binary.BigEndian.Uint32(f.Payload)
		}
	}()
	var busy time.Duration
	for i := range uint32(n) {
		start := time.Now()
		s.broadcast(msgBTGATTNotifyDataResponse, binary.BigEndian.AppendUint32(nil, i))
		busy += time.Since(start)
		if i%64 == 63 { // let the reading client keep up, so only the stalled one overflows
			waitFor(t, "the reading client", func() bool { return len(got) == int(i+1) })
		}
	}
	if busy > time.Second {
		t.Errorf("broadcasts took %v", busy)
	}
	for i := range uint32(n) {
		if v := <-got; v != i {
			t.Fatalf("message %d carried %d", i, v)
		}
	}
	waitFor(t, "the stalled client to be closed", func() bool { return clientCount(s) == 1 })
}

// TestStalledClientAdverts fills a stalled client's queue with advertisements: they are dropped, and the
// client stays until something that cannot be dropped finds the queue full.
func TestStalledClientAdverts(t *testing.T) {
	conn := &stuckConn{entered: make(chan struct{}), closed: make(chan struct{})}
	c := newClient(&Server{}, conn, logx.Discard())
	defer c.closeQueue()
	c.send(msgPingRequest, nil)
	<-conn.entered // the writer holds that message and waits
	for range 4 * outQueue {
		c.sendDroppable(msgBTRawAdvertisementsResponse, []byte{1})
	}
	alive := func() bool {
		c.wmu.Lock()
		defer c.wmu.Unlock()
		return !c.dead
	}
	if n := len(c.out); !alive() || n != dropLimit {
		t.Fatalf("alive %v with %d queued, want alive with %d", alive(), n, dropLimit)
	}
	for range outQueue - dropLimit {
		c.send(msgBTGATTNotifyDataResponse, []byte{1})
	}
	if !alive() {
		t.Fatal("closed before the queue was full")
	}
	c.send(msgBTGATTNotifyDataResponse, []byte{1})
	if alive() {
		t.Fatal("overflowing client not closed")
	}
	select {
	case <-conn.closed:
	default:
		t.Fatal("connection still open")
	}
}

// stuckConn is a connection whose writes wait until it is closed.
type stuckConn struct {
	net.Conn
	entered chan struct{} // closed by the first write
	closed  chan struct{}
	once    sync.Once
	wonce   sync.Once
}

func (c *stuckConn) Write([]byte) (int, error) {
	c.wonce.Do(func() { close(c.entered) })
	<-c.closed
	return 0, net.ErrClosed
}

func (c *stuckConn) SetWriteDeadline(time.Time) error { return nil }
func (c *stuckConn) Close() error                     { c.once.Do(func() { close(c.closed) }); return nil }

// TestNoGoroutineLeak connects clients, one of them stalled until it overflows, and checks that every
// goroutine they started ends once they are gone.
func TestNoGoroutineLeak(t *testing.T) {
	l := newPipeListener()
	logs := &Logs{}
	log := logx.New(nil, logx.LevelInfo, logs)
	s := NewServer(Config{Name: "test", Logs: logs}, testAdapter{}, log)
	go s.Serve(l)
	defer l.Close()
	time.Sleep(10 * time.Millisecond)
	base := runtime.NumGoroutine()

	var e enc
	e.uint(1, logDebug)
	sub := AppendFrame(nil, msgSubscribeLogsRequest, e.b)
	for range 3 {
		c := l.dial(t)
		go io.Copy(io.Discard, c)
		c.Write(sub)
	}
	stalled := l.dial(t)
	stalled.Write(sub)
	waitFor(t, "four clients", func() bool { return clientCount(s) == 4 })
	for i := range 2 * outQueue {
		s.broadcast(msgBTGATTNotifyDataResponse, []byte{byte(i)})
	}
	waitFor(t, "the stalled client to be closed", func() bool { return clientCount(s) < 4 })
	s.mu.Lock()
	for c := range s.clients {
		c.c.Close()
	}
	s.mu.Unlock()
	waitFor(t, "no clients", func() bool { return clientCount(s) == 0 })
	waitFor(t, "the clients' goroutines to end", func() bool { return runtime.NumGoroutine() <= base })
}

// flakyListener fails its first accepts as a process out of file descriptors does.
type flakyListener struct {
	*pipeListener
	fails int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.fails > 0 {
		l.fails--
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return l.pipeListener.Accept()
}

// TestServeRetriesAccept checks that Serve outlives failing accepts and returns only once the listener is
// closed.
func TestServeRetriesAccept(t *testing.T) {
	l := &flakyListener{pipeListener: newPipeListener(), fails: 5}
	s := NewServer(Config{Name: "test"}, testAdapter{}, logx.Discard())
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	c := l.dial(t)
	c.Write(AppendFrame(nil, msgPingRequest, nil))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if f, err := ReadFrame(bufio.NewReader(c)); err != nil || f.Type != msgPingResponse {
		t.Fatalf("got %+v, %v", f, err)
	}
	l.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the listener was closed")
	}
}
