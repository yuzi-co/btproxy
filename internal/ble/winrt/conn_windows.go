package winrt

import (
	"errors"
	"fmt"
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

	ops      sync.RWMutex // held shared by every call into the WinRT objects, exclusively to release them
	mu       sync.Mutex
	up       bool // reported connected to the caller
	closed   bool // no new calls
	released bool
}

type char struct {
	c     *gatt.GattCharacteristic
	props uint32
	h     *foundation.TypedEventHandler // ValueChanged handler, once notifications were enabled
	tok   foundation.EventRegistrationToken
	cfg   gatt.GattClientCharacteristicConfigurationDescriptorValue // last value written to the CCCD
}

func (a *Adapter) Connect(addr uint64, addrType uint32, ev ble.Events) (ble.Conn, error) {
	c := &conn{a: a, addr: addr, ev: ev, upCh: make(chan struct{}, 1), chars: map[uint16]*char{},
		descs: map[uint16]*gatt.GattDescriptor{}, cccd: map[uint16]uint16{}}
	a.mu.Lock()
	if _, busy := a.conns[addr]; busy {
		a.mu.Unlock()
		return nil, errors.New("connection already in progress")
	}
	a.conns[addr] = c
	cacheMode := bluetooth.BluetoothCacheModeCached
	if a.uncached[addr] {
		cacheMode = bluetooth.BluetoothCacheModeUncached
	}
	a.mu.Unlock()
	// Until the services are resolved or the connect fails: the link is most fragile in its first
	// connection events, and service discovery is a burst of traffic right after.
	defer a.pauseScan()()

	// One deadline for the link to come up, retries included.
	deadline := time.Now().Add(connectTimeout)
	log := a.log.With("addr", ble.FormatMAC(addr))
	c.ops.RLock() // an Unpair while connecting waits for this
	retries, err := a.connRetry.Do(func() error {
		return c.connect(addrType, cacheMode, deadline)
	}, func(err error) bool {
		if !radioFailure(err) {
			log.Debug("connect failed, not a radio failure", "err", err)
			return false
		}
		return c.reset()
	}, ev.Withdrawn, func(n int, err error) {
		log.Debug("connect failed on the radio, retrying", "retry", n, "err", err)
	})
	c.ops.RUnlock()
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
		a.mu.Lock()
		delete(a.uncached, addr)
		a.mu.Unlock()
	}
	return c, nil
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
	c.mu.Lock()
	closed := c.closed
	lost := closed || !c.connected()
	c.up = !lost
	c.mu.Unlock()
	if closed {
		return errors.New("connection closed while connecting")
	}
	if lost {
		return errLinkLost
	}
	return nil
}

