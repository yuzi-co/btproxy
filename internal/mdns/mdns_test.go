package mdns

import (
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
)

var (
	ifA = iface{name: "a", index: 1, ips: []net.IP{net.IPv4(10, 0, 10, 5).To4()},
		nets: []*net.IPNet{{IP: net.IPv4(10, 0, 10, 0).To4(), Mask: net.CIDRMask(24, 32)}}}
	ifB = iface{name: "b", index: 2, ips: []net.IP{net.IPv4(10, 0, 20, 5).To4()},
		nets: []*net.IPNet{{IP: net.IPv4(10, 0, 20, 0).To4(), Mask: net.CIDRMask(24, 32)}}}
)

type packet struct {
	iface string
	dst   *net.UDPAddr // nil: multicast
	ttl   uint32       // of the first answer
}

// fakeSock stands in for the socket: it records what the responder sends.
type fakeSock struct {
	mu      sync.Mutex
	pkts    []packet
	onWrite func(n int) // called with the number of packets so far, before each is recorded
}

func (f *fakeSock) write(t *testing.T) func(i iface, dst *net.UDPAddr, pkt []byte) error {
	return func(i iface, dst *net.UDPAddr, pkt []byte) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.onWrite != nil {
			f.onWrite(len(f.pkts))
		}
		_, _, a, _ := parseResponse(t, pkt)
		f.pkts = append(f.pkts, packet{iface: i.name, dst: dst, ttl: a[0].ttl})
		return nil
	}
}

func (f *fakeSock) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pkts)
}

// newTestResponder makes a responder on the given interfaces whose writes go to a fakeSock. The socket is real
// but unused, so that Close can close it.
func newTestResponder(t *testing.T, ifs ...iface) (*Responder, *fakeSock) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	r := newResponder(testSvc, logx.Discard(), conn)
	r.ifaces = ifs
	f := &fakeSock{}
	r.write = f.write(t)
	return r, f
}

func TestRateLimit(t *testing.T) {
	r, f := newTestResponder(t, ifA)
	browse := buildQuery(0, question{name: serviceType, qtype: typePTR, qclass: classIN})
	src := &net.UDPAddr{IP: net.IPv4(10, 0, 10, 20), Port: port}
	other := &net.UDPAddr{IP: net.IPv4(10, 0, 10, 21), Port: port}
	t0 := time.Now()
	expect := func(n int, what string) {
		t.Helper()
		if got := f.count(); got != n {
			t.Fatalf("%s: %d packets, want %d", what, got, n)
		}
	}
	r.handle(browse, src, t0)
	expect(1, "first query")
	r.handle(browse, src, t0.Add(500*time.Millisecond))
	r.handle(browse, other, t0.Add(999*time.Millisecond))
	expect(1, "repeats within a second, from any host on the interface")
	r.handle(browse, src, t0.Add(time.Second))
	expect(2, "a second later")
	// The services PTR was not in the browse answer, so it is not held back by it.
	r.handle(buildQuery(0, question{name: servicesName, qtype: typePTR, qclass: classIN}), src, t0.Add(1100*time.Millisecond))
	expect(3, "another record")
	// The A record went out as an additional record of the browse answer and is held back with it.
	r.handle(buildQuery(0, question{name: testSvc.host, qtype: typeA, qclass: classIN}), src, t0.Add(1100*time.Millisecond))
	expect(3, "a record sent as additional")
	// Unicast answers are limited per destination, and not by what was multicast.
	legacy := &net.UDPAddr{IP: net.IPv4(10, 0, 10, 20), Port: 40000}
	legacy2 := &net.UDPAddr{IP: net.IPv4(10, 0, 10, 20), Port: 40001}
	r.handle(browse, legacy, t0.Add(1200*time.Millisecond))
	r.handle(browse, legacy2, t0.Add(1200*time.Millisecond))
	expect(5, "legacy queries from two ports")
	r.handle(browse, legacy, t0.Add(1300*time.Millisecond))
	expect(5, "legacy query repeated")
	qu := buildQuery(0, question{name: serviceType, qtype: typePTR, qclass: classIN, unicast: true})
	r.handle(qu, src, t0.Add(1400*time.Millisecond))
	r.handle(qu, src, t0.Add(1500*time.Millisecond))
	expect(6, "QU query repeated")
	if p := f.pkts[5]; p.dst == nil || !p.dst.IP.Equal(src.IP) {
		t.Errorf("QU answer sent to %v", p.dst)
	}
	// An announcement goes out regardless; stop is closed so that announce does one round.
	close(r.stop)
	r.announce()
	expect(7, "announcement")
	if p := f.pkts[6]; p.dst != nil || p.iface != "a" || p.ttl != ttlOther {
		t.Errorf("announcement %+v", p)
	}
	// It counts as the records' last multicast.
	r.handle(browse, src, time.Now().Add(500*time.Millisecond))
	expect(7, "query after the announcement")
}

// TestCloseStopsAnnounce closes the responder while an announcement round is between interfaces: the goodbyes
// must be the last packets on every interface.
func TestCloseStopsAnnounce(t *testing.T) {
	r, f := newTestResponder(t, ifA, ifB)
	done := make(chan struct{})
	f.onWrite = func(n int) {
		if n == 0 {
			go func() {
				r.Close()
				close(done)
			}()
			<-r.stop
		}
	}
	r.announce()
	<-done
	var got []string
	for _, p := range f.pkts {
		if p.ttl == 0 {
			got = append(got, "goodbye "+p.iface)
		} else {
			got = append(got, "announce "+p.iface)
		}
	}
	if want := []string{"announce a", "goodbye a", "goodbye b"}; !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}
