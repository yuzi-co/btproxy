package api

import (
	"testing"

	"btproxy/internal/ble"
)

// TestEncServicesUUID checks that each attribute carries one UUID form: short_uuid for base UUIDs when the
// client reads it (API 1.12+), the 128-bit pair otherwise.
func TestEncServicesUUID(t *testing.T) {
	battery := ble.UUID{Hi: 0x180f<<32 | 0x1000, Lo: 0x800000805f9b34fb}
	custom, _ := ble.ParseUUID("6e400001-b5a3-f393-e0a9-e50e24dcca9e")
	svcs := []ble.Service{{UUID: battery, Handle: 1}, {UUID: custom, Handle: 0x10}}
	for _, tt := range []struct {
		efficient bool
		short     []uint64 // short_uuid per service, 0 if absent
		long      []int    // number of 128-bit halves per service
	}{
		{true, []uint64{0x180f, 0}, []int{0, 2}},
		{false, []uint64{0, 0}, []int{2, 2}},
	} {
		top, err := fields(encServices(1, svcs, tt.efficient))
		if err != nil {
			t.Fatal(err)
		}
		i := 0
		for _, f := range top {
			if f.num != 2 {
				continue
			}
			sf, err := fields(f.data)
			if err != nil {
				t.Fatal(err)
			}
			halves := 0
			for _, g := range sf {
				if g.num == 1 {
					halves++
				}
			}
			if got := fieldValue(sf, 4); got != tt.short[i] || halves != tt.long[i] {
				t.Errorf("efficient=%v service %d: short_uuid %#x, %d halves", tt.efficient, i, got, halves)
			}
			i++
		}
		if i != 2 {
			t.Errorf("efficient=%v: %d services", tt.efficient, i)
		}
	}
	if efficientUUIDs(1, 11) || !efficientUUIDs(1, 12) || !efficientUUIDs(2, 0) {
		t.Error("efficientUUIDs version check")
	}
}
