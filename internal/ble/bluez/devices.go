//go:build linux

package bluez

import (
	"github.com/godbus/dbus/v5"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// The devices BlueZ reports: a cache of each one's Device1 properties, kept up to date from signals, that the
// scanner's advertisements are rebuilt from while they do not come from the HCI monitor (see raw.go).

// device caches the Device1 properties an advertisement is rebuilt from.
type device struct {
	addr     uint64
	addrType uint32
	rssi     int16 // 0 until BlueZ reports one, so cached devices that are out of range stay silent
	name     string
	txPower  *int16
	flags    []byte
	uuids    []string
	mfr      map[uint16][]byte
	svcData  map[string][]byte
}

// evictDevice forgets one device that is not connected; which one is left to chance, so a flood of new
// addresses cannot keep a real device out for long. Called with a.mu held.
func (a *Adapter) evictDevice() {
	for p := range a.devices {
		if a.conns[p] == nil {
			delete(a.devices, p)
			return
		}
	}
}

// updateDevice merges Device1 properties into the cache and, if asked and scanning, returns an advertisement
// for the caller to deliver once it has released a.mu (see emit). While raw advertisements flow, the cache
// is not used for advertisements and its data maps are not rebuilt; they are cleared instead, so a fallback
// does not send stale data. Called with a.mu held.
func (a *Adapter) updateDevice(p dbus.ObjectPath, props map[string]dbus.Variant, emit bool) pending {
	d := a.devices[p]
	if d == nil {
		if len(a.devices) >= maxDevices {
			a.evictDevice()
		}
		d = &device{}
		a.devices[p] = d
	}
	fresh, cache := false, !a.rawProven()
	for k, v := range props {
		switch k {
		case "Address":
			if s, ok := v.Value().(string); ok {
				d.addr, _ = ble.ParseMAC(s)
			}
		case "AddressType":
			if s, _ := v.Value().(string); s == "random" {
				d.addrType = ble.AddrRandom
			} else {
				d.addrType = ble.AddrPublic
			}
		case "RSSI":
			if r, ok := v.Value().(int16); ok {
				d.rssi = r
				fresh = true
			}
		case "Name":
			d.name, _ = v.Value().(string)
		case "TxPower":
			if t, ok := v.Value().(int16); ok {
				d.txPower = &t
			}
		case "AdvertisingFlags":
			d.flags, _ = v.Value().([]byte)
		case "UUIDs":
			d.uuids, _ = v.Value().([]string)
		case "ManufacturerData":
			d.mfr = nil
			if m, ok := v.Value().(map[uint16]dbus.Variant); ok && cache {
				d.mfr = make(map[uint16][]byte, len(m))
				for id, data := range m {
					d.mfr[id], _ = data.Value().([]byte)
				}
			}
			fresh = true
		case "ServiceData":
			d.svcData = nil
			if m, ok := v.Value().(map[string]dbus.Variant); ok && cache {
				d.svcData = make(map[string][]byte, len(m))
				for u, data := range m {
					d.svcData[u], _ = data.Value().([]byte)
				}
			}
			fresh = true
		}
	}
	if emit && fresh {
		return a.emit(d)
	}
	return pending{}
}

// adData rebuilds raw AD structures from the cached properties. BlueZ only exposes the parsed form, so field
// order and the split between advertisement and scan response are not the original; Home Assistant parses
// the fields, so that does not matter. Fields that do not fit in ble.MaxAdvert bytes are dropped, least useful
// last.
func (d *device) adData() []byte {
	out := make([]byte, 0, ble.MaxAdvert)
	add := func(typ byte, payload []byte) {
		if len(payload) > 0 {
			out = ble.AppendAD(out, typ, payload)
		}
	}
	if len(d.flags) > 0 {
		add(0x01, d.flags)
	}
	for id, data := range d.mfr {
		add(0xff, append([]byte{byte(id), byte(id >> 8)}, data...))
	}
	for u, data := range d.svcData {
		uu, err := ble.ParseUUID(u)
		if err != nil {
			continue
		}
		if s, ok := uu.Short(); ok && s <= 0xffff {
			add(0x16, append([]byte{byte(s), byte(s >> 8)}, data...))
		} else if ok {
			add(0x20, append([]byte{byte(s), byte(s >> 8), byte(s >> 16), byte(s >> 24)}, data...))
		} else {
			add(0x21, append(uu.AppendLE(nil), data...))
		}
	}
	var u16, u128 []byte
	for _, u := range d.uuids {
		uu, err := ble.ParseUUID(u)
		if err != nil {
			continue
		}
		if s, ok := uu.Short(); ok && s <= 0xffff {
			u16 = append(u16, byte(s), byte(s>>8))
		} else {
			u128 = uu.AppendLE(u128)
		}
	}
	add(0x03, u16)
	add(0x07, u128)
	add(0x09, []byte(d.name))
	if d.txPower != nil {
		add(0x0a, []byte{byte(int8(*d.txPower))})
	}
	return out
}
