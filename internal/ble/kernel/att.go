// Package kernel is a Bluetooth backend for Linux that talks to the kernel's Bluetooth stack directly, without
// bluetoothd or D-Bus: the management socket for the controller and bonds, the HCI monitor for advertisements
// and an L2CAP socket on the ATT channel for each connection. The kernel still does HCI, encryption and pairing.
package kernel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// ATT opcodes (Bluetooth Core, Vol 3, Part F, 3.4.8).
const (
	opError        = 0x01
	opMTUReq       = 0x02
	opMTURsp       = 0x03
	opFindInfoReq  = 0x04
	opFindInfoRsp  = 0x05
	opReadTypeReq  = 0x08
	opReadTypeRsp  = 0x09
	opReadReq      = 0x0a
	opReadRsp      = 0x0b
	opReadBlobReq  = 0x0c
	opReadBlobRsp  = 0x0d
	opReadGroupReq = 0x10
	opReadGroupRsp = 0x11
	opWriteReq     = 0x12
	opWriteRsp     = 0x13
	opPrepareReq   = 0x16
	opPrepareRsp   = 0x17
	opExecuteReq   = 0x18
	opExecuteRsp   = 0x19
	opNotify       = 0x1b
	opIndicate     = 0x1d
	opConfirm      = 0x1e
	opWriteCmd     = 0x52

	opCommandBit = 0x40 // set on opcodes that get no response
)

// ATT error codes.
const (
	attErrInvalidHandle   = 0x01
	attErrInsufficientAut = 0x05
	attErrNotSupported    = 0x06
	attErrInvalidOffset   = 0x07
	attErrInsufficientAuz = 0x08
	attErrNotFound        = 0x0a
	attErrNotLong         = 0x0b
	attErrInsufficientEnc = 0x0f
	attErrUnsupportedType = 0x10 // the attribute type is not a grouping the server supports
)

const (
	defaultMTU = 23
	maxMTU     = 517 // the largest ATT_MTU the proxy asks for: a value of 512 bytes plus the longest header

	// attTimeout is the limit the ATT specification sets on a transaction.
	attTimeout = 30 * time.Second
)

// ATTError is an error response from the peripheral.
type ATTError struct {
	Op     byte
	Handle uint16
	Code   byte
}

func (e *ATTError) Error() string {
	return fmt.Sprintf("ATT error %#02x on handle %#04x (request %#02x)", e.Code, e.Handle, e.Op)
}

// transport carries one ATT PDU per Read or Write. Closing it ends a blocked Read.
type transport interface {
	Read(b []byte) (int, error)
	Write(b []byte) (int, error)
	Close() error
}

// attClient is the client side of ATT on one connection, with the GATT procedures in gatt.go. ATT allows one
// request at a time in each direction: reqMu holds the proxy's, and run answers the peripheral's.
type attClient struct {
	t        transport
	onNotify func(handle uint16, data []byte)
	// raise, if set, encrypts the link. A request refused for lack of encryption is retried once after it.
	raise   func() error
	timeout time.Duration

	mtu   atomic.Uint32
	reqMu sync.Mutex
	rsp   chan []byte

	raiseMu sync.Mutex
	raised  bool

	done    chan struct{}
	doneErr error // set before done closes

	gattMu      sync.Mutex
	services    []ble.Service
	servicesGen uint32 // the value of changes that services was discovered at
	// serviceChanged is the value handle of the Service Changed characteristic once discovery has seen it.
	serviceChanged atomic.Uint32
	// changes counts Service Changed indications. run must not take gattMu to note one: discovery holds it while it
	// waits for run to read its responses.
	changes atomic.Uint32
}

func newATTClient(t transport, onNotify func(uint16, []byte)) *attClient {
	c := &attClient{t: t, onNotify: onNotify, timeout: attTimeout, rsp: make(chan []byte, 1), done: make(chan struct{})}
	c.mtu.Store(defaultMTU)
	return c
}

// run reads PDUs until the link goes away, and returns why.
func (c *attClient) run() error {
	buf := make([]byte, maxMTU+1)
	for {
		n, err := c.t.Read(buf)
		if err != nil {
			c.doneErr = err
			close(c.done)
			return err
		}
		if n > 0 {
			c.receive(buf[:n])
		}
	}
}

func (c *attClient) receive(pdu []byte) {
	op := pdu[0]
	switch {
	case op == opNotify || op == opIndicate:
		if len(pdu) < 3 {
			return
		}
		h := binary.LittleEndian.Uint16(pdu[1:])
		if op == opIndicate {
			c.t.Write([]byte{opConfirm})
		}
		if uint32(h) == c.serviceChanged.Load() && h != 0 {
			c.changes.Add(1)
		}
		if c.onNotify != nil {
			c.onNotify(h, append([]byte(nil), pdu[3:]...))
		}
	case op == opMTUReq:
		if len(pdu) < 3 {
			return
		}
		c.t.Write(binary.LittleEndian.AppendUint16([]byte{opMTURsp}, maxMTU))
		c.mtu.Store(negotiated(binary.LittleEndian.Uint16(pdu[1:])))
	case op&1 == 1 && op&opCommandBit == 0: // a response
		select {
		case c.rsp <- append([]byte(nil), pdu...):
		default: // no request is waiting for it
		}
	case op&opCommandBit != 0: // a command: nothing to answer
	default:
		c.t.Write([]byte{opError, op, 0, 0, attErrNotSupported})
	}
}

func negotiated(peer uint16) uint32 {
	return uint32(max(min(peer, maxMTU), defaultMTU))
}

