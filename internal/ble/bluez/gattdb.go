//go:build linux

package bluez

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// The remote GATT database: BlueZ's object tree under the device, read into the service list reported to the
// caller and the maps the GATT operations find an object by handle through.

// handleOf reads the attribute handle from the last path element (service000a, char000b, desc000d).
func handleOf(p dbus.ObjectPath) (uint16, bool) {
	s := string(p)
	last := s[strings.LastIndex(s, "/")+1:]
	for _, prefix := range []string{"service", "char", "desc"} {
		if strings.HasPrefix(last, prefix) {
			v, err := strconv.ParseUint(last[len(prefix):], 16, 16)
			return uint16(v), err == nil
		}
	}
	return 0, false
}

var charFlags = map[string]uint32{
	"broadcast": 0x01, "read": 0x02, "write-without-response": 0x04, "write": 0x08, "notify": 0x10,
	"indicate": 0x20, "authenticated-signed-writes": 0x40, "extended-properties": 0x80,
}

// loadGATT reads BlueZ's object tree into the connection's GATT database.
func (c *conn) loadGATT() error {
	objs, err := c.a.managedObjects()
	if err != nil {
		return fmt.Errorf("gatt objects: %w", err)
	}
	c.gattDB = buildGATT(c.path, objs)
	return nil
}

// gattDB is the remote GATT database of a connection, with the D-Bus object behind every handle.
type gattDB struct {
	mtu       uint32
	services  []ble.Service
	chars     map[uint16]dbus.ObjectPath // by value handle
	descs     map[uint16]dbus.ObjectPath
	cccdOwner map[uint16]uint16 // CCCD descriptor handle -> characteristic value handle
}

// charValueHandle maps a characteristic object path to its value handle. BlueZ names the object after the
// characteristic declaration (char000b), while ESPHome reports the value handle that follows it (0x000c).
// Home Assistant keeps one GATT cache per device across proxies, so the handles must match an ESP32's.
func charValueHandle(p dbus.ObjectPath) (uint16, bool) {
	h, ok := handleOf(p)
	return h + 1, ok
}

// buildGATT builds the service list and handle maps from the objects under a device path.
func buildGATT(dev dbus.ObjectPath, objs map[dbus.ObjectPath]map[string]map[string]dbus.Variant) gattDB {
	prefix := string(dev) + "/"
	svcs := map[dbus.ObjectPath]*ble.Service{}
	type charEntry struct {
		svc dbus.ObjectPath
		ch  ble.Characteristic
	}
	chars := map[dbus.ObjectPath]*charEntry{}
	db := gattDB{mtu: 23, chars: map[uint16]dbus.ObjectPath{}, descs: map[uint16]dbus.ObjectPath{},
		cccdOwner: map[uint16]uint16{}}
	uuidOf := func(props map[string]dbus.Variant) ble.UUID {
		s, _ := props["UUID"].Value().(string)
		u, _ := ble.ParseUUID(s)
		return u
	}
	for p, ifs := range objs {
		if !strings.HasPrefix(string(p), prefix) {
			continue
		}
		if props, ok := ifs[ifService]; ok {
			if h, ok := handleOf(p); ok {
				svcs[p] = &ble.Service{UUID: uuidOf(props), Handle: h}
			}
		}
		if props, ok := ifs[ifChar]; ok {
			h, ok := charValueHandle(p)
			if !ok {
				continue
			}
			ch := ble.Characteristic{UUID: uuidOf(props), Handle: h}
			flags, _ := props["Flags"].Value().([]string)
			for _, f := range flags {
				ch.Properties |= charFlags[f]
			}
			if m, ok := props["MTU"].Value().(uint16); ok && uint32(m) > db.mtu {
				db.mtu = uint32(m)
			}
			svc, _ := props["Service"].Value().(dbus.ObjectPath)
			chars[p] = &charEntry{svc: svc, ch: ch}
			db.chars[h] = p
		}
	}
	for p, ifs := range objs {
		props, ok := ifs[ifDesc]
		if !ok || !strings.HasPrefix(string(p), prefix) {
			continue
		}
		h, ok := handleOf(p)
		if !ok {
			continue
		}
		chPath, _ := props["Characteristic"].Value().(dbus.ObjectPath)
		ce := chars[chPath]
		if ce == nil {
			continue
		}
		d := ble.Descriptor{UUID: uuidOf(props), Handle: h}
		ce.ch.Descriptors = append(ce.ch.Descriptors, d)
		db.descs[h] = p
		if s, ok := d.UUID.Short(); ok && s == 0x2902 {
			db.cccdOwner[h] = ce.ch.Handle
		}
	}
	for _, ce := range chars {
		if s := svcs[ce.svc]; s != nil {
			sortDescs(ce.ch.Descriptors)
			s.Characteristics = append(s.Characteristics, ce.ch)
		}
	}
	for _, s := range svcs {
		sortChars(s.Characteristics)
		db.services = append(db.services, *s)
	}
	sortServices(db.services)
	return db
}
