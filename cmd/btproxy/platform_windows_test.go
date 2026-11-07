package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// autostartFormValue is what the settings form sends for the autostart checkbox: its current state ("" is
// unchecked, as a browser leaves it out), so the end-to-end test leaves the real Run value alone.
func autostartFormValue() string {
	if autostartEnabled(autostartValue) {
		return "true"
	}
	return ""
}

// testChildArgs go to every proxy a test starts: no tray icon or message boxes, no settings page, and no
// settings file from %LOCALAPPDATA% (a later -config overrides this one).
var testChildArgs = []string{"-tray=false", "-ui", "", "-config", ""}

func TestAutostart(t *testing.T) {
	name := "btproxy-test-" + strconv.Itoa(os.Getpid())
	t.Cleanup(func() { setAutostart(name, false, "") })
	if autostartEnabled(name) {
		t.Fatal("enabled before the test")
	}
	cmd, err := autostartCommand()
	if err != nil || !strings.HasPrefix(cmd, `"`) || !strings.HasSuffix(cmd, `.exe"`) {
		t.Fatalf("command %q %v", cmd, err)
	}
	if err := setAutostart(name, true, cmd); err != nil {
		t.Fatal(err)
	}
	if !autostartEnabled(name) {
		t.Fatal("not enabled after setting it")
	}
	if err := setAutostart(name, false, ""); err != nil {
		t.Fatal(err)
	}
	if autostartEnabled(name) {
		t.Fatal("still enabled after removing it")
	}
	if err := setAutostart(name, false, ""); err != nil {
		t.Fatal("removing a missing value:", err)
	}
}

func TestRotatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "btproxy.log")
	opened := 0
	r, err := openRotating(path, 100, func(*os.File) { opened++ })
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for range 5 {
		r.Write([]byte(line))
	}
	r.f.Close()
	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	// 40+40 fit, the third rotates; then 40+40 again, the fifth rotates once more.
	if len(cur) != 40 || len(old) != 80 || opened != 3 {
		t.Fatalf("current %d bytes, old %d bytes, opened %d times", len(cur), len(old), opened)
	}
	// Reopening appends.
	r, err = openRotating(path, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Write([]byte(line))
	r.f.Close()
	if cur, _ := os.ReadFile(path); len(cur) != 80 {
		t.Fatalf("after reopening: %d bytes", len(cur))
	}
}

// While another process holds the log file (the old proxy, for a few seconds after a restart), the rename
// fails: logging goes on in the same file, the older file is kept, and the next attempt waits for another
// max bytes rather than repeating on every write.
func TestRotatingFileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "btproxy.log")
	if err := os.WriteFile(path+".1", []byte("older\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opened := 0
	r, err := openRotating(path, 100, func(*os.File) { opened++ })
	if err != nil {
		t.Fatal(err)
	}
	hold, err := os.Open(path) // Go opens without FILE_SHARE_DELETE, so the file cannot be moved
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hold.Close(); r.f.Close() }) // else TempDir cannot be removed after a failure
	line := strings.Repeat("x", 39) + "\n"          // 40 bytes
	for range 5 {
		r.Write([]byte(line)) // the third and the fifth would rotate
	}
	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	if len(cur) != 200 || string(old) != "older\n" || opened != 3 {
		t.Fatalf("held: current %d bytes, old %q, opened %d times", len(cur), old, opened)
	}
	hold.Close()
	r.Write([]byte(line)) // 80 bytes since the last attempt: not yet
	r.Write([]byte(line)) // 120: rotates now
	cur, _ = os.ReadFile(path)
	old, _ = os.ReadFile(path + ".1")
	if len(cur) != 40 || len(old) != 240 || opened != 4 {
		t.Fatalf("released: current %d bytes, old %d bytes, opened %d times", len(cur), len(old), opened)
	}
}

func TestPickIcon(t *testing.T) {
	for size, want := range map[int]int{16: 16, 20: 24, 24: 24, 32: 32, 48: 32, 8: 16} {
		img, ok := pickIcon(iconData, size)
		if !ok {
			t.Fatalf("%d: nothing", size)
		}
		// The images are PNGs; the width is in the IHDR chunk.
		if string(img[1:4]) != "PNG" || int(img[19]) != want {
			t.Errorf("size %d: got a %d pixel image, want %d", size, img[19], want)
		}
	}
	if _, ok := pickIcon([]byte{0, 0, 1, 0, 1, 0}, 16); ok {
		t.Error("truncated icon accepted")
	}
}

// A saved settings file is for its owner and SYSTEM only, whatever its directory allows.
func TestSaveConfigDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for range 2 { // the second save replaces the protected file
		if err := saveConfig(path, map[string]string{"encryption-key": "k"}); err != nil {
			t.Fatal(err)
		}
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sd.String(), "(A;;FA;;;OW)(A;;FA;;;SY)"; !strings.HasPrefix(got, "D:P") || !strings.HasSuffix(got, want) {
		t.Fatalf("DACL %s, want it protected and ending in %s", got, want)
	}
	if m, err := loadConfig(path); err != nil || m["encryption-key"] != "k" {
		t.Fatalf("reading it back: %v %v", m, err)
	}
}
