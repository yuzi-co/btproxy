//go:build linux

package hci

// Connection parameter updates. Neither BlueZ's D-Bus API nor the management interface changes a live connection:
// MGMT_OP_LOAD_CONN_PARAM only loads parameters for later ones, and also replaces the list bluetoothd keeps there.
// So the proxy sends the HCI LE Connection Update command itself on a raw HCI socket, as `hcitool lecup` does. That
// changes the live connection like esp_ble_gap_update_conn_params on an ESP32. The kernel keeps owning the
// connection and sees the controller's events as for its own commands. Sending the command needs CAP_NET_RAW.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

const (
	hciCommandPkt        = 0x01
	hciEventPkt          = 0x04
	hciFilterOpt         = 2 // HCI_FILTER, at level SOL_HCI
	evCmdStatus          = 0x0f
	evLEMeta             = 0x3e
	leConnUpdateComplete = 0x03   // LE Meta subevent
	opLEConnUpdate       = 0x2013 // OGF 0x08 (LE), OCF 0x0013
	leLink               = 0x80   // LE_LINK, the connection type HCIGETCONNINFO looks up

	cmdStatusTimeout  = 2 * time.Second  // the controller answers at once
	completionTimeout = 30 * time.Second // only for the log: the update applies some connection events later
)

// hciGetConnInfo is HCIGETCONNINFO, _IOR('H', 213, int). MIPS and PowerPC put the direction bits elsewhere.
func hciGetConnInfo() uintptr {
	switch runtime.GOARCH {
	case "mips", "mipsle", "mips64", "mips64le", "ppc64", "ppc64le":
		return 0x400448d5
	}
	return 0x800448d5
}

// ConnUpdate sends LE Connection Update for the connection to addr on the controller with the given index, and waits
// for the controller's Command Status. The completion event is only logged.
func ConnUpdate(index uint16, addr uint64, p ble.ConnParams, log *logx.Logger) error {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.BTPROTO_HCI)
	if err != nil {
		return fmt.Errorf("HCI socket: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			unix.Close(fd)
		}
	}()
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: index, Channel: unix.HCI_CHANNEL_RAW}); err != nil {
		return fmt.Errorf("bind HCI socket: %w", err)
	}
	handle, err := connHandle(fd, addr)
	if err != nil {
		return err
	}
	if err := unix.SetsockoptString(fd, unix.SOL_HCI, hciFilterOpt, string(hciFilter())); err != nil {
		return fmt.Errorf("HCI filter: %w", err)
	}
	if _, err := unix.Write(fd, connUpdateCmd(handle, p)); err != nil {
		if errors.Is(err, unix.EPERM) {
			err = fmt.Errorf("%w (needs CAP_NET_RAW)", err)
		}
		return fmt.Errorf("send LE Connection Update: %w", err)
	}
	buf := make([]byte, 260)
	deadline := time.Now().Add(cmdStatusTimeout)
	for {
		n, err := readUntil(fd, buf, deadline)
		if err != nil {
			return fmt.Errorf("waiting for the controller: %w", err)
		}
		if st, isStatus := cmdStatus(buf[:n]); isStatus {
			if st != 0 {
				return fmt.Errorf("controller refused LE Connection Update: HCI status %#02x", st)
			}
			break
		}
	}
	ok = true
	go func() {
		defer unix.Close(fd)
		deadline := time.Now().Add(completionTimeout)
		for {
			n, err := readUntil(fd, buf, deadline)
			if err != nil {
				log.Debug("no connection update completion", "addr", ble.FormatMAC(addr), "err", err)
				return
			}
			if st, got, done := connUpdateDone(buf[:n], handle); done {
				if st != 0 {
					log.Warn("connection update failed", "addr", ble.FormatMAC(addr), "status", st)
				} else {
					log.Info("connection updated", "addr", ble.FormatMAC(addr), "interval", got.MaxInterval,
						"latency", got.Latency, "timeout", got.Timeout)
				}
				return
			}
		}
	}()
	return nil
}

// connHandle looks up the controller's handle for the LE connection to addr.
func connHandle(fd int, addr uint64) (uint16, error) {
	// struct hci_conn_info_req (bdaddr, type, padded to 8) followed by struct hci_conn_info, which starts
	// with the handle.
	var req [8 + 16]byte
	binary.LittleEndian.PutUint64(req[:], addr) // bdaddr_t is little endian; byte 6 is the type
	req[6], req[7] = leLink, 0
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), hciGetConnInfo(), uintptr(unsafe.Pointer(&req[0])))
	if errno == unix.ENOENT {
		return 0, fmt.Errorf("no LE connection to %s in the kernel", ble.FormatMAC(addr))
	}
	if errno != 0 {
		return 0, fmt.Errorf("HCIGETCONNINFO: %w", errno)
	}
	return binary.NativeEndian.Uint16(req[8:]), nil
}

// hciFilter is struct hci_ufilter: HCI events only, Command Status and LE Meta, Command Status for LE
// Connection Update only.
func hciFilter() []byte {
	b := make([]byte, 14)
	binary.NativeEndian.PutUint32(b[0:], 1<<hciEventPkt)
	binary.NativeEndian.PutUint32(b[4:], 1<<evCmdStatus)
	binary.NativeEndian.PutUint32(b[8:], 1<<(evLEMeta-32))
	binary.LittleEndian.PutUint16(b[12:], opLEConnUpdate)
	return b
}

// connUpdateCmd builds the HCI LE Connection Update command packet; the CE lengths are left 0.
func connUpdateCmd(handle uint16, p ble.ConnParams) []byte {
	b := []byte{hciCommandPkt, opLEConnUpdate & 0xff, opLEConnUpdate >> 8, 14}
	for _, v := range []uint16{handle, p.MinInterval, p.MaxInterval, p.Latency, p.Timeout, 0, 0} {
		b = binary.LittleEndian.AppendUint16(b, v)
	}
	return b
}

// cmdStatus parses a Command Status event for LE Connection Update.
func cmdStatus(b []byte) (status byte, ok bool) {
	if len(b) < 7 || b[0] != hciEventPkt || b[1] != evCmdStatus ||
		binary.LittleEndian.Uint16(b[5:]) != opLEConnUpdate {
		return 0, false
	}
	return b[3], true
}

// connUpdateDone parses an LE Connection Update Complete event for handle. The parameters in effect come
// back with the interval in MinInterval and MaxInterval.
func connUpdateDone(b []byte, handle uint16) (status byte, p ble.ConnParams, ok bool) {
	if len(b) < 13 || b[0] != hciEventPkt || b[1] != evLEMeta || b[3] != leConnUpdateComplete ||
		binary.LittleEndian.Uint16(b[5:])&0x0fff != handle {
		return 0, p, false
	}
	iv := binary.LittleEndian.Uint16(b[7:])
	p = ble.ConnParams{MinInterval: iv, MaxInterval: iv, Latency: binary.LittleEndian.Uint16(b[9:]),
		Timeout: binary.LittleEndian.Uint16(b[11:])}
	return b[4], p, true
}

// readUntil reads one packet, giving up at deadline.
func readUntil(fd int, buf []byte, deadline time.Time) (int, error) {
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return 0, errors.New("timed out")
		}
		tv := unix.NsecToTimeval(left.Nanoseconds())
		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
			return 0, err
		}
		n, err := unix.Read(fd, buf)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		return n, err
	}
}
