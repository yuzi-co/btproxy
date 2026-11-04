//go:build linux

package kernel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/hci"
)

const (
	attCID = 4 // the fixed L2CAP channel of ATT

	// From <bluetooth/bluetooth.h>.
	solBluetooth   = 274
	btSecurity     = 4
	btSecurityMed  = 2
	connectTimeout = 30 * time.Second
	secureTimeout  = 40 * time.Second
	pollStep       = 200 // ms
)

// Open opens the controller named like hci0. It fails if bluetoothd is running: two stacks driving one controller
// undo each other's work.
func Open(name string, opt Options) (*Adapter, error) {
	if opt.USBReset {
		opt.resetUSB = hci.ResetUSB
	}
	if opt.BlueZDir == "" {
		opt.BlueZDir = "/var/lib/bluetooth"
	}
	return open(name, opt, linuxSystem{})
}

type linuxSystem struct{}

func (linuxSystem) openMonitor(index uint16) (advSource, error) { return hci.Open(index) }

// stackRunning looks for bluetoothd among the processes. It may be in another namespace and not visible here,
// as with a container, in which case the two will conflict: the documentation says so.
func (linuxSystem) stackRunning() bool {
	comms, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, p := range comms {
		if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == "bluetoothd" {
			return true
		}
	}
	return false
}

func (linuxSystem) openMgmt(index uint16, onEvent func(uint16, []byte)) (mgmtConn, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.BTPROTO_HCI)
	if err != nil {
		return nil, explainNoBluetooth(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrHCI{Dev: hciDevNone, Channel: unix.HCI_CHANNEL_CONTROL}); err != nil {
		unix.Close(fd)
		if errors.Is(err, unix.EPERM) {
			err = fmt.Errorf("%w (needs CAP_NET_ADMIN)", err)
		}
		return nil, err
	}
	m := &mgmtSocket{f: os.NewFile(uintptr(fd), "mgmt"), index: index, onEvent: onEvent, waiters: map[uint16][]chan mgmtReply{}}
	go m.read()
	return m, nil
}

func explainNoBluetooth(err error) error {
	if errors.Is(err, unix.EAFNOSUPPORT) {
		return fmt.Errorf("%w (the kernel has no Bluetooth support loaded)", err)
	}
	return err
}

type mgmtReply struct {
	data []byte
	err  error
}

// mgmtSocket is the management socket, used for one controller. Commands may be in progress together, as a
// pairing takes many seconds, so replies are matched to commands by opcode, first come first served.
type mgmtSocket struct {
	f       *os.File
	index   uint16
	onEvent func(uint16, []byte)

	mu      sync.Mutex
	waiters map[uint16][]chan mgmtReply
}

func (m *mgmtSocket) Close() error { return m.f.Close() }

func (m *mgmtSocket) Command(op uint16, param []byte, timeout time.Duration) ([]byte, error) {
	b := binary.LittleEndian.AppendUint16(nil, op)
	b = binary.LittleEndian.AppendUint16(b, m.index)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(param)))
	b = append(b, param...)
	ch := make(chan mgmtReply, 1)
	m.mu.Lock()
	m.waiters[op] = append(m.waiters[op], ch)
	_, err := m.f.Write(b)
	if err != nil {
		q := m.waiters[op]
		m.waiters[op] = q[:len(q)-1]
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-t.C:
		m.mu.Lock()
		q := m.waiters[op]
		for i, w := range q {
			if w == ch {
				m.waiters[op] = append(q[:i:i], q[i+1:]...)
				break
			}
		}
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %#04x", errMgmtTimeout, op)
	}
}

func (m *mgmtSocket) read() {
	buf := make([]byte, 4096)
	for {
		n, err := m.f.Read(buf)
		if err != nil {
			return
		}
		if n < 6 {
			continue
		}
		op := binary.LittleEndian.Uint16(buf)
		idx := binary.LittleEndian.Uint16(buf[2:])
		size := int(binary.LittleEndian.Uint16(buf[4:]))
		if 6+size > n || (idx != m.index && idx != hciDevNone) {
			continue
		}
		data := buf[6 : 6+size]
		switch op {
		case evCmdComplete, evCmdStatus:
			if size < 3 {
				continue
			}
			cmd := binary.LittleEndian.Uint16(data)
			r := mgmtReply{data: append([]byte(nil), data[3:]...)}
			if st := data[2]; st != 0 {
				r = mgmtReply{err: &MgmtError{Op: cmd, Status: st}}
			}
			m.mu.Lock()
			if q := m.waiters[cmd]; len(q) > 0 {
				q[0] <- r
				m.waiters[cmd] = q[1:]
			}
			m.mu.Unlock()
		default:
			m.onEvent(op, data)
		}
	}
}

