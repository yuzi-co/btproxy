package mdns

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"btproxy/internal/logx"
)

// TestCoexist binds the way avahi and umdns do (SO_REUSEADDR, plus SO_REUSEPORT for umdns) before our socket.
func TestCoexist(t *testing.T) {
	for name, opts := range map[string][]int{
		"reuseaddr":           {syscall.SO_REUSEADDR},
		"reuseaddr+reuseport": {syscall.SO_REUSEADDR, unix.SO_REUSEPORT},
	} {
		t.Run(name, func(t *testing.T) {
			other := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
				return control(c, func(fd uintptr) error {
					for _, o := range opts {
						if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, o, 1); err != nil {
							return err
						}
					}
					return nil
				})
			}}
			a, err := other.ListenPacket(context.Background(), "udp4", ":15353")
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			b, err := (&net.ListenConfig{Control: reuse}).ListenPacket(context.Background(), "udp4", ":15353")
			if err != nil {
				t.Fatalf("second bind: %v", err)
			}
			b.Close()
		})
	}
}

// TestResponder runs the real responder and resolves it with a legacy unicast query to the group.
func TestResponder(t *testing.T) {
	ifs := scan(nil)
	if len(ifs) == 0 {
		t.Skip("no multicast interface with IPv4")
	}
	r, err := Start(Config{Name: "btproxy-test", Port: 6053, TXT: []string{"mac=0123456789ab"}},
		logx.Discard())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ifs[0].ips[0]})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	q := buildQuery(7, question{name: "btproxy-test.local.", qtype: typeA, qclass: classIN})
	if _, err := c.WriteToUDP(q, &net.UDPAddr{IP: group, Port: port}); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	id, _, a, _ := parseResponse(t, buf[:n])
	if id != 7 || len(a) == 0 || !net.IP(a[0].data).Equal(ifs[0].ips[0]) {
		t.Errorf("id %d answers %+v, want %v", id, a, ifs[0].ips[0])
	}
}
