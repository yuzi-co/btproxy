// Package ble defines what the proxy needs from a Bluetooth stack. Backends: bluez (Linux, D-Bus) and fake (tests).
package ble

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Address types as ESPHome reports them.
const (
	AddrPublic uint32 = 0
	AddrRandom uint32 = 1
)

// Advert is one received advertisement. Data holds the raw AD structures (advertisement plus scan response),
// at most 62 bytes.
type Advert struct {
	Addr     uint64
	AddrType uint32
	RSSI     int8
	Data     []byte
}

// UUID is a 128-bit UUID split the way the ESPHome API carries it: Hi holds the first 8 bytes.
type UUID struct{ Hi, Lo uint64 }

// Bluetooth base UUID 0000xxxx-0000-1000-8000-00805f9b34fb.
const baseHi, baseLo = 0x0000000000001000, 0x800000805f9b34fb

// ParseUUID parses the canonical 36-character form.
func ParseUUID(s string) (UUID, error) {
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 {
		return UUID{}, fmt.Errorf("bad uuid %q", s)
	}
	hi, err := strconv.ParseUint(h[:16], 16, 64)
	if err != nil {
		return UUID{}, fmt.Errorf("bad uuid %q", s)
	}
	lo, err := strconv.ParseUint(h[16:], 16, 64)
	if err != nil {
		return UUID{}, fmt.Errorf("bad uuid %q", s)
	}
	return UUID{hi, lo}, nil
}

// Short returns the 16- or 32-bit form if the UUID is based on the Bluetooth base UUID.
func (u UUID) Short() (uint32, bool) {
	if u.Lo != baseLo || u.Hi&0xffffffff != baseHi {
		return 0, false
	}
	return uint32(u.Hi >> 32), true
}

func (u UUID) String() string {
	h := fmt.Sprintf("%016x%016x", u.Hi, u.Lo)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Descriptor, Characteristic and Service describe a remote GATT database. Handles are the attribute handles
// used by every later read, write and notify request.
type Descriptor struct {
	UUID   UUID
	Handle uint16
}

type Characteristic struct {
	UUID        UUID
	Handle      uint16
	Properties  uint32 // GATT characteristic properties bit field
	Descriptors []Descriptor
}

type Service struct {
	UUID            UUID
	Handle          uint16
	Characteristics []Characteristic
}

// Conn is an active connection to a peripheral.
type Conn interface {
	MTU() uint32
	Services() ([]Service, error)
	Read(handle uint16) ([]byte, error)
	Write(handle uint16, data []byte, withResponse bool) error
	ReadDescriptor(handle uint16) ([]byte, error)
	WriteDescriptor(handle uint16, data []byte) error
	// Notify enables or disables notifications (or indications) on a characteristic. Values arrive on the
	// callback given to Adapter.Connect.
	Notify(handle uint16, enable bool) error
	Disconnect() error
}

// Events reports what happens on a connection after Connect returns.
type Events struct {
	Notification func(handle uint16, data []byte)
	Disconnected func(reason error)
}

// Adapter is a local Bluetooth controller.
type Adapter interface {
	// MAC is the controller address, used as the proxy's Bluetooth MAC in device info.
	MAC() uint64
	// Scan delivers advertisements until stop is called.
	Scan(fn func(Advert)) (stop func(), err error)
	Connect(addr uint64, addrType uint32, ev Events) (Conn, error)
	// ClearCache forgets what the stack knows about a device (services, bonding where it applies).
	ClearCache(addr uint64) error
}

// ErrNotConnected is returned for requests on a closed connection.
var ErrNotConnected = errors.New("not connected")

// FormatMAC renders 0xAABBCCDDEEFF as AA:BB:CC:DD:EE:FF.
func FormatMAC(a uint64) string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X",
		byte(a>>40), byte(a>>32), byte(a>>24), byte(a>>16), byte(a>>8), byte(a))
}

// ParseMAC is the inverse of FormatMAC; it accepts lower case too.
func ParseMAC(s string) (uint64, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return 0, fmt.Errorf("bad mac %q", s)
	}
	var a uint64
	for _, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return 0, fmt.Errorf("bad mac %q", s)
		}
		a = a<<8 | v
	}
	return a, nil
}
