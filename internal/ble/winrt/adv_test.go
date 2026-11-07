package winrt

import (
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
)

func TestSortServices(t *testing.T) {
	s := []ble.Service{
		{Handle: 20, Characteristics: []ble.Characteristic{{Handle: 24}, {Handle: 22, Descriptors: []ble.Descriptor{{Handle: 27}, {Handle: 23}}}}},
		{Handle: 1},
	}
	sortServices(s)
	if s[0].Handle != 1 || s[1].Characteristics[0].Handle != 22 || s[1].Characteristics[0].Descriptors[0].Handle != 23 {
		t.Fatalf("not sorted: %+v", s)
	}
}
