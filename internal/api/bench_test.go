package api

import (
	"net"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
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
	b.Cleanup(c.out.close)
	s.clients[c] = true
	data := []byte{0x02, 0x01, 0x06, 0x1a, 0xff, 0x4c, 0x00, 0x02, 0x15, 0xe2, 0xc5, 0x6d, 0xb5, 0xdf, 0xfb, 0x48,
		0xd2, 0xb0, 0x60, 0xd0, 0xf5, 0xa7, 0x10, 0x96, 0xe0, 0x00, 0x01, 0x00, 0x02, 0xc5}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		s.onAdvert(ble.Advert{Addr: 0xF09E4A123400 | uint64(i&0xff), AddrType: ble.AddrRandom, RSSI: -67, Data: data})
	}
}

// BenchmarkAdvertsFiltered measures an advertisement the filter drops: an allow list it does not match.
func BenchmarkAdvertsFiltered(b *testing.B) {
	f, err := ble.ParseAdvFilter("uuid:fe95,mfr:0x0499", "", false)
	if err != nil {
		b.Fatal(err)
	}
	s := NewServer(Config{Name: "bench", AdvFilter: f}, testAdapter{}, logx.Discard())
	data := []byte{0x02, 0x01, 0x06, 0x1a, 0xff, 0x4c, 0x00, 0x02, 0x15, 0xe2, 0xc5, 0x6d, 0xb5, 0xdf, 0xfb, 0x48,
		0xd2, 0xb0, 0x60, 0xd0, 0xf5, 0xa7, 0x10, 0x96, 0xe0, 0x00, 0x01, 0x00, 0x02, 0xc5}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		s.onAdvert(ble.Advert{Addr: 0xF09E4A123400 | uint64(i&0xff), AddrType: ble.AddrRandom, RSSI: -67, Data: data})
	}
	if s.pendingN != 0 {
		b.Fatal("advertisement not filtered")
	}
}

// BenchmarkDecRequest measures decoding a GATT write request, the Bluetooth request with the most fields,
// and a device request, which has the fewest.
func BenchmarkDecRequest(b *testing.B) {
	var w enc
	w.uint(1, testAddr)
	w.uint(2, 0x2a)
	w.bool(3, true)
	w.bytes(4, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	var d enc
	d.uint(1, testAddr)
	d.uint(2, reqConnectV3WithCache)
	d.uint(4, 1)
	for _, c := range []struct {
		name string
		typ  uint32
		b    []byte
	}{{"write", msgBTGATTWriteRequest, w.b}, {"device", msgBTDeviceRequest, d.b}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := decRequest(c.typ, c.b); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
