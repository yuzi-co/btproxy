//go:build linux

package kernel

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A link that drops in its first seconds reports ENOTCONN after as long as it was up, and is worth retrying however
// long that took. HCI reason 0x3e has no errno of its own and reports ENOSYS within half a second.
func TestClassifyConnect(t *testing.T) {
	if err := classifyConnect(unix.ENOTCONN); !errors.Is(err, errLinkLost) {
		t.Errorf("ENOTCONN: %v", err)
	}
	var perm permanentError
	if err := classifyConnect(unix.EPERM); !errors.As(err, &perm) {
		t.Errorf("EPERM: %v", err)
	}
	if err := classifyConnect(unix.ETIMEDOUT); errors.Is(err, errLinkLost) || errors.Is(err, errRadio) || errors.As(err, &perm) {
		t.Errorf("ETIMEDOUT: %v", err)
	}
}

// The kernel names a script's process after the script, so running /etc/init.d/bluetoothd looks like bluetoothd.
// Package managers do that to stop it just as they remove it, which must not read as BlueZ being in use.
func TestIsBluetoothd(t *testing.T) {
	for _, tc := range []struct {
		name, comm, cmdline string
		want                bool
	}{
		{"daemon", "bluetoothd\n", "/usr/bin/bluetoothd\x00-n\x00", true},
		{"daemon with options", "bluetoothd\n", "/usr/libexec/bluetooth/bluetoothd\x00-E\x00", true},
		{"unreadable command line", "bluetoothd\n", "", true},
		{"OpenWrt init script", "bluetoothd\n", "/bin/sh\x00/etc/rc.common\x00/etc/init.d/bluetoothd\x00stop\x00", false},
		{"another init script path", "bluetoothd\n", "/bin/sh\x00/etc/rc.d/init.d/bluetoothd\x00stop\x00", false},
		{"another program", "btproxy\n", "/usr/bin/btproxy\x00", false},
	} {
		if got := isBluetoothd([]byte(tc.comm), []byte(tc.cmdline)); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

// mgmtPeer is a management socket whose kernel end is the test.
func mgmtPeer(t *testing.T) (*mgmtSocket, *os.File) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := newMgmtSocket(os.NewFile(uintptr(fds[0]), "mgmt"), 0, func(uint16, []byte) {})
	peer := os.NewFile(uintptr(fds[1]), "peer")
	go m.read()
	t.Cleanup(func() { peer.Close(); m.Close() })
	return m, peer
}

// complete sends Command Complete for a command, with the address its reply starts with.
func complete(t *testing.T, peer *os.File, op uint16, status byte, param []byte) {
	t.Helper()
	data := append([]byte{byte(op), byte(op >> 8), status}, param...)
	pkt := append([]byte{evCmdComplete, 0, 0, 0, byte(len(data)), 0}, data...)
	if _, err := peer.Write(pkt); err != nil {
		t.Fatal(err)
	}
}

// expectCommand waits for the next command to reach the kernel end.
func expectCommand(t *testing.T, peer *os.File) {
	t.Helper()
	if _, err := peer.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
}

// Pairings take seconds and end in any order, so a reply must reach the command for its device.
func TestMgmtRepliesMatchTheDevice(t *testing.T) {
	m, peer := mgmtPeer(t)
	devA, devB := addrInfo(0xa, bdAddrLEPublic), addrInfo(0xb, bdAddrLEPublic)
	type result struct {
		data []byte
		err  error
	}
	pair := func(addr []byte) chan result {
		ch := make(chan result, 1)
		go func() {
			d, err := m.Command(opPairDevice, append(append([]byte(nil), addr...), ioCapNoInputNoOutput), 5*time.Second)
			ch <- result{d, err}
		}()
		expectCommand(t, peer)
		return ch
	}
	a := pair(devA)
	b := pair(devB)
	complete(t, peer, opPairDevice, 5, devB) // B fails first
	if r := <-b; !isStatus(r.err, 5) {
		t.Errorf("B got %v", r.err)
	}
	select {
	case r := <-a:
		t.Fatalf("A got B's reply: %v", r.err)
	case <-time.After(50 * time.Millisecond):
	}
	complete(t, peer, opPairDevice, 0, devA)
	if r := <-a; r.err != nil || !bytes.Equal(r.data, devA) {
		t.Errorf("A got % x, %v", r.data, r.err)
	}
}

// A reply that comes after its command gave up must not answer the next command for another device.
func TestMgmtLateReplyIsDropped(t *testing.T) {
	m, peer := mgmtPeer(t)
	devA, devB := addrInfo(0xa, bdAddrLEPublic), addrInfo(0xb, bdAddrLEPublic)
	go m.Command(opDisconnect, devA, time.Millisecond)
	expectCommand(t, peer)
	time.Sleep(30 * time.Millisecond)
	ch := make(chan error, 1)
	go func() {
		_, err := m.Command(opDisconnect, devB, 5*time.Second)
		ch <- err
	}()
	expectCommand(t, peer)
	complete(t, peer, opDisconnect, 2, devA)
	select {
	case err := <-ch:
		t.Fatalf("B got A's reply: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	complete(t, peer, opDisconnect, 0, devB)
	if err := <-ch; err != nil {
		t.Error(err)
	}
}

// A refusal may come as Command Status, with no address to match: the oldest command takes it, as there is no other way.
func TestMgmtStatusWithoutAddress(t *testing.T) {
	m, peer := mgmtPeer(t)
	ch := make(chan error, 1)
	go func() {
		_, err := m.Command(opPairDevice, append(addrInfo(0xa, bdAddrLEPublic), 3), 5*time.Second)
		ch <- err
	}()
	expectCommand(t, peer)
	complete(t, peer, opPairDevice, 0x0d, nil)
	if err := <-ch; !isStatus(err, 0x0d) {
		t.Errorf("got %v", err)
	}
}

func TestBdaddr(t *testing.T) {
	if got := bdaddr(0xf424a206a947); got != [6]byte{0xf4, 0x24, 0xa2, 0x06, 0xa9, 0x47} {
		t.Errorf("% x", got)
	}
}

func TestClassifyRadioFailure(t *testing.T) {
	if err := classifyConnect(unix.ENOSYS); !errors.Is(err, errRadio) {
		t.Errorf("ENOSYS: %v", err)
	}
}
