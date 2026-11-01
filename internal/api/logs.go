package api

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"btproxy/internal/ble"
	"btproxy/internal/logx"
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
// queued in Log; each subscriber has its own sender, and a client that falls behind loses messages instead
// of holding up the program. Nothing on that path logs, so a log line never produces another.
type Logs struct {
	mu   sync.Mutex
	subs map[*client]*logSub
	max  atomic.Int32 // most verbose level any subscriber wants, logNone without subscribers
}

type logSub struct {
	level int32 // guarded by Logs.mu
	ch    chan []byte
}

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
	for c, sub := range l.subs {
		if level > sub.level {
			continue
		}
		select {
		case sub.ch <- payload:
		default:
			c.logDrops.Add(1)
		}
	}
}

// run passes a subscriber's messages to its client's queue until it unsubscribes. The client's writer reports
// the drops once it has caught up.
func (sub *logSub) run(c *client) {
	for p := range sub.ch {
		if !c.sendDroppable(msgSubscribeLogsResponse, p) {
			c.logDrops.Add(1)
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
		"  Scan mode: %s\n  Advertisement filter: %v\n  Connections: %d of %d in use",
		s.cfg.Name, s.cfg.ProjectVersion, s.cfg.Version, s.cfg.MAC, s.cfg.Backend, s.cfg.Adapter,
		ble.FormatMAC(s.adapter.MAC()), mode, s.cfg.AdvFilter, used, s.cfg.MaxConnections)
}

// esphomeLevel maps a logx level to ESPHome's. CONFIG has no logx counterpart: only the configuration dump
// uses it. Levels below Debug, which btproxy does not use, become VERBOSE and VERY_VERBOSE.
func esphomeLevel(l logx.Level) int32 {
	switch {
	case l >= logx.LevelError:
		return logError
	case l >= logx.LevelWarn:
		return logWarn
	case l >= logx.LevelInfo:
		return logInfo
	case l >= logx.LevelDebug:
		return logDebug
	case l >= logx.LevelDebug-4:
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

// Wants reports whether any subscriber takes records at level (logx.Tap).
func (l *Logs) Wants(level logx.Level) bool { return esphomeLevel(level) <= l.max.Load() }

// Log formats a record like an ESPHome log line and queues it for the subscribers that take its level
// (logx.Tap). The tag and line are the Go file and line that logged, standing in for ESPHome's component tag.
func (l *Logs) Log(level logx.Level, file string, line int, text string) {
	el := esphomeLevel(level)
	if el > l.max.Load() {
		return
	}
	tag := "btproxy"
	if file != "" {
		tag = strings.TrimSuffix(filepath.Base(file), ".go")
	}
	l.publish(el, encLog(el, formatLog(el, tag, line, text)))
}
