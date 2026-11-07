package kernel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// GATT attribute types.
const (
	uuidPrimary        = 0x2800
	uuidSecondary      = 0x2801
	uuidInclude        = 0x2802
	uuidCharacteristic = 0x2803
	uuidCCCD           = 0x2902
	uuidServiceChanged = 0x2a05

	propNotify   = 0x10
	propIndicate = 0x20
)

// uuidFromLE reads a 16-bit or 128-bit UUID in ATT's little-endian order.
func uuidFromLE(b []byte) ble.UUID {
	if len(b) == 2 {
		return ble.UUID{Hi: uint64(binary.LittleEndian.Uint16(b))<<32 | 0x1000, Lo: 0x800000805f9b34fb}
	}
	return ble.UUID{Hi: binary.LittleEndian.Uint64(b[8:]), Lo: binary.LittleEndian.Uint64(b)}
}

func isNotFound(err error) bool {
	var ae *ATTError
	return errors.As(err, &ae) && ae.Code == attErrNotFound
}

// Services implements ble.Conn. It discovers the database the first time it is called on a connection, and
// again after the peripheral indicates Service Changed. Home Assistant keeps its own copy and usually never
// asks, which is why Connect does not discover anything.
func (c *attClient) Services() ([]ble.Service, error) {
	c.gattMu.Lock()
	defer c.gattMu.Unlock()
	changes := c.changes.Load() // read first: a change during discovery makes the result out of date
	if c.services != nil && c.servicesGen == changes {
		return c.services, nil
	}
	svcs, err := c.discover()
	if err != nil {
		return nil, err
	}
	c.services, c.servicesGen = svcs, changes
	return svcs, nil
}

func (c *attClient) discover() ([]ble.Service, error) {
	type group struct {
		svc ble.Service
		end uint16
	}
	var groups []group
	for _, kind := range []uint16{uuidPrimary, uuidSecondary} {
		start := uint16(1)
		for {
			req := binary.LittleEndian.AppendUint16([]byte{opReadGroupReq}, start)
			req = binary.LittleEndian.AppendUint16(req, 0xffff)
			req = binary.LittleEndian.AppendUint16(req, kind)
			rsp, err := c.request(req, opReadGroupRsp)
			if isNotFound(err) {
				break
			}
			var ae *ATTError
			if kind == uuidSecondary && errors.As(err, &ae) && ae.Code == attErrUnsupportedType {
				break // a device without secondary services may say so this way
			}
			if err != nil {
				return nil, err
			}
			size, entries, err := entriesOf(rsp, 6, 20)
			if err != nil {
				return nil, fmt.Errorf("service discovery: %w", err)
			}
			last := start
			for off := 0; off < len(entries); off += size {
				e := entries[off : off+size]
				h, end := binary.LittleEndian.Uint16(e), binary.LittleEndian.Uint16(e[2:])
				if h < start || end < h {
					return nil, fmt.Errorf("service discovery: bad handles %#x to %#x", h, end)
				}
				groups = append(groups, group{ble.Service{UUID: uuidFromLE(e[4:]), Handle: h}, end})
				last = end
			}
			if last == 0xffff {
				break
			}
			start = last + 1
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].svc.Handle < groups[j].svc.Handle })
	svcs := make([]ble.Service, 0, len(groups))
	for _, g := range groups {
		chars, err := c.discoverChars(g.svc.Handle, g.end)
		if err != nil {
			return nil, err
		}
		g.svc.Characteristics = chars
		svcs = append(svcs, g.svc)
	}
	return svcs, nil
}

func (c *attClient) discoverChars(first, end uint16) ([]ble.Characteristic, error) {
	var chars []ble.Characteristic
	var decls []uint16
	start := first
	for start <= end {
		req := binary.LittleEndian.AppendUint16([]byte{opReadTypeReq}, start)
		req = binary.LittleEndian.AppendUint16(req, end)
		req = binary.LittleEndian.AppendUint16(req, uuidCharacteristic)
		rsp, err := c.request(req, opReadTypeRsp)
		if isNotFound(err) {
			break
		}
		if err != nil {
			return nil, err
		}
		size, entries, err := entriesOf(rsp, 7, 21)
		if err != nil {
			return nil, fmt.Errorf("characteristic discovery: %w", err)
		}
		last := start
		for off := 0; off < len(entries); off += size {
			e := entries[off : off+size]
			decl := binary.LittleEndian.Uint16(e)
			if decl < start || decl > end {
				return nil, fmt.Errorf("characteristic discovery: bad handle %#x", decl)
			}
			u := uuidFromLE(e[5:])
			value := binary.LittleEndian.Uint16(e[3:])
			chars = append(chars, ble.Characteristic{UUID: u, Handle: value, Properties: uint32(e[2])})
			decls = append(decls, decl)
			if s, ok := u.Short(); ok && s == uuidServiceChanged {
				c.serviceChanged.Store(uint32(value))
			}
			last = decl
		}
		if last == 0xffff {
			break
		}
		start = last + 1
	}
	for i := range chars {
		last := end
		if i+1 < len(chars) {
			last = decls[i+1] - 1
		}
		descs, err := c.discoverDescs(chars[i].Handle+1, last)
		if err != nil {
			return nil, err
		}
		chars[i].Descriptors = descs
	}
	return chars, nil
}

