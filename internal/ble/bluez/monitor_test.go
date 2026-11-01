//go:build linux

package bluez

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

// TestMonitorObjects checks the monitor trees BlueZ reads, the real one and the one that matches nothing:
// or_patterns monitors under the root, Patterns of D-Bus type a(yyay), no monitor over the 7 patterns BlueZ
// 5.66 can pass to the kernel, and no pattern past the 31 bytes of an AD structure, which BlueZ refuses.
func TestMonitorObjects(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		patterns := monitorPatterns
		if quiet {
			patterns = quietPatterns
		}
		objs := monitorObjects(quiet)
		if len(objs) != len(patterns) {
			t.Fatalf("quiet %v: %d objects, want %d", quiet, len(objs), len(patterns))
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
		for i, ps := range patterns {
			if len(ps) == 0 || len(ps) > 7 {
				t.Errorf("quiet %v: monitor %d has %d patterns", quiet, i, len(ps))
			}
			for _, p := range ps {
				if int(p.Start)+len(p.Content) > 31 {
					t.Errorf("quiet %v: monitor %d: pattern of %d bytes at %d", quiet, i, len(p.Content), p.Start)
				}
			}
		}
	}
}
