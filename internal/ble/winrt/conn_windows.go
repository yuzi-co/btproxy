package winrt

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"unsafe"

	"github.com/saltosystems/winrt-go/windows/foundation"
	"github.com/saltosystems/winrt-go/windows/foundation/collections"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/winrt/gen/windows/devices/bluetooth"
	gatt "github.com/yuzi-co/btproxy/internal/ble/winrt/gen/windows/devices/bluetooth/genericattributeprofile"
	"github.com/yuzi-co/btproxy/internal/ble/winrt/gen/windows/devices/enumeration"
)

// cccdUUID is the Client Characteristic Configuration descriptor.
var cccdUUID = ble.UUID{Hi: 0x0000290200001000, Lo: 0x800000805f9b34fb}

// conn is a GATT session with one device. Windows has no explicit connect: a GATT session that asks to keep
// the connection makes the stack connect, and the link drops once no session or service object holds it.
//
// Three locks guard a connection, taken in this order: c.ops before a.mu or c.mu, and a.mu and c.mu never
// together. Each lock is taken and released by one small method, so the order holds by construction:
//   - c.ops is held shared by every call into the WinRT objects (begin/end, attempt, statusChanged) and
//     exclusively to release them (release, reset). A release thus waits for calls in flight, and a call never
//     finds an object gone under it.
//   - a.mu guards the adapter's table of connections (claim, unclaim, connTo, clearUncached).
//   - c.mu guards the state flags and the per-characteristic notification state (isClosed, live, markUp, close,
//     subscribe, setCfg, takeConnParams). It is never held across a wait.
type conn struct {
	a    *Adapter
	addr uint64
	ev   ble.Events

	dev       *bluetooth.BluetoothLEDevice
	session   *gatt.GattSession
	statusH   *foundation.TypedEventHandler
	statusTok foundation.EventRegistrationToken
	upCh      chan struct{} // receives when the link comes up

	// Set before Connect returns, read-only afterwards.
	services []ble.Service
	svcObjs  []*gatt.GattDeviceService
	chars    map[uint16]*char // by value handle
	descs    map[uint16]*gatt.GattDescriptor
	cccd     map[uint16]uint16 // CCCD handle -> characteristic value handle

	cpReq *bluetooth.BluetoothLEPreferredConnectionParametersRequest // held while in effect; guarded by mu

	ops    sync.RWMutex // held shared by every call into the WinRT objects, exclusively to release them
	mu     sync.Mutex
	up     bool // reported connected to the caller
	closed bool // no new calls; release has run or is waiting for the calls in flight
}

type char struct {
	c     *gatt.GattCharacteristic
	props uint32
	h     *foundation.TypedEventHandler // ValueChanged handler, once notifications were enabled
	tok   foundation.EventRegistrationToken
	cfg   gatt.GattClientCharacteristicConfigurationDescriptorValue // last value written to the CCCD
}

// errClosed is a connect attempt overtaken by a Disconnect or Unpair.
var errClosed = errors.New("connection closed while connecting")

func (a *Adapter) Connect(addr uint64, addrType uint32, ev ble.Events) (ble.Conn, error) {
	c := &conn{a: a, addr: addr, ev: ev, upCh: make(chan struct{}, 1), chars: map[uint16]*char{},
		descs: map[uint16]*gatt.GattDescriptor{}, cccd: map[uint16]uint16{}}
	cacheMode, err := a.claim(c)
	if err != nil {
		return nil, err
	}
	// Until the services are resolved or the connect fails: the link is most fragile in its first
	// connection events, and service discovery is a burst of traffic right after.
	defer a.pauseScan()()

	// One deadline for the link to come up, retries included.
	deadline := time.Now().Add(connectTimeout)
	log := a.log.With("addr", ble.FormatMAC(addr))
	retries, err := a.connRetry.Do(func() error {
		return c.attempt(addrType, cacheMode, deadline)
	}, func(err error) bool {
		if !radioFailure(err) {
			log.Debug("connect failed, not a radio failure", "err", err)
			return false
		}
		return c.reset()
	}, ev.Withdrawn, func(n int, err error) {
		log.Debug("connect failed on the radio, retrying", "retry", n, "err", err)
	})
	if err != nil {
		c.release()
		if retries > 0 {
			return nil, fmt.Errorf("%d retries: %w", retries, err)
		}
		return nil, err
	}
	if retries > 0 {
		log.Info("connected after retries", "retries", retries)
	}
	if cacheMode == bluetooth.BluetoothCacheModeUncached {
		a.clearUncached(addr)
	}
	return c, nil
}