func (c *attClient) discoverDescs(start, end uint16) ([]ble.Descriptor, error) {
	var descs []ble.Descriptor
	for start != 0 && start <= end {
		found, err := c.findInfo(start, end)
		if isNotFound(err) {
			break
		}
		if err != nil {
			return nil, err
		}
		for _, d := range found {
			descs = append(descs, d)
			start = d.Handle
		}
		if start == 0xffff {
			break
		}
		start++
	}
	return descs, nil
}

// findInfo sends one Find Information request. The attributes it returns all have handles from start on.
func (c *attClient) findInfo(start, end uint16) ([]ble.Descriptor, error) {
	req := binary.LittleEndian.AppendUint16([]byte{opFindInfoReq}, start)
	req = binary.LittleEndian.AppendUint16(req, end)
	rsp, err := c.request(req, opFindInfoRsp)
	if err != nil {
		return nil, err
	}
	if len(rsp) < 2 {
		return nil, errors.New("descriptor discovery: short response")
	}
	size := 4
	if rsp[1] == 2 {
		size = 18
	}
	entries := rsp[2:]
	if len(entries) == 0 || len(entries)%size != 0 {
		return nil, errors.New("descriptor discovery: bad response length")
	}
	var found []ble.Descriptor
	// The handles must go up, and the next one after 0xffff is out of range rather than 0: an int keeps the
	// bound from wrapping, which would otherwise let the callers start over from handle 1 forever.
	next := int(start)
	for off := 0; off < len(entries); off += size {
		h := binary.LittleEndian.Uint16(entries[off:])
		if int(h) < next || h > end {
			return nil, fmt.Errorf("descriptor discovery: bad handle %#x", h)
		}
		next = int(h) + 1
		found = append(found, ble.Descriptor{UUID: uuidFromLE(entries[off+2 : off+size]), Handle: h})
	}
	return found, nil
}

// entriesOf checks the length byte of a Read By Type or Read By Group Type response against the two sizes an
// entry can have, for a 16-bit and a 128-bit UUID.
func entriesOf(rsp []byte, short, long int) (int, []byte, error) {
	if len(rsp) < 2 {
		return 0, nil, errors.New("short response")
	}
	size := int(rsp[1])
	entries := rsp[2:]
	if (size != short && size != long) || len(entries) == 0 || len(entries)%size != 0 {
		return 0, nil, fmt.Errorf("bad response: entries of %d bytes in %d", size, len(entries))
	}
	return size, entries, nil
}

// Notify implements ble.Conn by writing the characteristic's CCCD. Home Assistant asks for this on a
// connection where it has not discovered anything, since it keeps its own copy of the database, so the CCCD
// is found by looking at the attributes that follow the value, up to the next declaration.
func (c *attClient) Notify(h uint16, enable bool) error {
	cccd, err := c.findCCCD(h)
	if err != nil {
		if !enable {
			return nil
		}
		return err
	}
	value := []byte{0, 0}
	if enable {
		value[0] = 1
		// The declaration comes right before the value, and says whether the characteristic indicates.
		if decl, err := c.request(binary.LittleEndian.AppendUint16([]byte{opReadReq}, h-1), opReadRsp); err == nil &&
			len(decl) >= 2 && decl[1]&propNotify == 0 && decl[1]&propIndicate != 0 {
			value[0] = 2
		}
	}
	return c.Write(cccd, value, true)
}

func (c *attClient) findCCCD(h uint16) (uint16, error) {
	start := h + 1
	for start != 0 {
		found, err := c.findInfo(start, 0xffff)
		if isNotFound(err) {
			break
		}
		if err != nil {
			return 0, err
		}
		for _, d := range found {
			s, ok := d.UUID.Short()
			switch {
			case ok && s == uuidCCCD:
				return d.Handle, nil
			case ok && (s == uuidCharacteristic || s == uuidPrimary || s == uuidSecondary || s == uuidInclude):
				return 0, fmt.Errorf("characteristic %#04x has no CCCD", h)
			}
			start = d.Handle + 1
		}
	}
	return 0, fmt.Errorf("characteristic %#04x has no CCCD", h)
}
