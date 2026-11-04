//go:build linux

package hci

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	monCtrlOpen = 14 // HCI_MON_CTRL_OPEN

	ctrlOpenFixed = 14   // cookie 4, format 2, version 3, flags 4, name length 1
	stackScanMax  = 1024 // packets to read, in case live traffic never lets the socket run dry
)

// StackRunning tells whether bluetoothd holds a control socket. The kernel lists the open control sockets for each
// new monitor, with the name of the program that opened them. It keeps one list for the whole system, so this also
// sees a bluetoothd in another PID namespace, such as on the host of a container. It needs CAP_NET_RAW.
func StackRunning() (bool, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.BTPROTO_HCI)
	if err != nil {
		return false, fmt.Errorf("hci socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: hciDevNone, Channel: unix.HCI_CHANNEL_MONITOR}); err != nil {
		return false, fmt.Errorf("hci monitor: %w", err)
	}
	// The list is queued before bind returns, and ahead of any live traffic.
	buf := make([]byte, monHdrLen+2+255+1)
	for range stackScanMax {
		n, err := unix.Read(fd, buf)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EAGAIN):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("hci monitor: %w", err)
		}
		if controlOwner(buf[:n]) == "bluetoothd" {
			return true, nil
		}
	}
	return false, nil
}

// controlOwner returns the name of the program in a monitor packet about a newly listed control socket, or "".
func controlOwner(pkt []byte) string {
	if len(pkt) < monHdrLen+ctrlOpenFixed || binary.LittleEndian.Uint16(pkt) != monCtrlOpen {
		return ""
	}
	p := pkt[monHdrLen:]
	size := int(p[ctrlOpenFixed-1])
	if len(p) < ctrlOpenFixed+size {
		return ""
	}
	name := p[ctrlOpenFixed : ctrlOpenFixed+size]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	return string(name)
}
