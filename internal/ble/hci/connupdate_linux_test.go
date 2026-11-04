//go:build linux

package hci

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

// TestHCIConnUpdate checks the LE Connection Update packet and the parsing of the events that answer it.
func TestHCIConnUpdate(t *testing.T) {
	cmd := connUpdateCmd(0x0040, ble.ConnParams{MinInterval: 24, MaxInterval: 40, Latency: 1, Timeout: 500})
	want := []byte{0x01, 0x13, 0x20, 14, 0x40, 0, 24, 0, 40, 0, 1, 0, 0xf4, 0x01, 0, 0, 0, 0}
	if !bytes.Equal(cmd, want) {
		t.Errorf("command % x, want % x", cmd, want)
	}
	if st, ok := cmdStatus([]byte{0x04, 0x0f, 4, 0x12, 1, 0x13, 0x20}); !ok || st != 0x12 {
		t.Errorf("command status: %#x %v", st, ok)
	}
	if _, ok := cmdStatus([]byte{0x04, 0x0f, 4, 0, 1, 0x0d, 0x20}); ok {
		t.Error("status for another opcode accepted")
	}
	done := []byte{0x04, 0x3e, 10, 0x03, 0, 0x40, 0x20, 36, 0, 1, 0, 0xf4, 0x01} // handle with flag bits set
	st, p, ok := connUpdateDone(done, 0x0040)
	if !ok || st != 0 || p != (ble.ConnParams{MinInterval: 36, MaxInterval: 36, Latency: 1, Timeout: 500}) {
		t.Errorf("completion: %#x %+v %v", st, p, ok)
	}
	if _, _, ok := connUpdateDone(done, 0x0041); ok {
		t.Error("completion for another handle accepted")
	}
	f := hciFilter()
	if len(f) != 14 || binary.LittleEndian.Uint16(f[12:]) != opLEConnUpdate {
		t.Errorf("filter % x", f)
	}
}

// TestConnUpdateNoConnection runs the socket path as far as it goes without a controller: there is no hci0 in a
// container, or no connection to the address if there is one; either way an error, not a hang.
func TestConnUpdateNoConnection(t *testing.T) {
	err := ConnUpdate(0, 0xAABBCCDDEEFF, ble.ConnParams{MinInterval: 24, MaxInterval: 40, Timeout: 500}, logx.Discard())
	if err == nil {
		t.Fatal("no error")
	}
	t.Log(err)
}
