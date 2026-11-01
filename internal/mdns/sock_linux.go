package mdns

import (
	"syscall"

	"golang.org/x/sys/unix" // syscall lacks SO_REUSEPORT and IP_MULTICAST_ALL on amd64, 386 and arm
)

// reuse lets the socket share port 5353 with umdns or avahi: Linux allows a second UDP bind when both sockets set
// SO_REUSEADDR, or both set SO_REUSEPORT under the same user; setting both covers either peer. Multicast is
// delivered to every socket.
func reuse(_, _ string, c syscall.RawConn) error {
	return control(c, func(fd uintptr) error {
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
			return err
		}
		return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	})
}

// setup sets the multicast TTL RFC 6762 asks for and, with IP_MULTICAST_ALL off, limits reception to the
// groups this socket joined (by default Linux delivers groups joined by any socket on any interface).
func setup(fd uintptr) error {
	if err := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL, 255); err != nil {
		return err
	}
	return syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, unix.IP_MULTICAST_ALL, 0)
}

func join(fd uintptr, i iface) error {
	m := &syscall.IPMreqn{Multiaddr: [4]byte(group.To4()), Ifindex: int32(i.index)}
	return syscall.SetsockoptIPMreqn(int(fd), syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, m)
}

func setIf(fd uintptr, i iface) error {
	m := &syscall.IPMreqn{Ifindex: int32(i.index)}
	return syscall.SetsockoptIPMreqn(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, m)
}
