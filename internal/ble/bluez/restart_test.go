//go:build linux

package bluez

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
)

func TestStackRestartable(t *testing.T) {
	if (&Adapter{}).StackRestartable() {
		t.Error("restartable with no command")
	}
	a := &Adapter{}
	a.restartCmd = "true"
	if !a.StackRestartable() {
		t.Error("not restartable with a command")
	}
}

// TestRestartStack presses the button twice in a row: the second is refused. A press also holds off the
// watchdog, as there is one bluetoothd, but a later press does not have to wait for restartEvery.
func TestRestartStack(t *testing.T) {
	bluetoothdRestarts = restartGate{} // shared by the process: an earlier test's restart counts
	t.Cleanup(func() { bluetoothdRestarts = restartGate{} })
	marker := filepath.Join(t.TempDir(), "restarts")
	restarts := func() int {
		b, _ := os.ReadFile(marker)
		return strings.Count(string(b), "\n")
	}
	a := &Adapter{}
	a.log = logx.Discard()
	a.restartCmd = "echo restart >> '" + marker + "'"

	if err := a.RestartStack(); err != nil {
		t.Fatal(err)
	}
	if restarts() != 1 || a.Restarts() != 1 {
		t.Fatalf("ran %d times, counted %d", restarts(), a.Restarts())
	}
	if err := a.RestartStack(); err == nil {
		t.Fatal("second press in a row was not refused")
	}
	if restarts() != 1 {
		t.Fatalf("refused press ran the command: %d", restarts())
	}
	if bluetoothdRestarts.claim() {
		t.Error("the watchdog may restart right after a press")
	}

	bluetoothdRestarts.mu.Lock()
	bluetoothdRestarts.last = time.Now().Add(-manualRestartEvery - time.Second)
	bluetoothdRestarts.mu.Unlock()
	if err := a.RestartStack(); err != nil {
		t.Fatalf("press after the short limit: %v", err)
	}
	if restarts() != 2 {
		t.Fatalf("ran %d times", restarts())
	}
}

func TestRestartStackReportsAFailedCommand(t *testing.T) {
	bluetoothdRestarts = restartGate{}
	t.Cleanup(func() { bluetoothdRestarts = restartGate{} })
	a := &Adapter{}
	a.log = logx.Discard()
	a.restartCmd = "exit 3"
	if err := a.RestartStack(); err == nil {
		t.Fatal("no error from a failing command")
	}
}

func TestAdvertSource(t *testing.T) {
	a := &Adapter{}
	if got := a.AdvertSource(); got != "Rebuilt from BlueZ" {
		t.Errorf("without a monitor: %q", got)
	}
	a.raw = &fakeRaw{}
	if got := a.AdvertSource(); got != "HCI monitor" {
		t.Errorf("with a monitor: %q", got)
	}
}
