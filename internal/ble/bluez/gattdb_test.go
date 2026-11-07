//go:build linux

package bluez

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestHandleOf(t *testing.T) {
	for p, want := range map[dbus.ObjectPath]uint16{
		"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a":                   0x0a,
		"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a/char000b":          0x0b,
		"/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a/char000b/desc00d0": 0xd0,
	} {
		if h, ok := handleOf(p); !ok || h != want {
			t.Errorf("handleOf(%s) = %#x, %v; want %#x", p, h, ok, want)
		}
	}
	if _, ok := handleOf("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF"); ok {
		t.Error("device path parsed as a handle")
	}
}

// TestBuildGATT checks that characteristics are reported by value handle (BlueZ's path number + 1, as an
// ESP32 reports them) while services and descriptors keep the handle from their path.
func TestBuildGATT(t *testing.T) {
	dev := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	svc, chr := dev+"/service000a", dev+"/service000a/char000b"
	uuid := func(s string) dbus.Variant { return dbus.MakeVariant(s) }
	objs := map[dbus.ObjectPath]map[string]map[string]dbus.Variant{
		dev: {ifDevice: {}},
		svc: {ifService: {"UUID": uuid("0000180f-0000-1000-8000-00805f9b34fb")}},
		chr: {ifChar: {"UUID": uuid("00002a19-0000-1000-8000-00805f9b34fb"), "Service": dbus.MakeVariant(svc),
			"Flags": dbus.MakeVariant([]string{"read", "notify"}), "MTU": dbus.MakeVariant(uint16(247))}},
		chr + "/desc000e": {ifDesc: {"UUID": uuid("00002904-0000-1000-8000-00805f9b34fb"),
			"Characteristic": dbus.MakeVariant(chr)}},
		chr + "/desc000d": {ifDesc: {"UUID": uuid("00002902-0000-1000-8000-00805f9b34fb"),
			"Characteristic": dbus.MakeVariant(chr)}},
		"/org/bluez/hci0/dev_11_22_33_44_55_66/service0001": {ifService: {}}, // another device
	}
	db := buildGATT(dev, objs)
	if len(db.services) != 1 || db.services[0].Handle != 0x0a {
		t.Fatalf("services = %+v", db.services)
	}
	chars := db.services[0].Characteristics
	if len(chars) != 1 || chars[0].Handle != 0x0c || chars[0].Properties != 0x12 {
		t.Fatalf("characteristics = %+v", chars)
	}
	if d := chars[0].Descriptors; len(d) != 2 || d[0].Handle != 0x0d || d[1].Handle != 0x0e {
		t.Errorf("descriptors = %+v", d)
	}
	if db.chars[0x0c] != chr || db.descs[0x0d] != chr+"/desc000d" || db.cccdOwner[0x0d] != 0x0c || db.mtu != 247 {
		t.Errorf("maps: chars %v descs %v cccd %v mtu %d", db.chars, db.descs, db.cccdOwner, db.mtu)
	}
	if h, ok := charValueHandle(chr); !ok || h != 0x0c {
		t.Errorf("charValueHandle(%s) = %#x, %v", chr, h, ok)
	}
}
