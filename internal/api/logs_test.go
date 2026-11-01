package api

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEsphomeLevel(t *testing.T) {
	for l, want := range map[slog.Level]int32{
		slog.LevelError + 4: logError, slog.LevelError: logError, slog.LevelWarn: logWarn, slog.LevelInfo: logInfo,
		slog.LevelInfo - 1: logDebug, slog.LevelDebug: logDebug, slog.LevelDebug - 4: logVerbose,
		slog.LevelDebug - 8: logVeryVerbose,
	} {
		if got := esphomeLevel(l); got != want {
			t.Errorf("%v: got %d, want %d", l, got, want)
		}
	}
}

func TestFormatLog(t *testing.T) {
	for _, tt := range []struct {
		level int32
		line  int
		want  string
	}{
		{logError, 7, "\033[1;31m[E][server:007]: hi\033[0m"},
		{logInfo, 1234, "\033[0;32m[I][server:1234]: hi\033[0m"},
		{logVeryVerbose, 42, "\033[0;38m[VV][server:042]: hi\033[0m"},
	} {
		if got := string(formatLog(tt.level, "server", tt.line, "hi")); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}

// logPeer subscribes a client on one end of a pipe; the test reads the other end.
type logPeer struct {
	t *testing.T
	c *client
	p net.Conn
	r *bufio.Reader
}

func newLogPeer(t *testing.T, logs *Logs, level int32) *logPeer {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	c := &client{s: &Server{}, c: a, log: slog.New(logs.Handler(slog.NewTextHandler(io.Discard, nil)))}
	logs.subscribe(c, level)
	t.Cleanup(func() { logs.unsubscribe(c) })
	return &logPeer{t: t, c: c, p: b, r: bufio.NewReader(b)}
}

// next returns the level and the text of the next log message.
func (lp *logPeer) next() (int32, string) {
	lp.t.Helper()
	lp.p.SetReadDeadline(time.Now().Add(2 * time.Second))
	f, err := ReadFrame(lp.r)
	if err != nil {
		lp.t.Fatal(err)
	}
	if f.Type != msgSubscribeLogsResponse {
		lp.t.Fatalf("message type %d", f.Type)
	}
	fs, err := fields(f.Payload)
	if err != nil {
		lp.t.Fatal(err)
	}
	var msg string
	for _, f := range fs {
		if f.num == 3 {
			msg = string(f.data)
		}
	}
	return int32(fieldValue(fs, 1)), msg
}

// TestLogsLevels checks the formatting of records and attributes and each subscriber's level filter, including
// records below the level of the handler that writes to stderr.
func TestLogsLevels(t *testing.T) {
	logs := &Logs{}
	log := slog.New(logs.Handler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if log.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info enabled without subscribers")
	}
	debug := newLogPeer(t, logs, logDebug)
	warn := newLogPeer(t, logs, logWarn)
	if !log.Enabled(context.Background(), slog.LevelDebug) || log.Enabled(context.Background(), slog.LevelDebug-4) {
		t.Fatal("Enabled does not follow the subscribers")
	}
	go func() {
		l := log.With("client", "1.2.3.4:5").WithGroup("g")
		l.Debug("hello", "api", "1.10")
		l.Warn("bad message", "err", io.EOF, "s", "two words", slog.Group("x", "n", 1))
		log.Debug("later") // for the debug subscriber only
	}()
	re := regexp.MustCompile(`^\033\[0;36m\[D\]\[logs_test:\d{3}\]: hello client=1\.2\.3\.4:5 g\.api=1\.10\033\[0m$`)
	if l, m := debug.next(); l != logDebug || !re.MatchString(m) {
		t.Errorf("debug: level %d %q", l, m)
	}
	want := `bad message client=1.2.3.4:5 g.err=EOF g.s="two words" g.x.n=1`
	if l, m := debug.next(); l != logWarn || !strings.Contains(m, "[W][logs_test:") || !strings.Contains(m, want) {
		t.Errorf("debug: level %d %q", l, m)
	}
	if l, m := warn.next(); l != logWarn || !strings.Contains(m, want) {
		t.Errorf("warn: level %d %q", l, m)
	}
	if _, m := debug.next(); !strings.Contains(m, "later") {
		t.Errorf("debug: %q", m)
	}

	logs.subscribe(debug.c, logNone)
	logs.unsubscribe(warn.c)
	if log.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("enabled after the subscribers left")
	}
}

// countHandler counts the records that reach it.
type countHandler struct{ n *atomic.Int64 }

func (h countHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h countHandler) Handle(context.Context, slog.Record) error { h.n.Add(1); return nil }
func (h countHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h countHandler) WithGroup(string) slog.Handler             { return h }

// TestLogsSlowClient logs to a client that reads nothing: logging must not wait for it, the excess is dropped,
// and the client hears about the drops once it reads again.
func TestLogsSlowClient(t *testing.T) {
	logs := &Logs{}
	log := slog.New(logs.Handler(slog.NewTextHandler(io.Discard, nil)))
	lp := newLogPeer(t, logs, logInfo)
	done := make(chan struct{})
	go func() {
		for i := range 1000 {
			log.Info("line", "i", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("logging blocked on a slow client")
	}
	got := 0
	for {
		_, m := lp.next()
		if strings.Contains(m, "log messages dropped") {
			break
		}
		got++
	}
	if got == 0 || got > logQueue+1 {
		t.Errorf("%d messages before the drop report", got)
	}
}

// TestLogsDeadClient checks that a failed write to a subscriber does not start a loop of log lines about
// failed writes, each sent to the same client.
func TestLogsDeadClient(t *testing.T) {
	var n atomic.Int64
	logs := &Logs{}
	log := slog.New(logs.Handler(countHandler{&n}))
	a, b := net.Pipe()
	b.Close()
	c := &client{s: &Server{}, c: a, log: log}
	logs.subscribe(c, logVeryVerbose)
	defer logs.unsubscribe(c)
	log.Info("first")
	time.Sleep(200 * time.Millisecond)
	if got := n.Load(); got != 2 { // "first" and one "write" error
		t.Errorf("%d records logged", got)
	}
}

// TestSubscribeLogs subscribes through the API with dump_config and gets the configuration dump, then the
// server's own log lines.
func TestSubscribeLogs(t *testing.T) {
	logs := &Logs{}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	log := slog.New(logs.Handler(slog.NewTextHandler(io.Discard, nil)))
	go NewServer(Config{Name: "test", MaxConnections: 2, Logs: logs, Backend: "fake"}, testAdapter{}, log).Serve(l)
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	tc := &testClient{t: t, c: c, r: bufio.NewReader(c)}

	var e enc
	e.uint(1, logDebug)
	e.bool(2, true)
	tc.send(msgSubscribeLogsRequest, e.b)
	fs := tc.expect(msgSubscribeLogsResponse)
	if m := string(fs[1].data); fieldValue(fs, 1) != logConfig || !strings.HasPrefix(m, "\033[0;35m[C][btproxy:000]: Bluetooth Proxy:\n  Name: test\n") ||
		!strings.Contains(m, "  Connections: 0 of 2 in use") {
		t.Fatalf("config dump: %+v", fs)
	}
	tc.send(999, nil)
	fs = tc.expect(msgSubscribeLogsResponse)
	if m := string(fs[1].data); fieldValue(fs, 1) != logInfo ||
		!regexp.MustCompile(`\[I\]\[server:\d+\]: ignoring unsupported message client=127\.0\.0\.1:\d+ type=999`).MatchString(m) {
		t.Fatalf("log line: %q", m)
	}
}
