// Package winrt is the Windows backend: it drives the Bluetooth LE radio through the WinRT APIs
// (Windows.Devices.Bluetooth), the same ones Bleak uses on Windows. Bindings are in the gen subpackage.
package winrt

import (
	"slices"

	"github.com/yuzi-co/btproxy/internal/ble"
)

// maxAdvert is the most AD data an ESPHome raw advertisement carries (advertisement plus scan response).
const maxAdvert = 62

// appendAD appends one AD structure (length, type, data) to dst, as it was on air. WinRT hands advertisements
// over parsed into data sections; this puts them back together. A section that would take dst past maxAdvert
// is left out, so the result always parses.
func appendAD(dst []byte, typ byte, data []byte) []byte {
	if len(data) > 254 || len(dst)+2+len(data) > maxAdvert {
		return dst
	}
	dst = append(dst, byte(len(data)+1), typ)
	return append(dst, data...)
}

// sortServices orders services, characteristics and descriptors by handle, as an ESP32 lists them.
func sortServices(s []ble.Service) {
	slices.SortFunc(s, func(a, b ble.Service) int { return int(a.Handle) - int(b.Handle) })
	for _, svc := range s {
		slices.SortFunc(svc.Characteristics, func(a, b ble.Characteristic) int { return int(a.Handle) - int(b.Handle) })
		for _, ch := range svc.Characteristics {
			slices.SortFunc(ch.Descriptors, func(a, b ble.Descriptor) int { return int(a.Handle) - int(b.Handle) })
		}
	}
}
