package winrt

import (
	"syscall"
	"testing"
)

func TestUUIDOf(t *testing.T) {
	// 0000180f-0000-1000-8000-00805f9b34fb, the Battery service.
	g := syscall.GUID{Data1: 0x0000180f, Data2: 0x0000, Data3: 0x1000,
		Data4: [8]byte{0x80, 0x00, 0x00, 0x80, 0x5f, 0x9b, 0x34, 0xfb}}
	u := uuidOf(g)
	if s := u.String(); s != "0000180f-0000-1000-8000-00805f9b34fb" {
		t.Fatalf("got %s", s)
	}
	if v, ok := u.Short(); !ok || v != 0x180f {
		t.Fatalf("short %x %v", v, ok)
	}
	if uuidOf(syscall.GUID{Data1: 0x2902, Data3: 0x1000, Data4: g.Data4}) != cccdUUID {
		t.Fatal("cccd uuid mismatch")
	}
}
