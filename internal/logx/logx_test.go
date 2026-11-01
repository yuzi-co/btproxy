package logx

import (
	"bytes"
	"errors"
	"net"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// strip removes the time from the lines in buf.
func strip(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	re := regexp.MustCompile(`^time=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}(Z|[+-]\d\d:\d\d) `)
	var lines []string
	for _, l := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if l == "" {
			continue
		}
		if !re.MatchString(l) {
			t.Errorf("no time: %q", l)
		}
		lines = append(lines, re.ReplaceAllString(l, ""))
	}
	buf.Reset()
	return lines
}

func TestLevels(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelInfo, nil)
	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")
	want := []string{"level=INFO msg=i", "level=WARN msg=w", "level=ERROR msg=e"}
	if got := strip(t, &buf); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q", got)
	}
	for l, s := range map[Level]string{LevelDebug: "DEBUG", LevelInfo: "INFO", LevelWarn: "WARN", LevelError: "ERROR",
		LevelDebug - 4: "DEBUG-4", LevelInfo + 1: "INFO+1", LevelError + 2: "ERROR+2"} {
		if l.String() != s {
			t.Errorf("%d: %s, want %s", int(l), l.String(), s)
		}
	}
}

type stringer struct{}

func (stringer) String() string { return "str ing" }

type ptrErr struct{ s string }

func (e *ptrErr) Error() string { return e.s }

func TestValues(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelDebug, nil)
	var nilErr *ptrErr
	for _, tt := range []struct {
		kv   []any
		want string
	}{
		{[]any{"s", "plain"}, "s=plain"},
		{[]any{"s", ""}, `s=""`},
		{[]any{"s", "two words"}, `s="two words"`},
		{[]any{"s", `say "hi"`}, `s="say \"hi\""`},
		{[]any{"s", "a=b"}, `s="a=b"`},
		{[]any{"s", "tab\there"}, `s="tab\there"`},
		{[]any{"s", `back\slash`}, `s=back\slash`},
		{[]any{"s", "ünïcode"}, "s=ünïcode"},
		{[]any{"key with space", 1}, `"key with space"=1`},
		{[]any{"i", -3, "u16", uint16(7), "u64", uint64(1 << 63), "b", true}, "i=-3 u16=7 u64=9223372036854775808 b=true"},
		{[]any{"f", 1.5}, "f=1.5"},
		{[]any{"d", 1500 * time.Millisecond}, "d=1.5s"},
		{[]any{"err", errors.New("no such device")}, `err="no such device"`},
		{[]any{"err", error(nil)}, "err=<nil>"},
		{[]any{"err", nilErr}, "err=<nil>"},
		{[]any{"st", stringer{}}, `st="str ing"`},
		{[]any{"ip", net.IPv4(10, 0, 50, 1)}, "ip=10.0.50.1"},
		{[]any{"bytes", []byte("a b")}, `bytes="a b"`},
		{[]any{"list", []string{"a", "b"}}, `list="[a b]"`},
		{[]any{"struct", struct{ A int }{1}}, "struct={A:1}"},
		{[]any{"alone"}, "!BADKEY=alone"},
		{[]any{42, "x"}, "!BADKEY=42 !BADKEY=x"},
	} {
		l.Info("m", tt.kv...)
		got := strip(t, &buf)
		if want := "level=INFO msg=m " + tt.want; len(got) != 1 || got[0] != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	l.Info("two words")
	if got := strip(t, &buf); got[0] != `level=INFO msg="two words"` {
		t.Errorf("got %q", got)
	}
}

func TestWith(t *testing.T) {
	var buf bytes.Buffer
	root := New(&buf, LevelInfo, nil)
	a := root.With("client", "1.2.3.4:5")
	b := a.With("addr", "AA:BB", "n", 2)
	if root.With() != root {
		t.Error("With() made a new logger")
	}
	root.Info("r")
	a.Info("a", "k", "v")
	b.Warn("b")
	a.Info("a again")
	want := []string{
		"level=INFO msg=r",
		"level=INFO msg=a client=1.2.3.4:5 k=v",
		"level=WARN msg=b client=1.2.3.4:5 addr=AA:BB n=2",
		`level=INFO msg="a again" client=1.2.3.4:5`,
	}
	if got := strip(t, &buf); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q", got)
	}
}

// tap records what it gets and takes levels from want up.
type tap struct {
	want Level
	recs []string
}

func (t *tap) Wants(l Level) bool { return l >= t.want }
func (t *tap) Log(l Level, file string, line int, text string) {
	t.recs = append(t.recs, l.String()+" "+filepath.Base(file)+":"+strconv.Itoa(line)+" "+text)
}

func TestTap(t *testing.T) {
	var buf bytes.Buffer
	tp := &tap{want: LevelDebug}
	l := New(&buf, LevelWarn, tp).With("c", 1)
	if !l.Enabled(LevelDebug) || l.Enabled(LevelDebug-1) {
		t.Error("Enabled ignores the tap")
	}
	_, _, line, _ := runtime.Caller(0)
	l.Debug("to tap", "k", "two words")
	l.Warn("both")
	if got := strip(t, &buf); len(got) != 1 || got[0] != "level=WARN msg=both c=1" {
		t.Errorf("writer: %q", got)
	}
	want := []string{
		"DEBUG logx_test.go:" + strconv.Itoa(line+1) + ` to tap c=1 k="two words"`,
		"WARN logx_test.go:" + strconv.Itoa(line+2) + " both c=1",
	}
	if strings.Join(tp.recs, "|") != strings.Join(want, "|") {
		t.Errorf("tap: %q, want %q", tp.recs, want)
	}

	tp.want = LevelError
	if l.Enabled(LevelInfo) || !l.Enabled(LevelWarn) {
		t.Error("Enabled")
	}
	if d := Discard(); d.Enabled(LevelError) {
		t.Error("Discard enabled")
	}
	Discard().Error("nothing")
}
