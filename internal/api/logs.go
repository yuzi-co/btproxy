package api

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"btproxy/internal/ble"
)

// ESPHome log levels (LogLevel in api.proto).
const (
	logNone        = 0
	logError       = 1
	logWarn        = 2
	logInfo        = 3
	logConfig      = 4
	logDebug       = 5
	logVerbose     = 6
	logVeryVerbose = 7
)

// logQueue is how many messages wait for a slow client before newer ones are dropped.
const logQueue = 64

// Logs sends log records to the API clients that asked for them with SubscribeLogsRequest, formatted like
// ESPHome's logger so Home Assistant and `esphome logs` show them as they show a device's. Records are only
// queued in Handle; each subscriber has its own sender, and a client that falls behind loses messages instead
// of holding up the program. Nothing on that path logs, so a log line never produces another.
type Logs struct {
	mu   sync.Mutex
	subs map[*client]*logSub
	max  atomic.Int32 // most verbose level any subscriber wants, logNone without subscribers
}

type logSub struct {
	level   int32 // guarded by Logs.mu
	ch      chan []byte
	dropped atomic.Int64
}

// Handler returns an slog.Handler that passes records to next, as before, and also to the subscribers.
// Records below next's level still reach subscribers that asked for them.
func (l *Logs) Handler(next slog.Handler) slog.Handler { return &logHandler{logs: l, next: next} }

// subscribe sets a client's level, starting its sender on the first subscription. Level logNone keeps the
// subscription but sends nothing, as in ESPHome.
func (l *Logs) subscribe(c *client, level int32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.subs == nil {
		l.subs = map[*client]*logSub{}
	}
	sub := l.subs[c]
	if sub == nil {
		sub = &logSub{ch: make(chan []byte, logQueue)}
		l.subs[c] = sub
		go sub.run(c)
	}
	sub.level = level
	l.updateMax()
}

func (l *Logs) unsubscribe(c *client) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if sub := l.subs[c]; sub != nil {
		delete(l.subs, c)
		close(sub.ch)
		l.updateMax()
	}
}

// updateMax is called with l.mu held.
func (l *Logs) updateMax() {
	m := int32(logNone)
	for _, sub := range l.subs {
		m = max(m, sub.level)
	}
	l.max.Store(m)
}

// publish queues a message for every subscriber whose level takes it, dropping it for those whose queue is
// full.
func (l *Logs) publish(level int32, payload []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, sub := range l.subs {
		if level > sub.level {
			continue
		}
		select {
		case sub.ch <- payload:
		default:
			sub.dropped.Add(1)
		}
	}
}

// run sends a subscriber's messages until it unsubscribes. Drops are reported to that client once its queue
// has room again, not logged: a log line about them would only queue behind them.
func (sub *logSub) run(c *client) {
	for p := range sub.ch {
		c.send(msgSubscribeLogsResponse, p)
		if n := sub.dropped.Swap(0); n > 0 {
			c.send(msgSubscribeLogsResponse, encLog(logWarn,
				formatLog(logWarn, "api", 0, fmt.Sprintf("%d log messages dropped, client too slow", n))))
		}
	}
}

// configDump answers dump_config, which ESPHome devices answer with each component's settings at CONFIG level.
func (s *Server) configDump() string {
	s.mu.Lock()
	used := len(s.conns)
	s.mu.Unlock()
	mode := "active"
	if _, ok := s.adapter.(ble.ModeScanner); ok && s.cfg.ScanMode == ble.ScanPassive {
		mode = "passive"
	}
	return fmt.Sprintf("Bluetooth Proxy:\n"+
		"  Name: %s\n  Version: %s (ESPHome %s)\n  MAC: %s\n  Backend: %s\n  Adapter: %s (%s)\n"+
		"  Scan mode: %s\n  Connections: %d of %d in use",
		s.cfg.Name, s.cfg.ProjectVersion, s.cfg.Version, s.cfg.MAC, s.cfg.Backend, s.cfg.Adapter,
		ble.FormatMAC(s.adapter.MAC()), mode, used, s.cfg.MaxConnections)
}

// esphomeLevel maps an slog level to ESPHome's. CONFIG has no slog counterpart: only the configuration dump
// uses it. Levels below Debug, which btproxy does not use, become VERBOSE and VERY_VERBOSE.
func esphomeLevel(l slog.Level) int32 {
	switch {
	case l >= slog.LevelError:
		return logError
	case l >= slog.LevelWarn:
		return logWarn
	case l >= slog.LevelInfo:
		return logInfo
	case l >= slog.LevelDebug:
		return logDebug
	case l >= slog.LevelDebug-4:
		return logVerbose
	}
	return logVeryVerbose
}

// Letters and ANSI colours of ESPHome's log lines (logger/log_buffer.h), indexed by level.
var (
	logLetters = [...]string{"", "E", "W", "I", "C", "D", "V", "VV"}
	logColors  = [...]string{"", "\033[1;31m", "\033[0;33m", "\033[0;32m", "\033[0;35m", "\033[0;36m",
		"\033[0;37m", "\033[0;38m"}
)

// formatLog builds a line the way ESPHome's logger does: "[I][tag:042]: text" in the level's colour. Home
// Assistant strips the colour; `esphome logs` shows it.
func formatLog(level int32, tag string, line int, text string) []byte {
	return fmt.Appendf(nil, "%s[%s][%s:%03d]: %s\033[0m", logColors[level], logLetters[level], tag, line, text)
}

func encLog(level int32, msg []byte) []byte {
	var e enc
	e.uint(1, uint64(level))
	e.bytes(3, msg)
	return e.b
}

// logHandler is the slog.Handler from Logs.Handler. attrs holds the attributes added with WithAttrs, already
// formatted, and group the prefix from WithGroup.
type logHandler struct {
	logs  *Logs
	next  slog.Handler
	attrs string
	group string
}

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l) || esphomeLevel(l) <= h.logs.max.Load()
}

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.next.Enabled(ctx, r.Level) {
		err = h.next.Handle(ctx, r)
	}
	level := esphomeLevel(r.Level)
	if level > h.logs.max.Load() {
		return err
	}
	// The tag and line are the Go file and line that logged, standing in for ESPHome's component tag.
	tag, line := "btproxy", 0
	if r.PC != 0 {
		f, _ := runtime.CallersFrames([]uintptr{r.PC}).Next()
		tag, line = strings.TrimSuffix(filepath.Base(f.File), ".go"), f.Line
	}
	var b strings.Builder
	b.WriteString(r.Message)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.group, a)
		return true
	})
	h.logs.publish(level, encLog(level, formatLog(level, tag, line, b.String())))
	return err
}

func (h *logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range as {
		appendAttr(&b, h.group, a)
	}
	return &logHandler{logs: h.logs, next: h.next.WithAttrs(as), attrs: b.String(), group: h.group}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &logHandler{logs: h.logs, next: h.next.WithGroup(name), attrs: h.attrs, group: h.group + name + "."}
}

// appendAttr writes " key=value" as slog's text handler does, quoting values that need it.
func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			appendAttr(b, prefix, g)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	v := a.Value.String()
	if needsQuote(v) {
		v = strconv.Quote(v)
	}
	b.WriteString(v)
}

func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r == ' ' || r == '=' || r == '"' || r == utf8.RuneError || !unicode.IsPrint(r) {
			return true
		}
	}
	return false
}
