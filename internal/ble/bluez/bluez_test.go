//go:build linux

package bluez

import (
	"bytes"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"

	"btproxy/internal/ble"
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

func TestADData(t *testing.T) {
	tx := int16(-4)
	d := device{
		flags:   []byte{0x06},
		name:    "Tion",
		txPower: &tx,
		uuids:   []string{"0000180f-0000-1000-8000-00805f9b34fb", "6e400001-b5a3-f393-e0a9-e50e24dcca9e"},
		mfr:     map[uint16][]byte{0x004c: {0x02, 0x15}},
		svcData: map[string][]byte{"0000fe95-0000-1000-8000-00805f9b34fb": {0x01}},
	}
	want := []byte{
		0x02, 0x01, 0x06,
		0x05, 0xff, 0x4c, 0x00, 0x02, 0x15,
		0x04, 0x16, 0x95, 0xfe, 0x01,
		0x03, 0x03, 0x0f, 0x18,
		0x11, 0x07, 0x9e, 0xca, 0xdc, 0x24, 0x0e, 0xe5, 0xa9, 0xe0, 0x93, 0xf3, 0xa3, 0xb5, 0x01, 0x00, 0x40, 0x6e,
		0x05, 0x09, 'T', 'i', 'o', 'n',
		0x02, 0x0a, 0xfc,
	}
	if got := d.adData(); !bytes.Equal(got, want) {
		t.Errorf("adData\n got %x\nwant %x", got, want)
	}
	d.name = string(make([]byte, 40)) // too long to fit: dropped, the rest stays
	if got := d.adData(); len(got) > 62 || bytes.Contains(got, []byte{41, 0x09}) {
		t.Errorf("oversized name not dropped: %x", got)
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

// TestReadEcho checks that the Value change BlueZ emits for our own ReadValue is not forwarded as a
// notification, whether it is seen during the read or after it, while real notifications still are.
func TestReadEcho(t *testing.T) {
	const h = 0x0c
	p := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF/service000a/char000b")
	var got []string
	c := &conn{notifying: map[uint16]bool{h: true}, reading: map[uint16]int{}, held: map[uint16][][]byte{},
		echoes: map[uint16][]echo{},
		ev: ble.Events{Notification: func(hh uint16, d []byte) {
			if hh != h {
				t.Errorf("notification for %#x", hh)
			}
			got = append(got, string(d))
		}}}

	// Echo seen while the read is in flight, next to a real notification.
	c.readStarted(h)
	c.valueChanged(p, []byte("n1"))
	c.valueChanged(p, []byte("r1"))
	c.readDone(h, []byte("r1"), true)
	// Echo seen after the read returned; a later notification with the same value is real.
	c.readStarted(h)
	c.readDone(h, []byte("r2"), true)
	c.valueChanged(p, []byte("n2"))
	c.valueChanged(p, []byte("r2"))
	c.valueChanged(p, []byte("r2"))
	// A failed read expects no echo.
	c.readStarted(h)
	c.readDone(h, nil, false)
	c.valueChanged(p, nil)

	want := []string{"n1", "n2", "r2", ""}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("notifications %q, want %q", got, want)
	}
}
