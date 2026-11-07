package ble

import (
	"bytes"
	"testing"
)

func TestShortUUID(t *testing.T) {
	battery, _ := ParseUUID("0000180f-0000-1000-8000-00805f9b34fb")
	if u := ShortUUID(0x180f); u != battery {
		t.Errorf("16-bit: %v", u)
	}
	if s, ok := u32(0x1234abcd).Short(); !ok || s != 0x1234abcd {
		t.Errorf("32-bit: %x, %v", s, ok)
	}
	custom, _ := ParseUUID("6e400001-b5a3-f393-e0a9-e50e24dcca9e")
	if _, ok := custom.Short(); ok {
		t.Error("custom UUID has a short form")
	}
}

func u32(v uint32) UUID { return ShortUUID(v) }

func TestUUIDLE(t *testing.T) {
	if u := UUIDFromLE([]byte{0x0d, 0x18}); u != ShortUUID(0x180d) {
		t.Errorf("16-bit: %v", u)
	}
	if u := UUIDFromLE([]byte{0xcd, 0xab, 0x34, 0x12}); u != ShortUUID(0x1234abcd) {
		t.Errorf("32-bit: %v", u)
	}
	want, _ := ParseUUID("6e400001-b5a3-f393-e0a9-e50e24dcca9e")
	le := []byte{0x9e, 0xca, 0xdc, 0x24, 0x0e, 0xe5, 0xa9, 0xe0, 0x93, 0xf3, 0xa3, 0xb5, 0x01, 0x00, 0x40, 0x6e}
	if u := UUIDFromLE(le); u != want {
		t.Errorf("128-bit: %v", u)
	}
	if got := want.AppendLE([]byte{0xff}); !bytes.Equal(got[1:], le) || got[0] != 0xff {
		t.Errorf("AppendLE: % x", got)
	}
}

func TestAddrLE(t *testing.T) {
	le := []byte{0x47, 0xa9, 0x06, 0xa2, 0x24, 0xf4}
	if got := AddrFromLE(le); got != 0xf424a206a947 {
		t.Errorf("AddrFromLE: %x", got)
	}
	var b [7]byte
	PutAddrLE(b[:], 0xf424a206a947)
	if !bytes.Equal(b[:6], le) || b[6] != 0 {
		t.Errorf("PutAddrLE: % x", b)
	}
}
