package kernel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// attr is one attribute of the fake peripheral. A nil uuid128 means the 16-bit uuid.
type attr struct {
	handle  uint16
	uuid    uint16
	uuid128 []byte
	value   []byte
	end     uint16 // group end, for service declarations
	needEnc bool   // reads and writes fail with insufficient encryption until the link is encrypted
}

// peripheral is a GATT server speaking ATT over a fake transport, with the paging a real one does: responses
// never exceed the negotiated MTU.
type peripheral struct {
	mu        sync.Mutex
	attrs     []*attr
	mtu       int
	maxMTU    int
	encrypted bool
	written   map[uint16][]byte
	prepared  []byte
	requests  []byte // opcodes received, in order
	confirms  int
	silent    bool // never answer: for the request timeout

	noSecondary    bool // refuses secondary service discovery with Unsupported Group Type
	prepareFailAt  int  // the nth Prepare Write, from 1, fails with Prepare Queue Full
	prepareCorrupt bool // Prepare Write responses echo a changed value
	prepares       int

	in     chan []byte // towards the client
	closed chan struct{}
	once   sync.Once
}

func newPeripheral(attrs ...*attr) *peripheral {
	return &peripheral{attrs: attrs, mtu: 23, maxMTU: 247, written: map[uint16][]byte{},
		in: make(chan []byte, 16), closed: make(chan struct{})}
}

// The client's end of the transport.
func (p *peripheral) Read(b []byte) (int, error) {
	select {
	case pdu := <-p.in:
		return copy(b, pdu), nil
	case <-p.closed:
		return 0, io.EOF
	}
}