// reset frees the WinRT objects of a failed attempt so the next one starts afresh, and reports whether the
// connection may be retried (it was not released meanwhile). It is called with c.ops held shared.
func (c *conn) reset() bool {
	c.ops.RUnlock()
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
	c.ops.RLock()
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
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
	c.mu.Lock()
	report := c.up && !c.closed
	c.mu.Unlock()
	if report {
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
	if err != nil {
		return err
	}
	p, err := await(op, gatt.SignatureGattDeviceServicesResult, callTimeout)
	if err != nil {
		return err
	}
	res := (*gatt.GattDeviceServicesResult)(p)
	defer res.Release()
	if err := commStatus(res.GetStatus()); err != nil {
		if errors.Is(err, errProtocol) {
			return protocolError(res.GetProtocolError())
		}
		return err
	}
	v, err := res.GetServices()
	if err != nil {
		return err
	}
	defer v.Release()
	err = eachItem(v, func(p unsafe.Pointer) error {
		s := (*gatt.GattDeviceService)(p)
		c.svcObjs = append(c.svcObjs, s) // kept and closed with the connection
		return c.discoverService(s, mode)
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
	if err != nil {
		return err
	}
	p, err := await(op, gatt.SignatureGattCharacteristicsResult, callTimeout)
	if err != nil {
		return err
	}
	res := (*gatt.GattCharacteristicsResult)(p)
	defer res.Release()
	if err := commStatus(res.GetStatus()); err != nil {
		// A service the stack may not open (HID, for one) answers AccessDenied: list it without characteristics.
		if errors.Is(err, errAccessDenied) {
			c.services = append(c.services, svc)
			return nil
		}
		return fmt.Errorf("characteristics of %s: %w", svc.UUID, err)
	}
	v, err := res.GetCharacteristics()
	if err != nil {
		return err
	}
	defer v.Release()
	err = eachItem(v, func(p unsafe.Pointer) error {
		ch := (*gatt.GattCharacteristic)(p)
		info, err := c.discoverChar(ch, mode)
		if err != nil {
			ch.Release()
			return err
		}
		svc.Characteristics = append(svc.Characteristics, info)
		return nil
	})
	c.services = append(c.services, svc)
	return err
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
	if err != nil {
		return info, err
	}
	p, err := await(op, gatt.SignatureGattDescriptorsResult, callTimeout)
	if err != nil {
		return info, err
	}
	res := (*gatt.GattDescriptorsResult)(p)
	defer res.Release()
	if err := commStatus(res.GetStatus()); err != nil {
		return info, fmt.Errorf("descriptors of %s: %w", info.UUID, err)
	}
	v, err := res.GetDescriptors()
	if err != nil {
		return info, err
	}
	defer v.Release()
	err = eachItem(v, func(p unsafe.Pointer) error {
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
	if err != nil {
		return info, err
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

var (
	errUnreachable  = errors.New("device unreachable")
	errProtocol     = errors.New("GATT protocol error")
	errAccessDenied = errors.New("access denied")
)

func commStatus(s gatt.GattCommunicationStatus, err error) error {
	if err != nil {
		return err
	}
	switch s {
	case gatt.GattCommunicationStatusSuccess:
		return nil
	case gatt.GattCommunicationStatusUnreachable:
		return errUnreachable
	case gatt.GattCommunicationStatusProtocolError:
		return errProtocol
	case gatt.GattCommunicationStatusAccessDenied:
		return errAccessDenied
	}
	return fmt.Errorf("GATT status %d", s)
}

// begin starts a call into the connection's WinRT objects; end must follow once it is done.
func (c *conn) begin() error {
	c.ops.RLock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		c.ops.RUnlock()
		return ble.ErrNotConnected
	}
	return nil
}

func (c *conn) end() { c.ops.RUnlock() }

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
	if err != nil {
		return nil, err
	}
	p, err := await(op, gatt.SignatureGattReadResult, callTimeout)
	if err != nil {
		return nil, err
	}
	res := (*gatt.GattReadResult)(p)
	defer res.Release()
	if err := commStatus(res.GetStatus()); err != nil {
		if errors.Is(err, errProtocol) {
			return nil, protocolError(res.GetProtocolError())
		}
		return nil, err
	}
	b, err := res.GetValue()
	if err != nil || b == nil {
		return nil, err
	}
	defer b.Release()
	return appendBuffer(nil, b)
}

// writeResult unpacks an IAsyncOperation<GattWriteResult>.
func writeResult(op *foundation.IAsyncOperation, err error) error {
	if err != nil {
		return err
	}
	p, err := await(op, gatt.SignatureGattWriteResult, callTimeout)
	if err != nil {
		return err
	}
	res := (*gatt.GattWriteResult)(p)
	defer res.Release()
	if err := commStatus(res.GetStatus()); err != nil {
		if errors.Is(err, errProtocol) {
			return protocolError(res.GetProtocolError())
		}
		return err
	}
	return nil
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
	c.mu.Lock()
	if ch.cfg == v {
		c.mu.Unlock()
		return nil
	}
	if ch.h == nil && v != gatt.GattClientCharacteristicConfigurationDescriptorValueNone {
		ch.h = eventHandler(gatt.SignatureGattCharacteristic, gatt.SignatureGattValueChangedEventArgs,
			func(_, args unsafe.Pointer) { c.valueChanged(h, (*gatt.GattValueChangedEventArgs)(args)) })
		tok, err := ch.c.AddValueChanged(ch.h)
		if err != nil {
			ch.h.Release()
			ch.h = nil
			c.mu.Unlock()
			return err
		}
		ch.tok = tok
	}
	c.mu.Unlock()
	if err := writeResult(ch.c.WriteClientCharacteristicConfigurationDescriptorWithResultAsync(v)); err != nil {
		return err
	}
	c.mu.Lock()
	ch.cfg = v
	c.mu.Unlock()
	return nil
}

func (c *conn) valueChanged(h uint16, args *gatt.GattValueChangedEventArgs) {
	c.mu.Lock()
	up := c.up && !c.closed
	c.mu.Unlock()
	if !up {
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
	info, err := c.dev.GetDeviceInformation()
	if err != nil {
		return err
	}
	defer info.Release()
	pairing, err := info.GetPairing()
	if err != nil {
		return err
	}
	defer pairing.Release()
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
	if err != nil {
		return err
	}
	p, err := await(op, enumeration.SignatureDevicePairingResult, pairTimeout)
	if err != nil {
		return err
	}
	res := (*enumeration.DevicePairingResult)(p)
	defer res.Release()
	st, err := res.GetStatus()
	if err != nil {
		return err
	}
	if st != enumeration.DevicePairingResultStatusPaired && st != enumeration.DevicePairingResultStatusAlreadyPaired {
		return fmt.Errorf("pairing failed with status %d", st)
	}
	return nil
}

func (c *conn) Disconnect() error {
	c.release()
	return nil
}

// release closes every WinRT object of the connection, which lets Windows drop the link, and frees the
// address. It waits for calls in flight and reports whether this call did the teardown, which runs once.
func (c *conn) release() bool {
	c.mu.Lock()
	if c.closed && c.released {
		c.mu.Unlock()
		return false
	}
	c.closed = true
	c.released = true
	c.mu.Unlock()
	c.ops.Lock()
	defer c.ops.Unlock()

	c.a.mu.Lock()
	if c.a.conns[c.addr] == c {
		delete(c.a.conns, c.addr)
	}
	c.a.mu.Unlock()
	c.free()
	return true
}

// free closes the WinRT objects of the connection; it is called with c.ops held exclusively. A second call
// does nothing.
func (c *conn) free() {
	c.mu.Lock()
	req := c.cpReq
	c.cpReq = nil
	c.mu.Unlock()
	if req != nil {
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

// Unpair removes the bond with a device, dropping any connection to it without reporting it.
func (a *Adapter) Unpair(addr uint64) error {
	a.mu.Lock()
	c := a.conns[addr]
	a.mu.Unlock()
	if c != nil {
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
	if ok, err := pairing.GetIsPaired(); err == nil && !ok {
		return nil
	}
	op, err = pairing.UnpairAsync()
	if err != nil {
		return err
	}
	if p, err = await(op, enumeration.SignatureDeviceUnpairingResult, pairTimeout); err != nil {
		return err
	}
	res := (*enumeration.DeviceUnpairingResult)(p)
	defer res.Release()
	st, err := res.GetStatus()
	if err != nil {
		return err
	}
	if st != enumeration.DeviceUnpairingResultStatusUnpaired && st != enumeration.DeviceUnpairingResultStatusAlreadyUnpaired {
		return fmt.Errorf("unpairing failed with status %d", st)
	}
	return nil
}
