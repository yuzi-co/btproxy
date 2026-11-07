// Package winrt is the Windows backend: it drives the Bluetooth LE radio through the WinRT APIs
// (Windows.Devices.Bluetooth), the same ones Bleak uses on Windows. Bindings are in the gen subpackage.
package winrt

import (
	"slices"

	"github.com/yuzi-co/btproxy/internal/ble"
)

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