func (p *peripheral) Write(b []byte) (int, error) {
	select {
	case <-p.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	rsp := p.handle(append([]byte(nil), b...))
	if rsp != nil {
		p.push(rsp)
	}
	return len(b), nil
}

func (p *peripheral) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func (p *peripheral) push(pdu []byte) {
	select {
	case p.in <- pdu:
	case <-p.closed:
	}
}

func attErrorPDU(op byte, h uint16, code byte) []byte {
	return []byte{opError, op, byte(h), byte(h >> 8), code}
}

func (p *peripheral) find(h uint16) *attr {
	for _, a := range p.attrs {
		if a.handle == h {
			return a
		}
	}
	return nil
}

func (a *attr) uuidBytes() []byte {
	if a.uuid128 != nil {
		return a.uuid128
	}
	return []byte{byte(a.uuid), byte(a.uuid >> 8)}
}

func (p *peripheral) handle(b []byte) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	op := b[0]
	if op == opConfirm {
		p.confirms++
		return nil
	}
	p.requests = append(p.requests, op)
	if p.silent {
		return nil
	}
	u16 := func(i int) uint16 { return binary.LittleEndian.Uint16(b[i:]) }
	switch op {
	case opMTUReq:
		p.mtu = min(int(u16(1)), p.maxMTU)
		return binary.LittleEndian.AppendUint16([]byte{opMTURsp}, uint16(p.maxMTU))
	case opReadGroupReq:
		if p.noSecondary && u16(5) == uuidSecondary {
			return attErrorPDU(op, u16(1), attErrUnsupportedType)
		}
		var rsp []byte
		var size int
		for _, a := range p.attrs {
			if a.handle < u16(1) || a.handle > u16(3) || a.uuid != u16(5) {
				continue
			}
			entry := binary.LittleEndian.AppendUint16(nil, a.handle)
			entry = binary.LittleEndian.AppendUint16(entry, a.end)
			entry = append(entry, a.value...)
			if size == 0 {
				size = len(entry)
				rsp = []byte{opReadGroupRsp, byte(size)}
			}
			if len(entry) != size || len(rsp)+size > p.mtu {
				break
			}
			rsp = append(rsp, entry...)
		}
		if rsp == nil {
			return attErrorPDU(op, u16(1), attErrNotFound)
		}
		return rsp
	case opReadTypeReq:
		var rsp []byte
		var size int
		for _, a := range p.attrs {
			if a.handle < u16(1) || a.handle > u16(3) || a.uuid != u16(5) {
				continue
			}
			entry := append(binary.LittleEndian.AppendUint16(nil, a.handle), a.value...)
			if size == 0 {
				size = len(entry)
				rsp = []byte{opReadTypeRsp, byte(size)}
			}
			if len(entry) != size || len(rsp)+size > p.mtu {
				break
			}
			rsp = append(rsp, entry...)
		}
		if rsp == nil {
			return attErrorPDU(op, u16(1), attErrNotFound)
		}
		return rsp
	case opFindInfoReq:
		var rsp []byte
		var size int
		for _, a := range p.attrs {
			if a.handle < u16(1) || a.handle > u16(3) {
				continue
			}
			entry := append(binary.LittleEndian.AppendUint16(nil, a.handle), a.uuidBytes()...)
			if size == 0 {
				size = len(entry)
				format := byte(1)
				if size == 18 {
					format = 2
				}
				rsp = []byte{opFindInfoRsp, format}
			}
			if len(entry) != size || len(rsp)+size > p.mtu {
				break
			}
			rsp = append(rsp, entry...)
		}
		if rsp == nil {
			return attErrorPDU(op, u16(1), attErrNotFound)
		}
		return rsp
	case opReadReq, opReadBlobReq:
		a := p.find(u16(1))
		if a == nil {
			return attErrorPDU(op, u16(1), attErrInvalidHandle)
		}
		if a.needEnc && !p.encrypted {
			return attErrorPDU(op, a.handle, attErrInsufficientEnc)
		}
		off := 0
		rspOp := byte(opReadRsp)
		if op == opReadBlobReq {
			off, rspOp = int(u16(3)), opReadBlobRsp
			if len(a.value) <= p.mtu-1 {
				return attErrorPDU(op, a.handle, attErrNotLong)
			}
		}
		if off > len(a.value) {
			return attErrorPDU(op, a.handle, attErrInvalidOffset)
		}
		v := a.value[off:]
		return append([]byte{rspOp}, v[:min(len(v), p.mtu-1)]...)
	case opWriteReq:
		a := p.find(u16(1))
		if a == nil {
			return attErrorPDU(op, u16(1), attErrInvalidHandle)
		}
		if a.needEnc && !p.encrypted {
			return attErrorPDU(op, a.handle, attErrInsufficientEnc)
		}
		p.written[a.handle] = append([]byte(nil), b[3:]...)
		return []byte{opWriteRsp}
	case opWriteCmd:
		p.written[u16(1)] = append([]byte(nil), b[3:]...)
		return nil
	case opPrepareReq:
		p.prepares++
		if p.prepares == p.prepareFailAt {
			return attErrorPDU(op, u16(1), 0x09)
		}
		p.prepared = append(p.prepared, b[5:]...)
		rsp := append([]byte{opPrepareRsp}, b[1:]...)
		if p.prepareCorrupt {
			rsp[len(rsp)-1] ^= 0xff
		}
		return rsp
	case opExecuteReq:
		if b[1] == 1 {
			p.written[1000] = p.prepared
		}
		p.prepared = nil
		return []byte{opExecuteRsp}
	}
	return attErrorPDU(op, 0, attErrNotSupported)
}

func (p *peripheral) ops() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.requests...)
}

func (p *peripheral) wrote(h uint16) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written[h]
}

func svc(h, end, uuid uint16) *attr {
	return &attr{handle: h, uuid: 0x2800, end: end, value: []byte{byte(uuid), byte(uuid >> 8)}}
}

func charDecl(h uint16, props byte, uuid uint16) *attr {
	return &attr{handle: h, uuid: 0x2803, value: []byte{props, byte(h + 1), byte((h + 1) >> 8), byte(uuid), byte(uuid >> 8)}}
}

func short(s uint16) ble.UUID {
	u, err := ble.ParseUUID("0000" + hex4(s) + "-0000-1000-8000-00805f9b34fb")
	if err != nil {
		panic(err)
	}
	return u
}

func hex4(s uint16) string {
	const d = "0123456789abcdef"
	return string([]byte{d[s>>12], d[s>>8&15], d[s>>4&15], d[s&15]})
}

