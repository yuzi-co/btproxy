//go:build linux

package hci

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yuzi-co/btproxy/internal/ble"
)

const (
	hciDevNone = 0xffff
	rcvBuf     = 64 << 10 // kernel receive buffer; the default is several times larger
	readWake   = holdFor  // Run wakes up at least this often to release held advertisements
)

// Monitor is a read-only HCI monitor socket. It coexists with bluetoothd, which keeps driving the controller,
// and needs CAP_NET_RAW.
type Monitor struct {
	fd     int
	index  uint16
	closed atomic.Bool
}

// Open opens the monitor channel for the controller with the given index (hci0 is 0).
func Open(index uint16) (*Monitor, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return nil, fmt.Errorf("hci socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: hciDevNone, Channel: unix.HCI_CHANNEL_MONITOR}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("hci monitor: %w", err)
	}
	// The filter only saves work: without it Run skips the other packets itself.
	prog := filterProg(index)
	f := make([]unix.SockFilter, len(prog))
	for i, in := range prog {
		f[i] = unix.SockFilter{Code: in.code, Jt: in.jt, Jf: in.jf, K: in.k}
	}
	unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER,
		&unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]})
	unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, rcvBuf)
	tv := unix.NsecToTimeval(int64(readWake))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("hci monitor: %w", err)
	}
	return &Monitor{fd: fd, index: index}, nil
}

// Run reads advertisements and passes them to emit until Close is called or reading fails; it closes the
// socket when it returns. active tells whether the scan running is active (see decoder).
func (m *Monitor) Run(emit func(ble.Advert), active func() bool) error {
	defer unix.Close(m.fd)
	d := decoder{emit: emit}
	buf := make([]byte, monHdrLen+2+255+1) // one packet: header and the largest event; +1 shows truncation
	for !m.closed.Load() {
		n, err := unix.Read(m.fd, buf)
		now := time.Now()
		d.active = active()
		switch {
		case errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR):
			d.expire(now)
			continue
		case err != nil:
			return err
		case n < monHdrLen || n == len(buf):
			continue
		}
		op, idx := binary.LittleEndian.Uint16(buf), binary.LittleEndian.Uint16(buf[2:])
		if op == monEventPk && idx == m.index {
			d.event(buf[monHdrLen:n], now)
		} else {
			d.expire(now)
		}
	}
	return nil
}

// Close stops Run within readWake.
func (m *Monitor) Close() { m.closed.Store(true) }