// claim registers c as the connection to its address, and tells which GATT cache mode its service discovery
// uses: uncached once after ClearCache.
func (a *Adapter) claim(c *conn) (bluetooth.BluetoothCacheMode, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, busy := a.conns[c.addr]; busy {
		return 0, errors.New("connection already in progress")
	}
	a.conns[c.addr] = c
	if a.uncached[c.addr] {
		return bluetooth.BluetoothCacheModeUncached, nil
	}
	return bluetooth.BluetoothCacheModeCached, nil
}

// unclaim frees c's address, unless another connection has taken it since.
func (a *Adapter) unclaim(c *conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conns[c.addr] == c {
		delete(a.conns, c.addr)
	}
}

// connTo is the connection to addr, nil if there is none.
func (a *Adapter) connTo(addr uint64) *conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conns[addr]
}

// clearUncached ends the uncached discovery asked for addr, which a connection has done.
func (a *Adapter) clearUncached(addr uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.uncached, addr)
}

// attempt makes one connect attempt with c.ops held shared, so a release meanwhile (an Unpair, or a
// Disconnect from the client) waits for it. A connection released before the attempt starts is not tried.
func (c *conn) attempt(addrType uint32, cacheMode bluetooth.BluetoothCacheMode, deadline time.Time) error {
	if err := c.begin(); err != nil {
		return errClosed
	}
	defer c.end()
	return c.connect(addrType, cacheMode, deadline)
}

// connect makes one attempt: device lookup, GATT session, link up by deadline, service discovery. On failure
// it leaves the WinRT objects it made in c, for reset or release to free.
func (c *conn) connect(addrType uint32, cacheMode bluetooth.BluetoothCacheMode, deadline time.Time) error {
	t := bluetooth.BluetoothAddressTypePublic
	if addrType == ble.AddrRandom {
		t = bluetooth.BluetoothAddressTypeRandom
	}
	op, err := bluetooth.BluetoothLEDeviceFromBluetoothAddressWithBluetoothAddressTypeAsync(c.addr, t)
	if err != nil {
		return err
	}
	p, err := await(op, bluetooth.SignatureBluetoothLEDevice, callTimeout)
	if err != nil {
		return fmt.Errorf("device lookup: %w", err)
	}
	if p == nil {
		return errors.New("device not found")
	}
	c.dev = (*bluetooth.BluetoothLEDevice)(p)

	c.statusH = eventHandler(bluetooth.SignatureBluetoothLEDevice, inspectableSig, func(_, _ unsafe.Pointer) {
		c.statusChanged()
	})
	if c.statusTok, err = c.dev.AddConnectionStatusChanged(c.statusH); err != nil {
		c.statusH.Release()
		c.statusH = nil
		return err
	}
	id, err := c.dev.GetBluetoothDeviceId()
	if err != nil {
		return err
	}
	op, err = gatt.GattSessionFromDeviceIdAsync(id)
	id.Release()
	if err != nil {
		return err
	}
	if p, err = await(op, gatt.SignatureGattSession, callTimeout); err != nil {
		return fmt.Errorf("GATT session: %w", err)
	}
	if p == nil {
		return errors.New("no GATT session")
	}
	c.session = (*gatt.GattSession)(p)
	if err := c.session.SetMaintainConnection(true); err != nil {
		return err
	}

	if !c.connected() {
		t := time.NewTimer(time.Until(deadline))
		defer t.Stop()
		select {
		case <-c.upCh:
		case <-t.C:
			return errors.New("no connection in time")
		}
	}
	if err := c.discover(cacheMode); err != nil {
		return fmt.Errorf("service discovery: %w", err)
	}
	c.settleMTU()

	// From here on a lost link is reported through ev.Disconnected; before, Connect fails instead.
	closed, lost := c.markUp()
	if closed {
		return errClosed
	}
	if lost {
		return errLinkLost
	}
	return nil
}

