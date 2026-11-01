package ui

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
)

func parse(s string) (*Request, error) {
	return readRequest(bufio.NewReaderSize(strings.NewReader(s), 4096))
}

func TestReadRequest(t *testing.T) {
	r, err := parse("GET /status.json?a=1&b=x%20y HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nX-A: 1\r\nx-a: 2\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "GET" || r.Path != "/status.json" || r.Query.Get("b") != "x y" || r.Header["host"] != "127.0.0.1:8080" ||
		r.Header["x-a"] != "1, 2" {
		t.Fatalf("%+v", r)
	}
	r, err = parse("POST /save HTTP/1.0\nContent-Length: 5\n\nhello trailing")
	if err != nil || string(r.Body) != "hello" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestReadRequestRejects(t *testing.T) {
	long := strings.Repeat("a", 5000)
	many := strings.Repeat("X-A: "+strings.Repeat("b", 1000)+"\r\n", 9)
	for _, c := range []struct {
		name, req string
		status    int
	}{
		{"garbage", "hello\r\n\r\n", 400},
		{"no slash", "GET status HTTP/1.1\r\n\r\n", 400},
		{"extra space", "GET  / HTTP/1.1\r\n\r\n", 400},
		{"version", "GET / HTTP/2.0\r\n\r\n", 505},
		{"long line", "GET /" + long + " HTTP/1.1\r\n\r\n", 431},
		{"header total", "GET / HTTP/1.1\r\n" + many + "\r\n", 431},
		{"folded", "GET / HTTP/1.1\r\nA: b\r\n c\r\n\r\n", 400},
		{"no colon", "GET / HTTP/1.1\r\nAbc\r\n\r\n", 400},
		{"space in name", "GET / HTTP/1.1\r\nA b: c\r\n\r\n", 400},
		{"chunked", "POST /save HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", 501},
		{"no length", "POST /save HTTP/1.1\r\n\r\n", 411},
		{"bad length", "POST /save HTTP/1.1\r\nContent-Length: -1\r\n\r\n", 400},
		{"plus length", "POST /save HTTP/1.1\r\nContent-Length: +1\r\n\r\nx", 400},
		{"two lengths", "POST /save HTTP/1.1\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\nxx", 400},
		{"big body", "POST /save HTTP/1.1\r\nContent-Length: 16385\r\n\r\n", 413},
		{"bad query", "GET /?a=%zz HTTP/1.1\r\n\r\n", 400},
	} {
		_, err := parse(c.req)
		var he *httpError
		if !errors.As(err, &he) || he.status != c.status {
			t.Errorf("%s: %v, want status %d", c.name, err, c.status)
		}
	}
	// A body shorter than its Content-Length is a client that went away: no answer.
	if _, err := parse("POST /save HTTP/1.1\r\nContent-Length: 10\r\n\r\nabc"); err == nil || errors.As(err, new(*httpError)) {
		t.Errorf("short body: %v", err)
	}
}

func startServer(t *testing.T, handle func(*Request) *Response) string {
	t.Helper()
	return startServerLimits(t, handle, defaultLimits)
}

func startServerLimits(t *testing.T, handle func(*Request) *Response, lim limits) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go serve(l, handle, logx.Discard(), lim)
	return l.Addr().String()
}

func okHandler(*Request) *Response { return text(200, "ok") }

// A client that sends its request slowly (or not at all) is cut off at the deadline.
func TestSlowClient(t *testing.T) {
	addr := startServerLimits(t, okHandler, limits{conns: defaultLimits.conns, ioTimeout: 200 * time.Millisecond})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	go func() {
		for range 20 {
			if _, err := c.Write([]byte("X")); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, _ := io.ReadAll(c)
	if d := time.Since(start); d > 2*time.Second || len(b) != 0 {
		t.Fatalf("closed after %v with %q", d, b)
	}
}

// Connections beyond the limit are closed at once, and the server keeps working.
func TestConnLimit(t *testing.T) {
	addr := startServerLimits(t, okHandler, limits{conns: 2, ioTimeout: time.Second})
	var held []net.Conn
	for range 2 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		held = append(held, c)
	}
	time.Sleep(50 * time.Millisecond)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	var ne net.Error
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil || errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("over the limit: want the connection closed, got %d %v", n, err)
	}
	c.Close()
	for _, h := range held {
		h.Close()
	}
	time.Sleep(50 * time.Millisecond)
	c, err = net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	b, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(b), "HTTP/1.1 200 OK\r\n") || !strings.Contains(string(b), "Connection: close") {
		t.Fatalf("%q", b)
	}
}

func TestErrorResponse(t *testing.T) {
	addr := startServer(t, okHandler)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("BREW /pot HTTP/3\r\n\r\n"))
	b, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(b), "HTTP/1.1 505 ") {
		t.Fatalf("%q", b)
	}
}