// request sends a PDU and waits for its response. An error response comes back as *ATTError, and every
// failure of the link wraps ble.ErrNotConnected. A request that times out takes the link down, because ATT
// allows no further request after one.
func (c *attClient) request(pdu []byte, want byte) ([]byte, error) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	select {
	case <-c.done:
		return nil, c.linkErr()
	default:
	}
	select { // an answer to nothing must not pass for the answer to this request
	case <-c.rsp:
	default:
	}
	if _, err := c.t.Write(pdu); err != nil {
		return nil, fmt.Errorf("%w: %v", ble.ErrNotConnected, err)
	}
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	for {
		select {
		case rsp := <-c.rsp:
			if rsp[0] == opError && len(rsp) >= 5 && rsp[1] == pdu[0] {
				return nil, &ATTError{Op: rsp[1], Handle: binary.LittleEndian.Uint16(rsp[2:]), Code: rsp[4]}
			}
			if rsp[0] == want {
				return rsp, nil
			}
		case <-c.done:
			return nil, c.linkErr()
		case <-timer.C:
			c.t.Close()
			return nil, fmt.Errorf("%w: no answer to ATT request %#02x", ble.ErrNotConnected, pdu[0])
		}
	}
}

func (c *attClient) linkErr() error {
	return fmt.Errorf("%w: %v", ble.ErrNotConnected, c.doneErr)
}

// secure retries op once with the link encrypted if the peripheral refuses it for lack of encryption, as
// BlueZ and the ESP32 do without the client asking.
func (c *attClient) secure(op func() error) error {
	err := op()
	var ae *ATTError
	if c.raise == nil || !errors.As(err, &ae) || (ae.Code != attErrInsufficientEnc &&
		ae.Code != attErrInsufficientAut && ae.Code != attErrInsufficientAuz) {
		return err
	}
	c.raiseMu.Lock()
	again := !c.raised
	c.raised = true
	c.raiseMu.Unlock()
	if !again {
		return err
	}
	if rerr := c.raise(); rerr != nil {
		return fmt.Errorf("%w (encrypting after %v)", rerr, err)
	}
	return op()
}

// MTU implements ble.Conn.
func (c *attClient) MTU() uint32 { return c.mtu.Load() }

func (c *attClient) exchangeMTU(want uint16) error {
	rsp, err := c.request(binary.LittleEndian.AppendUint16([]byte{opMTUReq}, want), opMTURsp)
	if err != nil {
		return err
	}
	if len(rsp) < 3 {
		return errors.New("short MTU response")
	}
	c.mtu.Store(uint32(min(uint32(want), negotiated(binary.LittleEndian.Uint16(rsp[1:])))))
	return nil
}

// Read implements ble.Conn: a long value takes further requests after the first.
func (c *attClient) Read(h uint16) ([]byte, error) {
	var data []byte
	err := c.secure(func() error {
		var err error
		data, err = c.readLong(h)
		return err
	})
	return data, err
}

func (c *attClient) readLong(h uint16) ([]byte, error) {
	rsp, err := c.request(binary.LittleEndian.AppendUint16([]byte{opReadReq}, h), opReadRsp)
	if err != nil {
		return nil, err
	}
	data := append([]byte(nil), rsp[1:]...)
	last := len(rsp) - 1
	for last == int(c.mtu.Load())-1 && len(data) < 0x10000 {
		req := binary.LittleEndian.AppendUint16([]byte{opReadBlobReq}, h)
		req = binary.LittleEndian.AppendUint16(req, uint16(len(data)))
		rsp, err := c.request(req, opReadBlobRsp)
		var ae *ATTError
		if errors.As(err, &ae) && (ae.Code == attErrNotLong || ae.Code == attErrInvalidOffset) {
			break // the value ended exactly at the end of the last response
		}
		if err != nil {
			return nil, err
		}
		data = append(data, rsp[1:]...)
		last = len(rsp) - 1
	}
	return data, nil
}

// ReadDescriptor implements ble.Conn. In ATT a descriptor is an attribute like any other.
func (c *attClient) ReadDescriptor(h uint16) ([]byte, error) { return c.Read(h) }

// Write implements ble.Conn. A write with a response longer than a packet goes in prepared writes.
func (c *attClient) Write(h uint16, data []byte, withResponse bool) error {
	limit := int(c.mtu.Load()) - 3
	if !withResponse {
		if len(data) > limit {
			return fmt.Errorf("%d bytes do not fit a write without response at MTU %d", len(data), limit+3)
		}
		_, err := c.t.Write(append(binary.LittleEndian.AppendUint16([]byte{opWriteCmd}, h), data...))
		if err != nil {
			return fmt.Errorf("%w: %v", ble.ErrNotConnected, err)
		}
		return nil
	}
	return c.secure(func() error {
		if len(data) <= limit {
			_, err := c.request(append(binary.LittleEndian.AppendUint16([]byte{opWriteReq}, h), data...), opWriteRsp)
			return err
		}
		return c.writeLong(h, data)
	})
}

func (c *attClient) writeLong(h uint16, data []byte) error {
	chunk := int(c.mtu.Load()) - 5
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		req := binary.LittleEndian.AppendUint16([]byte{opPrepareReq}, h)
		req = binary.LittleEndian.AppendUint16(req, uint16(off))
		req = append(req, data[off:end]...)
		rsp, err := c.request(req, opPrepareRsp)
		if err == nil && !bytes.Equal(rsp[1:], req[1:]) {
			err = errors.New("the device echoed a prepared write with different contents")
		}
		if err != nil {
			// What is already queued would go out with the next long write on this link.
			c.request([]byte{opExecuteReq, 0}, opExecuteRsp)
			return err
		}
	}
	_, err := c.request([]byte{opExecuteReq, 1}, opExecuteRsp)
	return err
}

// WriteDescriptor implements ble.Conn.
func (c *attClient) WriteDescriptor(h uint16, data []byte) error { return c.Write(h, data, true) }