// markUp records the link as up, unless the connection was closed meanwhile or the link is down again, and
// reports which. It is called with c.ops held shared, as it asks the device.
func (c *conn) markUp() (closed, lost bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	closed = c.closed
	lost = closed || !c.connected()
	c.up = !lost
	return closed, lost
}

// reset frees the WinRT objects of a failed attempt so the next one starts afresh, and reports whether the
// connection may be retried: a release meanwhile (Disconnect or Unpair) ends the retries. The attempt has let
// go of c.ops, so reset takes it exclusively the way release does, which also keeps statusChanged from
// reading the device while it goes. A release that gets the lock first leaves nothing to free and is seen in
// closed; one that comes later is seen by the next attempt's begin.
func (c *conn) reset() bool {
	c.ops.Lock()
	c.free()
	c.services, c.svcObjs = nil, nil
	c.chars = map[uint16]*char{}
	c.descs = map[uint16]*gatt.GattDescriptor{}
	c.cccd = map[uint16]uint16{}
	select {
	case <-c.upCh:
	default:
	}
	c.ops.Unlock()
	return !c.isClosed()
}

func (c *conn) connected() bool {
	s, err := c.dev.GetConnectionStatus()
	return err == nil && s == bluetooth.BluetoothConnectionStatusConnected
}

// statusChanged runs on a WinRT thread when the link goes up or down.
func (c *conn) statusChanged() {
	if !c.ops.TryRLock() {
		return // being released
	}
	up := c.dev != nil && c.connected()
	c.ops.RUnlock()
	if up {
		select {
		case c.upCh <- struct{}{}:
		default:
		}
		return
	}
	if c.live() {
		// Not on the event thread: tearing down the session from its own event can stall it.
		go func() {
			if c.release() {
				c.ev.Disconnected(errors.New("link lost"))
			}
		}()
	}
}

// settleMTU gives Windows a moment to finish the MTU exchange it starts on its own after connecting, so the
// MTU reported with the connection is the negotiated one rather than the default 23.
func (c *conn) settleMTU() {
	deadline := time.Now().Add(mtuSettleWindow)
	for c.mtu() <= 23 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *conn) MTU() uint32 {
	if c.begin() != nil {
		return 23
	}
	defer c.end()
	return c.mtu()
}

func (c *conn) mtu() uint32 {
	if n, err := c.session.GetMaxPduSize(); err == nil && n > 0 {
		return uint32(n)
	}
	return 23
}

func (c *conn) Services() ([]ble.Service, error) { return c.services, nil }

// discover loads the GATT database and keeps the WinRT object behind every handle.
func (c *conn) discover(mode bluetooth.BluetoothCacheMode) error {
	op, err := c.dev.GetGattServicesWithCacheModeAsync(mode)
	err = gattOp(op, err, gatt.SignatureGattDeviceServicesResult, func(res *gatt.GattDeviceServicesResult) error {
		v, err := res.GetServices()
		if err != nil {
			return err
		}
		defer v.Release()
		return eachItem(v, func(p unsafe.Pointer) error {
			s := (*gatt.GattDeviceService)(p)
			c.svcObjs = append(c.svcObjs, s) // kept and closed with the connection
			return c.discoverService(s, mode)
		})
	})
	if err != nil {
		return err
	}
	sortServices(c.services)
	return nil
}

