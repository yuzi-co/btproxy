//go:build linux

package bluez

import (
	"bytes"
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
)

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
	if got := d.adData(); len(got) > ble.MaxAdvert || bytes.Contains(got, []byte{41, 0x09}) {
		t.Errorf("oversized name not dropped: %x", got)
	}
}
