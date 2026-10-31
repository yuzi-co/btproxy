package mdns

import "syscall"

// reuse lets the socket share port 5353 with Windows' own mDNS responder and browsers.
func reuse(_, _ string, c syscall.RawConn) error {
	return control(c, func(fd uintptr) error {
		return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	})
}

func setup(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL, 255)
}

func join(fd uintptr, i iface) error {
	m := &syscall.IPMreq{Multiaddr: [4]byte(group.To4()), Interface: [4]byte(i.ips[0])}
	return syscall.SetsockoptIPMreq(syscall.Handle(fd), syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, m)
}

func setIf(fd uintptr, i iface) error {
	return syscall.SetsockoptInet4Addr(syscall.Handle(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF,
		[4]byte(i.ips[0]))
}
