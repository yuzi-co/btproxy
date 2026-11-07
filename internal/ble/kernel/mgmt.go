package kernel

import (
	"errors"
	"fmt"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// Commands of the Bluetooth management interface (doc/mgmt-api.txt in BlueZ).
const (
	opReadInfo         = 0x0004
	opSetPowered       = 0x0005
	opSetBondable      = 0x0009
	opSetLE            = 0x000d
	opLoadLTKs         = 0x0013
	opDisconnect       = 0x0014
	opSetIOCap         = 0x0018
	opPairDevice       = 0x0019
	opUnpairDevice     = 0x001b
	opUserConfirm      = 0x001c
	opStartDiscovery   = 0x0023
	opStopDiscovery    = 0x0024
	opSetSecureConn    = 0x002d
	opLoadIRKs         = 0x0030
	opAddAdvMonitor    = 0x0052
	opRemoveAdvMonitor = 0x0053
)

// Events.
const (
	evCmdComplete  = 0x0001
	evCmdStatus    = 0x0002
	evIndexAdded   = 0x0004
	evIndexRemoved = 0x0005
	evNewSettings  = 0x0006
	evNewLTK       = 0x000a
	evUserConfirm  = 0x000f
	evDiscovering  = 0x0013
	evNewIRK       = 0x0018
)

// Status codes.
const (
	mgmtStatusUnknownCommand = 0x01
	mgmtStatusNotConnected   = 0x02
	mgmtStatusNotPaired      = 0x06
	mgmtStatusBusy           = 0x0a
	mgmtStatusRejected       = 0x0b
	mgmtStatusAlreadyPaired  = 0x13
)

const (
	hciDevNone = 0xffff

	// Address types as the management interface and L2CAP sockets number them, which differ from ESPHome's.
	bdAddrLEPublic = 1
	bdAddrLERandom = 2
	addrTypeLEAll  = 1<<bdAddrLEPublic | 1<<bdAddrLERandom

	ioCapNoInputNoOutput = 3

	settingPowered = 1 << 0

	addrInfoSize = 7 // struct mgmt_addr_info

	ltkSize = 36 // struct mgmt_ltk_info
	irkSize = 23 // struct mgmt_irk_info

	mgmtTimeout = 10 * time.Second
	pairTimeout = 40 * time.Second // SMP's own limit is 30 s
)

// errMgmtTimeout is a command the kernel did not answer, which means a controller that stopped responding.
var errMgmtTimeout = errors.New("management command timed out")

// MgmtError is a command the kernel refused.
type MgmtError struct {
	Op     uint16
	Status byte
}

func (e *MgmtError) Error() string {
	return fmt.Sprintf("management command %#04x: status %#02x", e.Op, e.Status)
}

func isStatus(err error, status byte) bool {
	var me *MgmtError
	return errors.As(err, &me) && me.Status == status
}

// mgmtConn is the management socket, for one controller.
type mgmtConn interface {
	// Command sends a command and returns its reply, or a *MgmtError, or errMgmtTimeout after the given time.
	Command(op uint16, param []byte, timeout time.Duration) ([]byte, error)
	Close() error
}

// addrInfo is struct mgmt_addr_info for an LE device: the address as the kernel keeps bdaddr_t, then its type.
func addrInfo(addr uint64, bdType byte) []byte {
	b := make([]byte, addrInfoSize)
	ble.PutAddrLE(b, addr)
	b[6] = bdType
	return b
}

// bdType converts ESPHome's address type to the kernel's.
func bdType(addrType uint32) byte {
	if addrType == 1 {
		return bdAddrLERandom
	}
	return bdAddrLEPublic
}

// quietMonitor is the parameter of Add Advertisement Patterns Monitor: one pattern that matches no real
// advertisement, manufacturer data under company id 0xffff, which the Bluetooth SIG keeps for tests. The proxy
// reads advertisements from the HCI monitor, so it wants nothing from this one except its effect on the kernel:
// while any monitor exists the kernel scans passively if nothing else is scanning, and turns the controller's
// duplicate filter off. Struct mgmt_adv_pattern has a value of 31 bytes whatever its length.
func quietMonitor() []byte {
	content := append([]byte{0xff, 0xff}, "btproxy:quiet"...)
	b := make([]byte, 1+3+31)
	b[0] = 1 // pattern count
	b[1] = 0xff
	b[2] = 0 // offset
	b[3] = byte(len(content))
	copy(b[4:], content)
	return b
}
