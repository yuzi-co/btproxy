//go:build linux

package bluez

import (
	"slices"

	"btproxy/internal/ble"
)

func sortServices(s []ble.Service) {
	slices.SortFunc(s, func(a, b ble.Service) int { return int(a.Handle) - int(b.Handle) })
}

func sortChars(c []ble.Characteristic) {
	slices.SortFunc(c, func(a, b ble.Characteristic) int { return int(a.Handle) - int(b.Handle) })
}
