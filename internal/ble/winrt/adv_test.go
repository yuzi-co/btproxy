package winrt

import (
	"bytes"
	"testing"

	"btproxy/internal/ble"
)

func TestAppendAD(t *testing.T) {
	b := appendAD(nil, 0x01, []byte{0x06})
	b = appendAD(b, 0x09, []byte("abc"))
	if want := []byte{2, 0x01, 0x06, 4, 0x09, 'a', 'b', 'c'}; !bytes.Equal(b, want) {
		t.Fatalf("got % x, want % x", b, want)
	}
	// A section that does not fit is dropped whole; a later one that fits is kept.
	b = appendAD(b, 0xff, make([]byte, maxAdvert))
	b = appendAD(b, 0x0a, []byte{0xf4})
	if want := []byte{2, 0x01, 0x06, 4, 0x09, 'a', 'b', 'c', 2, 0x0a, 0xf4}; !bytes.Equal(b, want) {
		t.Fatalf("got % x, want % x", b, want)
	}
	full := appendAD(nil, 0xff, make([]byte, maxAdvert-2))
	if len(full) != maxAdvert {
		t.Fatalf("len %d, want %d", len(full), maxAdvert)
	}
	if got := appendAD(full, 0x01, nil); len(got) != maxAdvert {
		t.Fatalf("overflowed to %d", len(got))
	}
}

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
