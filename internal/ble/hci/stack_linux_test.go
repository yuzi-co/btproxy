//go:build linux

package hci

import (
	"encoding/binary"
	"testing"
)

// ctrlOpen builds what the kernel sends a monitor for a socket on a control channel, as btmon shows it.
func ctrlOpen(comm string) []byte {
	name := make([]byte, 16)
	copy(name, comm)
	p := binary.LittleEndian.AppendUint32(nil, 1) // cookie
	p = binary.LittleEndian.AppendUint16(p, 2)    // format: management
	p = append(p, 1, 23, 0)                       // version
	p = binary.LittleEndian.AppendUint32(p, 1)    // flags: trusted
	p = append(p, byte(len(name)))
	p = append(p, name...)
	b := binary.LittleEndian.AppendUint16(nil, 14) // HCI_MON_CTRL_OPEN, as seen from a real kernel
	b = binary.LittleEndian.AppendUint16(b, hciDevNone)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(p)))
	return append(b, p...)
}

func TestControlOwner(t *testing.T) {
	short := ctrlOpen("bluetoothd")
	other := ctrlOpen("bluetoothd")
	binary.LittleEndian.PutUint16(other, monEventPk)
	for _, tc := range []struct {
		name string
		pkt  []byte
		want string
	}{
		{"bluetoothd", ctrlOpen("bluetoothd"), "bluetoothd"},
		{"another program", ctrlOpen("btproxy"), "btproxy"},
		{"another packet", other, ""},
		{"cut off in the name", short[:monHdrLen+14+3], ""},
		{"cut off in the header", short[:monHdrLen+5], ""},
		{"nothing", nil, ""},
	} {
		if got := controlOwner(tc.pkt); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestStackRunning runs against the real kernel where it has Bluetooth and the test CAP_NET_RAW. Any answer will do,
// as long as asking works.
func TestStackRunning(t *testing.T) {
	if _, err := StackRunning(); err != nil {
		t.Skip("no HCI monitor here:", err)
	}
}
