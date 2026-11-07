// Package logx is btproxy's leveled key=value logger. It writes lines in the format of log/slog's TextHandler
// ("time=... level=INFO msg=... key=value") and passes each record to an optional Tap, which streams logs to API
// clients. It stands in for log/slog, which would need a custom Handler for the tap either way and costs about
// 64 KB on mipsle (measured 2026-11-07; encoding/json, once the reason, is linked anyway by the kernel backend).
package logx

import (
	"encoding"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Level is a record's severity, with log/slog's values.
type Level int

const (
	LevelDebug Level = -4
	LevelInfo  Level = 0
	LevelWarn  Level = 4
	LevelError Level = 8
)

// String names the level as log/slog does: DEBUG, INFO, WARN or ERROR, with an offset for levels in between.
func (l Level) String() string {
	name := func(base string, d Level) string {
		if d == 0 {
			return base
		}
		if d > 0 {
			base += "+"
		}
		return base + strconv.Itoa(int(d))
	}
	switch {
	case l < LevelInfo:
		return name("DEBUG", l-LevelDebug)
	case l < LevelWarn:
		return name("INFO", l-LevelInfo)
	case l < LevelError:
		return name("WARN", l-LevelWarn)
	}
	return name("ERROR", l-LevelError)
}

// Tap receives records besides the writer, whatever the writer's level. Wants is asked before a record is
// formatted and must be cheap; Log must not block and must not log.
type Tap interface {
	Wants(Level) bool
	// Log gets the source file and line that logged ("" and 0 if unknown) and the message followed by its
	// " key=value" attributes.
	Log(level Level, file string, line int, text string)
}

// Logger writes records. Loggers made by With share the writer, level and tap of their parent.
type Logger struct {
	o     *output
	attrs string // formatted " key=value" pairs added with With
}

type output struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
	tap   Tap
}

// New returns a logger that writes records at level and above to w, one Write per line, and passes those the
// tap wants to it. w and tap may be nil.
func New(w io.Writer, level Level, tap Tap) *Logger {
	return &Logger{o: &output{w: w, level: level, tap: tap}}
}

// Discard returns a logger that drops everything.
func Discard() *Logger { return New(nil, 0, nil) }

// With returns a logger that adds the key-value pairs to every record.
func (l *Logger) With(kv ...any) *Logger {
	if len(kv) == 0 {
		return l
	}
	return &Logger{o: l.o, attrs: string(appendKVs([]byte(l.attrs), kv))}
}

// Enabled reports whether a record at level would go anywhere, so callers can skip building costly values.
func (l *Logger) Enabled(level Level) bool {
	o := l.o
	return o.w != nil && level >= o.level || o.tap != nil && o.tap.Wants(level)
}

func (l *Logger) Debug(msg string, kv ...any) { l.log(LevelDebug, msg, kv) }
func (l *Logger) Info(msg string, kv ...any)  { l.log(LevelInfo, msg, kv) }
func (l *Logger) Warn(msg string, kv ...any)  { l.log(LevelWarn, msg, kv) }
func (l *Logger) Error(msg string, kv ...any) { l.log(LevelError, msg, kv) }

// log must be called directly by the exported methods: the caller's frame is two up.
func (l *Logger) log(level Level, msg string, kv []any) {
	o := l.o
	toW := o.w != nil && level >= o.level
	toTap := o.tap != nil && o.tap.Wants(level)
	if !toW && !toTap {
		return
	}
	var now time.Time
	if toW {
		now = time.Now()
	}
	attrs := appendKVs([]byte(l.attrs), kv)
	if toTap {
		_, file, line, ok := runtime.Caller(2)
		if !ok {
			file, line = "", 0
		}
		o.tap.Log(level, file, line, msg+string(attrs))
	}
	if toW {
		b := make([]byte, 0, 64+len(msg)+len(attrs))
		b = append(b, "time="...)
		b = now.AppendFormat(b, "2006-01-02T15:04:05.000Z07:00")
		b = append(b, " level="...)
		b = append(b, level.String()...)
		b = append(b, " msg="...)
		b = appendString(b, msg)
		b = append(b, attrs...)
		b = append(b, '\n')
		o.mu.Lock()
		o.w.Write(b)
		o.mu.Unlock()
	}
}

// appendKVs appends " key=value" for each pair. Like log/slog, a value without a key gets the key !BADKEY.
func appendKVs(b []byte, kv []any) []byte {
	for len(kv) > 0 {
		key, ok := kv[0].(string)
		if !ok || len(kv) == 1 {
			b = appendKV(b, "!BADKEY", kv[0])
			kv = kv[1:]
			continue
		}
		b = appendKV(b, key, kv[1])
		kv = kv[2:]
	}
	return b
}

func appendKV(b []byte, key string, v any) []byte {
	b = append(b, ' ')
	b = appendString(b, key)
	b = append(b, '=')
	return appendValue(b, v)
}

// appendValue formats a value as log/slog's TextHandler does.
func appendValue(b []byte, v any) []byte {
	switch v := v.(type) {
	case nil:
		return append(b, "<nil>"...)
	case string:
		return appendString(b, v)
	case int:
		return strconv.AppendInt(b, int64(v), 10)
	case int8:
		return strconv.AppendInt(b, int64(v), 10)
	case int16:
		return strconv.AppendInt(b, int64(v), 10)
	case int32:
		return strconv.AppendInt(b, int64(v), 10)
	case int64:
		return strconv.AppendInt(b, v, 10)
	case uint:
		return strconv.AppendUint(b, uint64(v), 10)
	case uint8:
		return strconv.AppendUint(b, uint64(v), 10)
	case uint16:
		return strconv.AppendUint(b, uint64(v), 10)
	case uint32:
		return strconv.AppendUint(b, uint64(v), 10)
	case uint64:
		return strconv.AppendUint(b, v, 10)
	case uintptr:
		return strconv.AppendUint(b, uint64(v), 10)
	case bool:
		return strconv.AppendBool(b, v)
	case float32:
		return strconv.AppendFloat(b, float64(v), 'g', -1, 64)
	case float64:
		return strconv.AppendFloat(b, v, 'g', -1, 64)
	case time.Duration:
		return append(b, v.String()...)
	case time.Time:
		return v.AppendFormat(b, "2006-01-02T15:04:05.000Z07:00")
	case []byte:
		return strconv.AppendQuote(b, string(v))
	case encoding.TextMarshaler:
		t, err := marshalText(v)
		if err != nil {
			return appendString(b, "!ERROR:"+err.Error())
		}
		return appendString(b, string(t))
	case error:
		return appendString(b, callString(v, v.Error))
	case fmt.Stringer:
		return appendString(b, callString(v, v.String))
	}
	return appendString(b, fmt.Sprintf("%+v", v))
}

// callString calls an Error or String method, falling back to fmt, which prints "<nil>" for a nil pointer whose
// method panics.
func callString(v any, f func() string) (s string) {
	defer func() {
		if recover() != nil {
			s = fmt.Sprintf("%+v", v)
		}
	}()
	return f()
}

func marshalText(v encoding.TextMarshaler) (t []byte, err error) {
	defer func() {
		if recover() != nil {
			t, err = []byte(fmt.Sprintf("%+v", v)), nil
		}
	}()
	return v.MarshalText()
}

// appendString appends s, quoted if it is empty or holds spaces, quotes, '=' or unprintable characters.
func appendString(b []byte, s string) []byte {
	if needsQuote(s) {
		return strconv.AppendQuote(b, s)
	}
	return append(b, s...)
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
