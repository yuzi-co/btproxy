//go:build linux

package bluez

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

// TestMonitorObjects checks the monitor tree BlueZ reads: or_patterns monitors under the root, Patterns of
// D-Bus type a(yyay), and no monitor over the 7 patterns BlueZ 5.66 can pass to the kernel.
func TestMonitorObjects(t *testing.T) {
	objs := monitorObjects()
	if len(objs) != len(monitorPatterns) {
		t.Fatalf("%d objects, want %d", len(objs), len(monitorPatterns))
	}
	for p, ifs := range objs {
		if !p.IsValid() || len(p) <= len(monitorRoot) || p[:len(monitorRoot)+1] != monitorRoot+"/" {
			t.Errorf("monitor path %s not under %s", p, monitorRoot)
		}
		props := ifs[ifMonitor]
		if props["Type"].Value() != "or_patterns" {
			t.Errorf("%s: type %v", p, props["Type"])
		}
		if sig := props["Patterns"].Signature(); sig != dbus.ParseSignatureMust("a(yyay)") {
			t.Errorf("%s: Patterns signature %s", p, sig)
		}
	}
	for i, ps := range monitorPatterns {
		if len(ps) == 0 || len(ps) > 7 {
			t.Errorf("monitor %d has %d patterns", i, len(ps))
		}
	}
}