// attSocket is a connected L2CAP socket on the ATT channel, on the runtime's poller so that Close ends a Read.
type attSocket struct{ f *os.File }

func (s *attSocket) Read(b []byte) (int, error)  { return s.f.Read(b) }
func (s *attSocket) Write(b []byte) (int, error) { return s.f.Write(b) }
func (s *attSocket) Close() error                { return s.f.Close() }

// Secure implements socket. Asking for a security level on a live ATT channel makes the kernel encrypt the link
// and pair if it has no key. The socket cannot send until that ends, which poll reports as writable.
func (s *attSocket) Secure() error {
	raw, err := s.f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	cerr := raw.Control(func(fd uintptr) {
		if opErr = setSecurity(int(fd)); opErr != nil {
			return
		}
		deadline := time.Now().Add(secureTimeout)
		for time.Now().Before(deadline) {
			p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
			n, err := unix.Poll(p, pollStep)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				opErr = err
				return
			}
			if n > 0 {
				if p[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
					opErr = ble.ErrNotConnected
				}
				return
			}
		}
		opErr = errors.New("timed out encrypting the link")
	})
	if cerr != nil {
		return cerr
	}
	return opErr
}

func setSecurity(fd int) error {
	// struct bt_security is the level and a key size, 0 for no requirement.
	return unix.SetsockoptString(fd, solBluetooth, btSecurity, string([]byte{btSecurityMed, 0}))
}

func (linuxSystem) dialATT(local, addr uint64, addrType uint32, secure bool, withdrawn func() bool) (socket, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.BTPROTO_L2CAP)
	if err != nil {
		return nil, errPermanent(explainNoBluetooth(err).Error())
	}
	ok := false
	defer func() {
		if !ok {
			unix.Close(fd)
		}
	}()
	// Binding to the controller's address picks the controller. Its address type does not affect that, and a
	// socket bound as LE takes the LE path in connect.
	if err := unix.Bind(fd, &unix.SockaddrL2{CID: attCID, Addr: bdaddr(local), AddrType: bdAddrLEPublic}); err != nil {
		return nil, errPermanent("bind: " + err.Error())
	}
	if secure {
		if err := setSecurity(fd); err != nil {
			return nil, errPermanent("security level: " + err.Error())
		}
	}
	err = unix.Connect(fd, &unix.SockaddrL2{CID: attCID, Addr: bdaddr(addr), AddrType: bdType(addrType)})
	if err != nil && !errors.Is(err, unix.EINPROGRESS) {
		return nil, classifyConnect(err)
	}
	deadline := time.Now().Add(connectTimeout)
	for {
		if withdrawn != nil && withdrawn() {
			return nil, errors.New("connect withdrawn")
		}
		if time.Now().After(deadline) {
			return nil, unix.ETIMEDOUT
		}
		p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		n, err := unix.Poll(p, pollStep)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n > 0 {
			break
		}
	}
	soErr, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
	if err != nil {
		return nil, err
	}
	if soErr != 0 {
		return nil, classifyConnect(unix.Errno(soErr))
	}
	ok = true
	return &attSocket{f: os.NewFile(uintptr(fd), "att")}, nil
}

// classifyConnect marks a link that came up and dropped, and the errors another attempt will not change.
func classifyConnect(err error) error {
	switch {
	case errors.Is(err, unix.ENOTCONN):
		return fmt.Errorf("%w: %v", errLinkLost, err)
	case errors.Is(err, unix.ENOSYS): // the kernel's bt_to_errno has nothing for reason 0x3e
		return errRadio
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES), errors.Is(err, unix.ENODEV),
		errors.Is(err, unix.ENETDOWN), errors.Is(err, unix.EHOSTUNREACH), errors.Is(err, unix.EOPNOTSUPP):
		return errPermanent(err.Error())
	}
	return err
}

// bdaddr is an address as unix.SockaddrL2 takes it, which reverses it into bdaddr_t.
func bdaddr(addr uint64) [6]byte {
	var a [6]byte
	for i := range 6 {
		a[i] = byte(addr >> (8 * (5 - i)))
	}
	return a
}

var _ ble.ConnParamsSetter = (*Adapter)(nil)

// SetConnParams implements ble.ConnParamsSetter.
func (a *Adapter) SetConnParams(addr uint64, p ble.ConnParams) error {
	return hci.ConnUpdate(a.index, addr, p, a.log)
}