// heartRate is a small database: a service with a notifying characteristic and its CCCD, a second
// characteristic that may only be read over an encrypted link, and a service with a 128-bit UUID.
func heartRate() *peripheral {
	uuid128 := []byte{0xfb, 0x34, 0x9b, 0x5f, 0x80, 0, 0, 0x80, 0, 0x10, 0, 0, 0x34, 0x12, 0xcd, 0xab}
	long := append([]byte(nil), uuid128...)
	return newPeripheral(
		svc(1, 7, 0x180d),
		charDecl(2, 0x10, 0x2a37), &attr{handle: 3, uuid: 0x2a37, value: []byte{0, 60}},
		&attr{handle: 4, uuid: 0x2902, value: []byte{0, 0}},
		&attr{handle: 5, uuid: 0x2901, value: []byte("rate")},
		charDecl(6, 0x02, 0x2a38), &attr{handle: 7, uuid: 0x2a38, value: []byte{1}, needEnc: true},
		&attr{handle: 8, uuid: 0x2800, end: 10, value: long},
		&attr{handle: 9, uuid: 0x2803, value: append([]byte{0x20, 10, 0}, long...)},
		&attr{handle: 10, uuid128: long, value: bytes.Repeat([]byte{7}, 100)},
	)
}

func startClient(t *testing.T, p *peripheral, onNotify func(uint16, []byte)) *attClient {
	t.Helper()
	c := newATTClient(p, onNotify)
	go c.run()
	t.Cleanup(func() { p.Close() })
	return c
}

func TestExchangeMTU(t *testing.T) {
	p := heartRate()
	c := startClient(t, p, nil)
	if c.MTU() != 23 {
		t.Errorf("MTU before the exchange: %d", c.MTU())
	}
	if err := c.exchangeMTU(517); err != nil {
		t.Fatal(err)
	}
	if c.MTU() != 247 { // the server's limit
		t.Errorf("MTU %d, want 247", c.MTU())
	}
}

func TestServicesWithPaging(t *testing.T) {
	for _, mtu := range []uint16{23, 247} { // 23 forces a response per few attributes
		c := startClient(t, heartRate(), nil)
		if mtu != 23 {
			if err := c.exchangeMTU(mtu); err != nil {
				t.Fatal(err)
			}
		}
		svcs, err := c.Services()
		if err != nil {
			t.Fatal(err)
		}
		if len(svcs) != 2 {
			t.Fatalf("mtu %d: %d services: %+v", mtu, len(svcs), svcs)
		}
		hr := svcs[0]
		if hr.UUID != short(0x180d) || hr.Handle != 1 || len(hr.Characteristics) != 2 {
			t.Fatalf("service %+v", hr)
		}
		ch := hr.Characteristics[0]
		// ESPHome reports the value handle, not the declaration's.
		if ch.Handle != 3 || ch.UUID != short(0x2a37) || ch.Properties != 0x10 {
			t.Errorf("characteristic %+v", ch)
		}
		want := []ble.Descriptor{{UUID: short(0x2902), Handle: 4}, {UUID: short(0x2901), Handle: 5}}
		if len(ch.Descriptors) != 2 || ch.Descriptors[0] != want[0] || ch.Descriptors[1] != want[1] {
			t.Errorf("descriptors %+v", ch.Descriptors)
		}
		if got := hr.Characteristics[1]; got.Handle != 7 || len(got.Descriptors) != 0 {
			t.Errorf("second characteristic %+v", got)
		}
		custom, _ := ble.ParseUUID("abcd1234-0000-1000-8000-00805f9b34fb")
		if svcs[1].UUID != custom || svcs[1].Characteristics[0].UUID != custom {
			t.Errorf("128-bit uuids: %+v", svcs[1])
		}
	}
}

func TestServicesAskedOncePerConnection(t *testing.T) {
	p := heartRate()
	c := startClient(t, p, nil)
	if _, err := c.Services(); err != nil {
		t.Fatal(err)
	}
	n := len(p.ops())
	if _, err := c.Services(); err != nil {
		t.Fatal(err)
	}
	if got := len(p.ops()); got != n {
		t.Errorf("second call sent %d requests", got-n)
	}
}

func TestReadLong(t *testing.T) {
	p := heartRate()
	c := startClient(t, p, nil)
	got, err := c.Read(10)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{7}, 100)) {
		t.Errorf("read %d bytes", len(got))
	}
}