func (c *conn) discoverService(s *gatt.GattDeviceService, mode bluetooth.BluetoothCacheMode) error {
	h, err := s.GetAttributeHandle()
	if err != nil {
		return err
	}
	g, err := s.GetUuid()
	if err != nil {
		return err
	}
	svc := ble.Service{UUID: uuidOf(g), Handle: h}
	op, err := s.GetCharacteristicsWithCacheModeAsync(mode)
	err = gattOp(op, err, gatt.SignatureGattCharacteristicsResult, func(res *gatt.GattCharacteristicsResult) error {
		v, err := res.GetCharacteristics()
		if err != nil {
			return err
		}
		defer v.Release()
		return eachItem(v, func(p unsafe.Pointer) error {
			ch := (*gatt.GattCharacteristic)(p)
			info, err := c.discoverChar(ch, mode)
			if err != nil {
				ch.Release()
				return err
			}
			svc.Characteristics = append(svc.Characteristics, info)
			return nil
		})
	})
	// A service the stack may not open (HID, for one) answers AccessDenied: list it without characteristics.
	if err != nil && !errors.Is(err, errAccessDenied) {
		return fmt.Errorf("characteristics of %s: %w", svc.UUID, err)
	}
	c.services = append(c.services, svc)
	return nil
}

func (c *conn) discoverChar(ch *gatt.GattCharacteristic, mode bluetooth.BluetoothCacheMode) (ble.Characteristic, error) {
	var info ble.Characteristic
	decl, err := ch.GetAttributeHandle()
	if err != nil {
		return info, err
	}
	// Windows gives the characteristic declaration's handle; ESPHome reports the value handle that follows
	// it, and Home Assistant keeps one GATT cache per device across proxies, so the handles must match.
	h := decl + 1
	g, err := ch.GetUuid()
	if err != nil {
		return info, err
	}
	props, err := ch.GetCharacteristicProperties()
	if err != nil {
		return info, err
	}
	// The low byte is the GATT properties field; the rest are extended properties Windows folds in.
	info = ble.Characteristic{UUID: uuidOf(g), Handle: h, Properties: uint32(props) & 0xff}
	op, err := ch.GetDescriptorsWithCacheModeAsync(mode)
	err = gattOp(op, err, gatt.SignatureGattDescriptorsResult, func(res *gatt.GattDescriptorsResult) error {
		v, err := res.GetDescriptors()
		if err != nil {
			return err
		}
		defer v.Release()
		return eachItem(v, func(p unsafe.Pointer) error {
			d := (*gatt.GattDescriptor)(p)
			dh, err := d.GetAttributeHandle()
			if err != nil {
				d.Release()
				return err
			}
			dg, err := d.GetUuid()
			if err != nil {
				d.Release()
				return err
			}
			u := uuidOf(dg)
			c.descs[dh] = d
			if u == cccdUUID {
				c.cccd[dh] = h
			}
			info.Descriptors = append(info.Descriptors, ble.Descriptor{UUID: u, Handle: dh})
			return nil
		})
	})
	if err != nil {
		return info, fmt.Errorf("descriptors of %s: %w", info.UUID, err)
	}
	// Only now: on an error the caller releases ch, so free must not find it here and release it again.
	c.chars[h] = &char{c: ch, props: info.Properties}
	return info, nil
}

