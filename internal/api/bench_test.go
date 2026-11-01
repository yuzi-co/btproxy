package api

import (
	"net"
	"testing"
	"time"

	"btproxy/internal/ble"
	"btproxy/internal/logx"
)

// discardConn is a net.Conn that accepts every write at once.
type discardConn struct{ net.Conn }

func (discardConn) Write(b []byte) (int, error)      { return len(b), nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }
func (discardConn) Close() error                     { return nil }

// BenchmarkAdverts measures the advertisement path from the scan callback to the client's socket, with one
// subscribed plaintext client.
func BenchmarkAdverts(b *testing.B) {
	s := NewServer(Config{Name: "bench"}, testAdapter{}, logx.Discard())
	c := newClient(s, discardConn{}, s.log)
	c.advSubscribed = true
	b.Cleanup(c.closeQueue)
	s.clients[c] = true
	data := []byte{0x02, 0x01, 0x06, 0x1a, 0xff, 0x4c, 0x00, 0x02, 0x15, 0xe2, 0xc5, 0x6d, 0xb5, 0xdf, 0xfb, 0x48,
		0xd2, 0xb0, 0x60, 0xd0, 0xf5, 0xa7, 0x10, 0x96, 0xe0, 0x00, 0x01, 0x00, 0x02, 0xc5}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		s.onAdvert(ble.Advert{Addr: 0xF09E4A123400 | uint64(i&0xff), AddrType: ble.AddrRandom, RSSI: -67, Data: data})
	}
}