func TestReadExactlyOnePacketEndsOnNotLong(t *testing.T) {
	p := newPeripheral(&attr{handle: 1, uuid: 0x2a00, value: bytes.Repeat([]byte{1}, 22)})
	c := startClient(t, p, nil)
	got, err := c.Read(1)
	if err != nil || len(got) != 22 {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
}

func TestReadError(t *testing.T) {
	c := startClient(t, heartRate(), nil)
	_, err := c.Read(999)
	var ae *ATTError
	if !errors.As(err, &ae) || ae.Code != attErrInvalidHandle || ae.Handle != 999 {
		t.Fatalf("got %v", err)
	}
}

func TestWrites(t *testing.T) {
	p := heartRate()
	c := startClient(t, p, nil)
	if err := c.Write(3, []byte{1, 2}, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.wrote(3), []byte{1, 2}) {
		t.Errorf("wrote % x", p.wrote(3))
	}
	if err := c.Write(5, []byte{3}, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.wrote(5), []byte{3}) {
		t.Errorf("command wrote % x", p.wrote(5))
	}
	if err := c.WriteDescriptor(4, []byte{1, 0}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.wrote(4), []byte{1, 0}) {
		t.Errorf("descriptor wrote % x", p.wrote(4))
	}
}

func TestWriteWithoutResponseTooLong(t *testing.T) {
	c := startClient(t, heartRate(), nil)
	if err := c.Write(3, make([]byte, 21), false); err == nil {
		t.Error("no error for a command over the MTU")
	}
}

func TestWriteLongUsesPreparedWrites(t *testing.T) {
	p := heartRate()
	c := startClient(t, p, nil)
	data := make([]byte, 50)
	for i := range data {
		data[i] = byte(i)
	}
	if err := c.Write(3, data, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.wrote(1000), data) {
		t.Errorf("prepared % x", p.wrote(1000))
	}
}

// A long write that fails must leave nothing queued on the peripheral, or the next one commits it too.
func TestWriteLongCancelledOnFailure(t *testing.T) {
	p := heartRate()
	p.prepareFailAt = 2
	c := startClient(t, p, nil)
	if err := c.Write(3, make([]byte, 50), true); err == nil {
		t.Fatal("no error")
	}
	ops := p.ops()
	if ops[len(ops)-1] != opExecuteReq {
		t.Fatalf("requests % x", ops)
	}
	if len(p.prepared) != 0 || p.wrote(1000) != nil {
		t.Errorf("left queued % x, committed % x", p.prepared, p.wrote(1000))
	}
}

// The echo of a Prepare Write is there to catch a value that changed on the way.
func TestWriteLongChecksTheEcho(t *testing.T) {
	p := heartRate()
	p.prepareCorrupt = true
	c := startClient(t, p, nil)
	if err := c.Write(3, make([]byte, 50), true); err == nil {
		t.Fatal("no error")
	}
	if p.wrote(1000) != nil {
		t.Errorf("committed % x", p.wrote(1000))
	}
	if ops := p.ops(); ops[len(ops)-1] != opExecuteReq {
		t.Errorf("requests % x", ops)
	}
}

func TestSecondaryServicesUnsupported(t *testing.T) {
	p := heartRate()
	p.noSecondary = true
	c := startClient(t, p, nil)
	svcs, err := c.Services()
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 2 {
		t.Errorf("%d services", len(svcs))
	}
}

// indicating sends a Service Changed indication just before the answer to the first Find Information.
type indicating struct {
	*peripheral
	once sync.Once
}

func (i *indicating) Write(b []byte) (int, error) {
	if b[0] == opFindInfoReq {
		i.once.Do(func() { i.push([]byte{opIndicate, 3, 0}) })
	}
	return i.peripheral.Write(b)
}

// The read loop must keep reading while a discovery waits for it, and what that discovery found is out of date.
func TestServiceChangedDuringDiscovery(t *testing.T) {
	p := newPeripheral(svc(1, 4, 0x1801), charDecl(2, 0x20, uuidServiceChanged),
		&attr{handle: 3, uuid: uuidServiceChanged}, &attr{handle: 4, uuid: 0x2902})
	c := newATTClient(&indicating{peripheral: p}, nil)
	c.timeout = 300 * time.Millisecond
	go c.run()
	t.Cleanup(func() { p.Close() })
	if _, err := c.Services(); err != nil {
		t.Fatal(err)
	}
	asked := len(p.ops())
	if _, err := c.Services(); err != nil {
		t.Fatal(err)
	}
	if len(p.ops()) == asked {
		t.Error("kept the services found before the change")
	}
	asked = len(p.ops())
	if _, err := c.Services(); err != nil {
		t.Fatal(err)
	}
	if len(p.ops()) != asked {
		t.Error("discovered again with no new change")
	}
}

func TestSecurityRaisedOnceAndRetried(t *testing.T) {
	p := heartRate()
	c := startClient(t, p, nil)
	raised := 0
	c.raise = func() error {
		raised++
		p.mu.Lock()
		p.encrypted = true
		p.mu.Unlock()
		return nil
	}
	got, err := c.Read(7)
	if err != nil || !bytes.Equal(got, []byte{1}) {
		t.Fatalf("read % x, %v", got, err)
	}
	if err := c.Write(7, []byte{2}, true); err != nil {
		t.Fatal(err)
	}
	if raised != 1 {
		t.Errorf("raised %d times", raised)
	}
}

func TestSecurityFailureReported(t *testing.T) {
	c := startClient(t, heartRate(), nil)
	boom := errors.New("pairing refused")
	c.raise = func() error { return boom }
	if _, err := c.Read(7); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestNotifyWritesCCCD(t *testing.T) {
	p := heartRate()
	c := startClient(t, p, nil)
	if err := c.Notify(3, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.wrote(4), []byte{1, 0}) {
		t.Errorf("enable wrote % x", p.wrote(4))
	}
	if err := c.Notify(3, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.wrote(4), []byte{0, 0}) {
		t.Errorf("disable wrote % x", p.wrote(4))
	}
}

func TestNotifyPicksIndicationWhenOnlyThatIsOffered(t *testing.T) {
	p := newPeripheral(svc(1, 4, 0x1801), charDecl(2, 0x20, 0x2a05),
		&attr{handle: 3, uuid: 0x2a05}, &attr{handle: 4, uuid: 0x2902})
	c := startClient(t, p, nil)
	if err := c.Notify(3, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.wrote(4), []byte{2, 0}) {
		t.Errorf("wrote % x", p.wrote(4))
	}
}

// Home Assistant keeps its own copy of the services, so Notify must work without a discovery on this
// connection and must not walk the whole database to find one descriptor.
func TestNotifyWithoutDiscovery(t *testing.T) {
	p := heartRate()
	c := startClient(t, p, nil)
	if err := c.Notify(3, true); err != nil {
		t.Fatal(err)
	}
	for _, op := range p.ops() {
		if op == opReadGroupReq || op == opReadTypeReq {
			t.Errorf("Notify did a discovery: op %#x", op)
		}
	}
}

func TestNotifyStopsAtNextCharacteristic(t *testing.T) {
	p := newPeripheral(svc(1, 5, 0x180f), charDecl(2, 0x12, 0x2a19), &attr{handle: 3, uuid: 0x2a19},
		charDecl(4, 0x10, 0x2a1a), &attr{handle: 5, uuid: 0x2a1a}, &attr{handle: 6, uuid: 0x2902})
	c := startClient(t, p, nil)
	if err := c.Notify(3, true); err == nil {
		t.Errorf("found another characteristic's CCCD: wrote % x", p.wrote(6))
	}
}

func TestNotificationsDelivered(t *testing.T) {
	p := heartRate()
	got := make(chan []byte, 2)
	startClient(t, p, func(h uint16, data []byte) {
		got <- append(binary.LittleEndian.AppendUint16(nil, h), data...)
	})
	p.push([]byte{opNotify, 3, 0, 9})
	p.push([]byte{opIndicate, 3, 0, 8})
	for _, want := range [][]byte{{3, 0, 9}, {3, 0, 8}} {
		select {
		case b := <-got:
			if !bytes.Equal(b, want) {
				t.Errorf("got % x, want % x", b, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no notification")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		n := p.confirms
		p.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d confirmations", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPeerRequestsAnswered(t *testing.T) {
	p := heartRate()
	sent := make(chan []byte, 2)
	c := newATTClient(&tapWrites{transport: p, sent: sent}, nil)
	go c.run()
	t.Cleanup(func() { p.Close() })
	p.push([]byte{opMTUReq, 100, 0})
	if b := <-sent; b[0] != opMTURsp {
		t.Errorf("answer to an MTU request: % x", b)
	}
	if c.MTU() != 100 {
		t.Errorf("MTU %d after the peer's exchange", c.MTU())
	}
	p.push([]byte{opReadReq, 1, 0})
	if b := <-sent; b[0] != opError || b[1] != opReadReq || b[4] != attErrNotSupported {
		t.Errorf("answer to a read: % x", b)
	}
}

type tapWrites struct {
	transport
	sent chan []byte
}

func (t *tapWrites) Write(b []byte) (int, error) {
	t.sent <- append([]byte(nil), b...)
	return len(b), nil
}

func TestRequestFailsWhenLinkDrops(t *testing.T) {
	p := heartRate()
	p.silent = true
	c := startClient(t, p, nil)
	errc := make(chan error, 1)
	go func() {
		_, err := c.Read(3)
		errc <- err
	}()
	time.Sleep(10 * time.Millisecond)
	p.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ble.ErrNotConnected) {
			t.Errorf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not end")
	}
	if _, err := c.Read(3); !errors.Is(err, ble.ErrNotConnected) {
		t.Errorf("after the drop: %v", err)
	}
}

func TestRequestTimeoutClosesLink(t *testing.T) {
	p := heartRate()
	p.silent = true
	c := startClient(t, p, nil)
	c.timeout = 20 * time.Millisecond
	if _, err := c.Read(3); err == nil {
		t.Fatal("no error")
	}
	select {
	case <-p.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the link stayed up: ATT allows no further request after a timeout")
	}
}

func TestMalformedResponsesFail(t *testing.T) {
	for name, rsp := range map[string][]byte{
		"group length":  {opReadGroupRsp, 1, 2},
		"group partial": {opReadGroupRsp, 6, 1, 0, 2, 0, 0},
		"no progress":   {opReadGroupRsp, 6, 0, 0, 0, 0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			c := newATTClient(&scripted{rsp: rsp, in: make(chan []byte, 1), closed: make(chan struct{})}, nil)
			go c.run()
			defer c.t.Close()
			_, err := c.Services()
			if err == nil || errors.Is(err, ble.ErrNotConnected) { // a timeout would be ErrNotConnected
				t.Errorf("got %v, want a parse error", err)
			}
		})
	}
}

// A peripheral answering Find Information with handle 0xffff followed by handle 0 made the handle check wrap
// around, so the walk started over from handle 1 and never ended. Both walks must stop within a few requests.
func TestDescriptorWalkEndsOnWrappedHandles(t *testing.T) {
	rsp := []byte{opFindInfoRsp, 1, 0xff, 0xff, 0x01, 0x29, 0x00, 0x00, 0x01, 0x29}
	for name, walk := range map[string]func(c *attClient) error{
		"notify": func(c *attClient) error { return c.Notify(3, true) },
		"descriptors": func(c *attClient) error {
			_, err := c.discoverDescs(4, 0xffff)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := &scripted{rsp: rsp, limit: 8, in: make(chan []byte, 1), closed: make(chan struct{})}
			c := newATTClient(s, nil)
			go c.run()
			defer c.t.Close()
			err := walk(c)
			if err == nil || errors.Is(err, ble.ErrNotConnected) { // the limit makes a loop ErrNotConnected
				t.Errorf("got %v, want a parse error", err)
			}
		})
	}
}

// scripted answers every request with the same bytes. With a limit, it drops the link after that many requests.
type scripted struct {
	rsp    []byte
	limit  int
	writes int
	in     chan []byte
	closed chan struct{}
	once   sync.Once
}

func (s *scripted) Read(b []byte) (int, error) {
	select {
	case pdu := <-s.in:
		return copy(b, pdu), nil
	case <-s.closed:
		return 0, io.EOF
	}
}

func (s *scripted) Write(b []byte) (int, error) {
	s.writes++
	if s.limit > 0 && s.writes > s.limit {
		s.Close()
		return 0, io.ErrClosedPipe
	}
	s.in <- s.rsp
	return len(b), nil
}

func (s *scripted) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}
