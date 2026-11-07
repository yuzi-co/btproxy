// Package ble defines what the proxy needs from a Bluetooth stack. Backends: bluez (Linux, D-Bus), kernel (Linux,
// without bluetoothd), winrt (Windows) and fake (tests).
package ble

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Address types as ESPHome reports them.
const (
	AddrPublic uint32 = 0
	AddrRandom uint32 = 1
)

// Advert is one received advertisement. Data holds the raw AD structures (advertisement plus scan response),
// at most MaxAdvert bytes. Data is only valid during the call to the scan callback that receives it: backends
// may reuse the buffer for the next advertisement, so a callback that keeps the data copies it.
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

// ShortUUID is the inverse of Short: the 128-bit form of a 16- or 32-bit UUID on the Bluetooth base UUID.
func ShortUUID(v uint32) UUID {
	return UUID{Hi: uint64(v)<<32 | baseHi, Lo: baseLo}
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

// UUIDFromLE reads a UUID in the order ATT and AD structures carry it, least significant byte first: 2 bytes
// for a 16-bit UUID, 4 for a 32-bit one, 16 otherwise.
func UUIDFromLE(b []byte) UUID {
	switch len(b) {
	case 2:
		return ShortUUID(uint32(binary.LittleEndian.Uint16(b)))
	case 4:
		return ShortUUID(binary.LittleEndian.Uint32(b))
	}
	return UUID{Hi: binary.LittleEndian.Uint64(b[8:16]), Lo: binary.LittleEndian.Uint64(b[:8])}
}

// AppendLE appends the 16 bytes of the UUID in the order UUIDFromLE reads them.
func (u UUID) AppendLE(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, u.Lo)
	return binary.LittleEndian.AppendUint64(dst, u.Hi)
}

// AddrFromLE reads a device address as HCI and the kernel carry it (bdaddr_t), least significant byte first,
// into the form ble uses: 0xAABBCCDDEEFF for AA:BB:CC:DD:EE:FF.
func AddrFromLE(b []byte) uint64 {
	return uint64(b[5])<<40 | uint64(b[4])<<32 | uint64(b[3])<<24 | uint64(b[2])<<16 | uint64(b[1])<<8 | uint64(b[0])
}

// PutAddrLE writes a device address into b[:6] in the order AddrFromLE reads it.
func PutAddrLE(b []byte, a uint64) {
	_ = b[5]
	for i := range 6 {
		b[i] = byte(a >> (8 * i))
	}
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
	// Pair bonds with the device. A device that is already paired succeeds at once.
	Pair() error
	Disconnect() error
}

// Events reports what happens on a connection after Connect returns.
type Events struct {
	Notification func(handle uint16, data []byte)
	Disconnected func(reason error)
	// Withdrawn, if set, reports whether the connect is no longer wanted (a disconnect was requested, or
	// every client left). A backend that retries a failed connect asks it before each retry.
	Withdrawn func() bool
}

// Adapter is a local Bluetooth controller.
type Adapter interface {
	// MAC is the controller address, used as the proxy's Bluetooth MAC in device info.
	MAC() uint64
	// Scan delivers advertisements until stop is called. fn must not keep Advert.Data after it returns.
	Scan(fn func(Advert)) (stop func(), err error)
	Connect(addr uint64, addrType uint32, ev Events) (Conn, error)
	// ClearCache forgets the services the stack has cached for a device. Bonds are kept.
	ClearCache(addr uint64) error
	// Unpair removes the bond with a device, connected or not, and drops any connection to it without
	// calling Events.Disconnected.
	Unpair(addr uint64) error
}

// ConnectOptions configures Adapter.Connect the same way on every backend.
type ConnectOptions struct {
	// ConnectRetries is how often a connect that failed on the radio or lost the link early is retried.
	ConnectRetries int
	// ConnectRetryDelay is the wait before the first retry; each later one waits twice the previous (see
	// ConnectRetry). 0 retries at once.
	ConnectRetryDelay time.Duration
	// PauseOnConnect stops scanning while a connect is in progress.
	PauseOnConnect bool
}

// WatchdogOptions configures the scan watchdog of the backends that have one.
type WatchdogOptions struct {
	// StallTimeout is how long scanning may deliver nothing before the watchdog tries to bring it back; 0 never.
	StallTimeout time.Duration
	// USBReset lets the watchdog reset the adapter's USB device when nothing else brought scanning back.
	USBReset bool
}

// ConnParams are LE connection parameters in the units of the HCI LE Connection Update command: intervals in
// 1.25 ms, supervision timeout in 10 ms, latency in connection events the peripheral may skip.
type ConnParams struct {
	MinInterval, MaxInterval, Latency, Timeout uint16
}

// ConnParamsSetter is implemented by adapters that can change the parameters of a live connection.
type ConnParamsSetter interface {
	// SetConnParams asks the controller to update the connection to addr and returns once the controller
	// has accepted or refused the request; the new parameters take effect some connection events later.
	SetConnParams(addr uint64, p ConnParams) error
}

// ConnParamsSupport is implemented by ConnParamsSetters that only know at run time whether they can change
// connection parameters (the Windows backend needs Windows 11). Without it, a ConnParamsSetter always can.
type ConnParamsSupport interface {
	ConnParamsSupported() bool
}

// CanSetConnParams returns the adapter's ConnParamsSetter if it has one that works here.
func CanSetConnParams(a Adapter) (ConnParamsSetter, bool) {
	cs, ok := a.(ConnParamsSetter)
	if s, has := a.(ConnParamsSupport); ok && has && !s.ConnParamsSupported() {
		return nil, false
	}
	return cs, ok
}

// RestartCounter is implemented by adapters whose watchdog restarts the Bluetooth stack, for the proxy's
// diagnostic sensors.
type RestartCounter interface {
	// Restarts is how many times the stack was restarted since the adapter was opened.
	Restarts() uint64
}

// RecoveryCounter is implemented by adapters whose watchdog can do more than restart the Bluetooth stack, for
// the proxy's diagnostic sensors.
type RecoveryCounter interface {
	// Recoveries is how many times the adapter was power-cycled or reset since it was opened.
	Recoveries() uint64
}

// StackRestarter is implemented by adapters that can restart their Bluetooth stack on request.
type StackRestarter interface {
	// StackRestartable tells whether a restart is possible here, such as with a command configured.
	StackRestartable() bool
	// RestartStack restarts the stack and returns once that is done or refused.
	RestartStack() error
}

// AdvertSourcer is implemented by adapters that have more than one source of advertisements.
type AdvertSourcer interface {
	// AdvertSource names where advertisements come from now, for a person to read.
	AdvertSource() string
}

// ScanSettinger is implemented by adapters that can fall back from the scan mode asked for.
type ScanSettinger interface {
	// ScanSetting names how the adapter scans, for a person to read.
	ScanSetting() string
}

var (
	// ErrNotConnected is returned for requests on a closed connection.
	ErrNotConnected = errors.New("not connected")
	// ErrLinkLost is a link that went down on its own: Events.Disconnected reports it, and Connect fails with
	// it (wrapped with where it happened) when the link drops before the connection is handed over.
	ErrLinkLost = errors.New("link lost")
)

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