// eachItem calls fn for every element of a vector view; fn owns the element.
func eachItem(v *collections.IVectorView, fn func(unsafe.Pointer) error) error {
	n, err := v.GetSize()
	if err != nil {
		return err
	}
	for i := range n {
		p, err := v.GetAt(i)
		if err != nil {
			return err
		}
		if p == nil {
			continue
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

// The GattCommunicationStatus values other than Success. A ProtocolError comes with the ATT error code, as an
// attError, which is also an errProtocol.
var (
	errUnreachable  = errors.New("device unreachable")
	errProtocol     = errors.New("GATT protocol error")
	errAccessDenied = errors.New("access denied")
)

// attError is the error code of an ATT error response, which Windows reports as a ProtocolError status with
// the code in the result's ProtocolError, or as an HRESULT of FACILITY_BLUETOOTH_ATT. The values are the ATT
// ones (Core spec Vol 3 Part F, 3.4.1.1), which are also what an ESP32 proxy reports for them (the low
// esp_gatt_status_t values match the ATT codes).
type attError byte

// attErrorNames names the codes the core specification defines; 0x80-0x9f are application defined and
// 0xe0-0xff common profile and service codes.
var attErrorNames = map[attError]string{
	0x01: "invalid handle",
	0x02: "read not permitted",
	0x03: "write not permitted",
	0x04: "invalid PDU",
	0x05: "insufficient authentication",
	0x06: "request not supported",
	0x07: "invalid offset",
	0x08: "insufficient authorization",
	0x09: "prepare queue full",
	0x0a: "attribute not found",
	0x0b: "attribute not long",
	0x0c: "encryption key size too short",
	0x0d: "invalid attribute value length",
	0x0e: "unlikely error",
	0x0f: "insufficient encryption",
	0x10: "unsupported group type",
	0x11: "insufficient resources",
	0x12: "database out of sync",
	0x13: "value not allowed",
	0xfc: "write request rejected",
	0xfd: "CCCD improperly configured",
	0xfe: "procedure already in progress",
	0xff: "out of range",
}

func (e attError) Error() string {
	if name, ok := attErrorNames[e]; ok {
		return fmt.Sprintf("ATT error 0x%02x (%s)", byte(e), name)
	}
	return fmt.Sprintf("ATT error 0x%02x", byte(e))
}

// Is makes every ATT error match errProtocol, the GATT status it arrives with.
func (e attError) Is(target error) bool { return target == errProtocol }

// protocolError reads the ATT error code from a GATT result's ProtocolError reference; without one, the
// error is the bare errProtocol.
func protocolError(ref *foundation.IReference, err error) error {
	if err != nil || ref == nil {
		return errProtocol
	}
	defer ref.Release()
	v, err := ref.GetValue() // IReference<UInt8>: the byte comes back in the pointer-sized out slot
	if err != nil {
		return errProtocol
	}
	return attError(byte(uintptr(v)))
}

// gattResult is what every GATT result object (services, characteristics, descriptors, read, write) reports:
// a communication status, and the ATT error code when the status is ProtocolError.
type gattResult interface {
	GetStatus() (gatt.GattCommunicationStatus, error)
	GetProtocolError() (*foundation.IReference, error)
	Release() int32
}

// gattOp runs the ladder every GATT operation shares: op, started with err, is awaited with callTimeout; a
// status other than Success becomes errUnreachable, errAccessDenied, the attError of a ProtocolError or an
// error naming the status; on Success the result goes to use, and is released once use returns. The result
// type is that of use's parameter, and sig its WinRT signature.
func gattOp[R any, PR interface {
	*R
	gattResult
}](op *foundation.IAsyncOperation, err error, sig string, use func(PR) error) error {
	if err != nil {
		return err
	}
	p, err := await(op, sig, callTimeout)
	if err != nil {
		return err
	}
	res := PR(p)
	defer res.Release()
	st, err := res.GetStatus()
	if err != nil {
		return err
	}
	switch st {
	case gatt.GattCommunicationStatusSuccess:
		return use(res)
	case gatt.GattCommunicationStatusUnreachable:
		return errUnreachable
	case gatt.GattCommunicationStatusProtocolError:
		return protocolError(res.GetProtocolError())
	case gatt.GattCommunicationStatusAccessDenied:
		return errAccessDenied
	}
	return fmt.Errorf("GATT status %d", st)
}

// begin starts a call into the connection's WinRT objects; end must follow once it is done.
func (c *conn) begin() error {
	c.ops.RLock()
	if c.isClosed() {
		c.ops.RUnlock()
		return ble.ErrNotConnected
	}
	return nil
}

func (c *conn) end() { c.ops.RUnlock() }

func (c *conn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// live reports whether the connection is up and not closed: whether the device's events still count.
func (c *conn) live() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.up && !c.closed
}

func (c *conn) char(h uint16) (*char, error) {
	if ch, ok := c.chars[h]; ok {
		return ch, nil
	}
	return nil, fmt.Errorf("no characteristic with handle 0x%04x", h)
}

func (c *conn) desc(h uint16) (*gatt.GattDescriptor, error) {
	if d, ok := c.descs[h]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("no descriptor with handle 0x%04x", h)
}

// readResult unpacks an IAsyncOperation<GattReadResult>.
func readResult(op *foundation.IAsyncOperation, err error) ([]byte, error) {
	var data []byte
	err = gattOp(op, err, gatt.SignatureGattReadResult, func(res *gatt.GattReadResult) error {
		b, err := res.GetValue()
		if err != nil || b == nil {
			return err
		}
		defer b.Release()
		data, err = appendBuffer(nil, b)
		return err
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// writeResult unpacks an IAsyncOperation<GattWriteResult>.
func writeResult(op *foundation.IAsyncOperation, err error) error {
	return gattOp(op, err, gatt.SignatureGattWriteResult, func(*gatt.GattWriteResult) error { return nil })
}

func (c *conn) Read(h uint16) ([]byte, error) {
	if err := c.begin(); err != nil {
		return nil, err
	}
	defer c.end()
	ch, err := c.char(h)
	if err != nil {
		return nil, err
	}
	return readResult(ch.c.ReadValueWithCacheModeAsync(bluetooth.BluetoothCacheModeUncached))
}

func (c *conn) Write(h uint16, data []byte, withResponse bool) error {
	if err := c.begin(); err != nil {
		return err
	}
	defer c.end()
	ch, err := c.char(h)
	if err != nil {
		return err
	}
	b, err := newBuffer(data)
	if err != nil {
		return err
	}
	defer b.Release()
	opt := gatt.GattWriteOptionWriteWithoutResponse
	if withResponse {
		opt = gatt.GattWriteOptionWriteWithResponse
	}
	return writeResult(ch.c.WriteValueWithResultAndOptionAsync(b, opt))
}

func (c *conn) ReadDescriptor(h uint16) ([]byte, error) {
	if err := c.begin(); err != nil {
		return nil, err
	}
	defer c.end()
	d, err := c.desc(h)
	if err != nil {
		return nil, err
	}
	return readResult(d.ReadValueWithCacheModeAsync(bluetooth.BluetoothCacheModeUncached))
}

// WriteDescriptor writes a descriptor. Windows owns the CCCD, so a CCCD write goes through the
// characteristic's configuration call, which also has notifications delivered.
func (c *conn) WriteDescriptor(h uint16, data []byte) error {
	if err := c.begin(); err != nil {
		return err
	}
	defer c.end()
	if vh, ok := c.cccd[h]; ok {
		ch, err := c.char(vh)
		if err != nil {
			return err
		}
		v := gatt.GattClientCharacteristicConfigurationDescriptorValueNone
		switch {
		case len(data) > 0 && data[0]&0x01 != 0:
			v = gatt.GattClientCharacteristicConfigurationDescriptorValueNotify
		case len(data) > 0 && data[0]&0x02 != 0:
			v = gatt.GattClientCharacteristicConfigurationDescriptorValueIndicate
		}
		return c.configure(vh, ch, v)
	}
	d, err := c.desc(h)
	if err != nil {
		return err
	}
	b, err := newBuffer(data)
	if err != nil {
		return err
	}
	defer b.Release()
	return writeResult(d.WriteValueWithResultAsync(b))
}

// Notify enables notifications, or indications on a characteristic that only indicates.
func (c *conn) Notify(h uint16, enable bool) error {
	if err := c.begin(); err != nil {
		return err
	}
	defer c.end()
	ch, err := c.char(h)
	if err != nil {
		return err
	}
	v := gatt.GattClientCharacteristicConfigurationDescriptorValueNone
	if enable {
		v = gatt.GattClientCharacteristicConfigurationDescriptorValueNotify
		if ch.props&uint32(gatt.GattCharacteristicPropertiesNotify) == 0 &&
			ch.props&uint32(gatt.GattCharacteristicPropertiesIndicate) != 0 {
			v = gatt.GattClientCharacteristicConfigurationDescriptorValueIndicate
		}
	}
	return c.configure(h, ch, v)
}

// configure writes the CCCD of a characteristic unless it already holds v, and has its values delivered.
// Home Assistant enables a notification twice, by request and by writing the CCCD.
func (c *conn) configure(h uint16, ch *char, v gatt.GattClientCharacteristicConfigurationDescriptorValue) error {
	if same, err := c.subscribe(h, ch, v); same || err != nil {
		return err
	}
	if err := writeResult(ch.c.WriteClientCharacteristicConfigurationDescriptorWithResultAsync(v)); err != nil {
		return err
	}
	c.setCfg(ch, v)
	return nil
}

// subscribe reports whether the CCCD of ch already holds v, in which case there is nothing to write, and
// otherwise registers the ValueChanged handler if v enables notifications and none is registered yet.
func (c *conn) subscribe(h uint16, ch *char, v gatt.GattClientCharacteristicConfigurationDescriptorValue) (same bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch.cfg == v {
		return true, nil
	}
	if ch.h == nil && v != gatt.GattClientCharacteristicConfigurationDescriptorValueNone {
		ch.h = eventHandler(gatt.SignatureGattCharacteristic, gatt.SignatureGattValueChangedEventArgs,
			func(_, args unsafe.Pointer) { c.valueChanged(h, (*gatt.GattValueChangedEventArgs)(args)) })
		tok, err := ch.c.AddValueChanged(ch.h)
		if err != nil {
			ch.h.Release()
			ch.h = nil
			return false, err
		}
		ch.tok = tok
	}
	return false, nil
}

// setCfg records v as the value the CCCD of ch holds.
func (c *conn) setCfg(ch *char, v gatt.GattClientCharacteristicConfigurationDescriptorValue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch.cfg = v
}

func (c *conn) valueChanged(h uint16, args *gatt.GattValueChangedEventArgs) {
	if !c.live() {
		return
	}
	b, err := args.GetCharacteristicValue()
	if err != nil || b == nil {
		return
	}
	data, err := appendBuffer(nil, b)
	b.Release()
	if err == nil {
		c.ev.Notification(h, data)
	}
}

// Pair bonds with the device, accepting the pairing kinds that need no input (Just Works, and showing or
// confirming a passkey, which an ESP32 proxy also accepts blindly).
func (c *conn) Pair() error {
	if err := c.begin(); err != nil {
		return err
	}
	defer c.end()
	return withPairing(c.dev, func(pairing *enumeration.DeviceInformationPairing) error {
		if ok, err := pairing.GetIsPaired(); err == nil && ok {
			return nil
		}
		custom, err := pairing.GetCustom()
		if err != nil {
			return err
		}
		defer custom.Release()
		h := eventHandler(enumeration.SignatureDeviceInformationCustomPairing,
			enumeration.SignatureDevicePairingRequestedEventArgs, func(_, args unsafe.Pointer) {
				(*enumeration.DevicePairingRequestedEventArgs)(args).Accept()
			})
		defer h.Release()
		tok, err := custom.AddPairingRequested(h)
		if err != nil {
			return err
		}
		defer custom.RemovePairingRequested(tok)
		op, err := custom.PairAsync(enumeration.DevicePairingKindsConfirmOnly | enumeration.DevicePairingKindsDisplayPin |
			enumeration.DevicePairingKindsConfirmPinMatch)
		return pairingResult[enumeration.DevicePairingResult](op, err, enumeration.SignatureDevicePairingResult, "pairing",
			enumeration.DevicePairingResultStatusPaired, enumeration.DevicePairingResultStatusAlreadyPaired)
	})
}

// withPairing runs fn with the pairing object of dev, which holds the bond state and the pairing calls.
func withPairing(dev *bluetooth.BluetoothLEDevice, fn func(*enumeration.DeviceInformationPairing) error) error {
	info, err := dev.GetDeviceInformation()
	if err != nil {
		return err
	}
	defer info.Release()
	pairing, err := info.GetPairing()
	if err != nil {
		return err
	}
	defer pairing.Release()
	return fn(pairing)
}

// statusResult is a WinRT result object that reports a status of enum type S.
type statusResult[S ~int32] interface {
	GetStatus() (S, error)
	Release() int32
}

// pairingResult awaits a pairing or unpairing operation (op, started with err) for pairTimeout and checks that
// its status is one of done; any other status fails with the status number and what was attempted. The result
// type R is given at the call.
func pairingResult[R any, S ~int32, PR interface {
	*R
	statusResult[S]
}](op *foundation.IAsyncOperation, err error, sig, what string, done ...S) error {
	if err != nil {
		return err
	}
	p, err := await(op, sig, pairTimeout)
	if err != nil {
		return err
	}
	res := PR(p)
	defer res.Release()
	st, err := res.GetStatus()
	if err != nil {
		return err
	}
	if slices.Contains(done, st) {
		return nil
	}
	return fmt.Errorf("%s failed with status %d", what, st)
}

func (c *conn) Disconnect() error {
	c.release()
	return nil
}

// release closes every WinRT object of the connection, which lets Windows drop the link, and frees the
// address. It waits for calls in flight and reports whether this call did the teardown, which runs once.
func (c *conn) release() bool {
	if !c.close() {
		return false
	}
	c.ops.Lock()
	defer c.ops.Unlock()
	c.a.unclaim(c)
	c.free()
	return true
}

// close marks the connection closed, so no new call starts, and reports whether this call was the one to
// do it, which makes it the one to free the WinRT objects.
func (c *conn) close() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.closed = true
	return true
}

// free closes the WinRT objects of the connection; it is called with c.ops held exclusively. A second call
// does nothing.
func (c *conn) free() {
	if req := c.takeConnParams(); req != nil {
		req.Close()
		req.Release()
	}
	for h, ch := range c.chars {
		if ch.h != nil {
			ch.c.RemoveValueChanged(ch.tok)
			ch.h.Release()
		}
		ch.c.Release()
		delete(c.chars, h)
	}
	for h, d := range c.descs {
		d.Release()
		delete(c.descs, h)
	}
	for _, s := range c.svcObjs {
		s.Close()
		s.Release()
	}
	c.svcObjs = nil
	if c.session != nil {
		c.session.SetMaintainConnection(false)
		c.session.Close()
		c.session.Release()
		c.session = nil
	}
	if c.dev != nil {
		if c.statusH != nil {
			c.dev.RemoveConnectionStatusChanged(c.statusTok)
			c.statusH.Release()
			c.statusH = nil
		}
		c.dev.Close()
		c.dev.Release()
		c.dev = nil
	}
}

// takeConnParams hands over the connection parameter request in effect, if any, leaving none.
func (c *conn) takeConnParams() *bluetooth.BluetoothLEPreferredConnectionParametersRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	req := c.cpReq
	c.cpReq = nil
	return req
}

// Unpair removes the bond with a device, dropping any connection to it without reporting it.
func (a *Adapter) Unpair(addr uint64) error {
	if c := a.connTo(addr); c != nil {
		c.release()
	}
	op, err := bluetooth.BluetoothLEDeviceFromBluetoothAddressAsync(addr)
	if err != nil {
		return err
	}
	p, err := await(op, bluetooth.SignatureBluetoothLEDevice, callTimeout)
	if err != nil {
		return err
	}
	if p == nil {
		return nil // Windows knows nothing of the device, so there is no bond
	}
	dev := (*bluetooth.BluetoothLEDevice)(p)
	defer dev.Release()
	defer dev.Close()
	return withPairing(dev, func(pairing *enumeration.DeviceInformationPairing) error {
		if ok, err := pairing.GetIsPaired(); err == nil && !ok {
			return nil
		}
		op, err := pairing.UnpairAsync()
		return pairingResult[enumeration.DeviceUnpairingResult](op, err, enumeration.SignatureDeviceUnpairingResult, "unpairing",
			enumeration.DeviceUnpairingResultStatusUnpaired, enumeration.DeviceUnpairingResultStatusAlreadyUnpaired)
	})
}
