package ble

import (
	"bytes"
	"testing"
)

func TestADs(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
		want []byte // the types seen
	}{
		{"complete", []byte{2, 0x01, 0x06, 4, 0x09, 'a', 'b', 'c'}, []byte{0x01, 0x09}},
		{"zero length stops", []byte{2, 0x01, 0x06, 0, 4, 0x09, 'a', 'b', 'c'}, []byte{0x01}},
		{"cut off stops", []byte{2, 0x01, 0x06, 5, 0x09, 'a'}, []byte{0x01}},
		{"length alone", []byte{2, 0x01, 0x06, 2}, []byte{0x01}},
		{"empty", nil, nil},
	} {
		var got []byte
		for typ := range ADs(c.data) {
			got = append(got, typ)
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("%s: types % x, want % x", c.name, got, c.want)
		}
	}
	// Leaving the loop ends the walk.
	n := 0
	for range ADs([]byte{2, 0x01, 0x06, 2, 0x0a, 4, 2, 0x0a, 4}) {
		if n++; n == 2 {
			break
		}
	}
	if n != 2 {
		t.Errorf("walked %d structures after the break, want 2", n)
	}
}

func TestWholeAD(t *testing.T) {
	name := []byte{5, 0x09, 'A', 'b', 'c', 'd'}
	cut := []byte{20, 0xff, 1, 2, 3, 4}
	if got := WholeAD(append(append([]byte(nil), name...), cut...)); !bytes.Equal(got, name) {
		t.Errorf("got % x, want the name alone", got)
	}
	if got := WholeAD(name); !bytes.Equal(got, name) {
		t.Errorf("complete data cut to % x", got)
	}
}

func TestAppendAD(t *testing.T) {
	b := AppendAD(nil, 0x01, []byte{0x06})
	b = AppendAD(b, 0x09, []byte("abc"))
	if want := []byte{2, 0x01, 0x06, 4, 0x09, 'a', 'b', 'c'}; !bytes.Equal(b, want) {
		t.Fatalf("got % x, want % x", b, want)
	}
	// A structure that does not fit is dropped whole; a later one that fits is kept.
	b = AppendAD(b, 0xff, make([]byte, MaxAdvert))
	b = AppendAD(b, 0x0a, []byte{0xf4})
	if want := []byte{2, 0x01, 0x06, 4, 0x09, 'a', 'b', 'c', 2, 0x0a, 0xf4}; !bytes.Equal(b, want) {
		t.Fatalf("got % x, want % x", b, want)
	}
	full := AppendAD(nil, 0xff, make([]byte, MaxAdvert-2))
	if len(full) != MaxAdvert {
		t.Fatalf("len %d, want %d", len(full), MaxAdvert)
	}
	if got := AppendAD(full, 0x01, nil); len(got) != MaxAdvert {
		t.Fatalf("overflowed to %d", len(got))
	}
}

// An extended advertisement can carry up to 1650 bytes; the rebuilt data keeps the structures that fit in an
// ESPHome entry, in order, and stays parseable.
func TestAppendADExtended(t *testing.T) {
	var b []byte
	b = AppendAD(b, 0x01, []byte{0x06})
	b = AppendAD(b, 0xff, bytes.Repeat([]byte{0xaa}, 200)) // long manufacturer data: dropped
	b = AppendAD(b, 0x16, bytes.Repeat([]byte{0xbb}, 40))  // service data: fits
	b = AppendAD(b, 0x09, bytes.Repeat([]byte{'n'}, 30))   // name: does not fit any more
	b = AppendAD(b, 0x0a, []byte{0x04})                    // TX power: fits
	if len(b) > MaxAdvert {
		t.Fatalf("len %d", len(b))
	}
	var types []byte
	for typ := range ADs(b) {
		types = append(types, typ)
	}
	if want := []byte{0x01, 0x16, 0x0a}; !bytes.Equal(types, want) {
		t.Fatalf("types % x, want % x", types, want)
	}
	if len(WholeAD(b)) != len(b) {
		t.Fatalf("not parseable to the end: % x", b)
	}
}

func TestAppendADs(t *testing.T) {
	big := append([]byte{57, 0xff}, make([]byte, 56)...)
	for _, c := range []struct {
		dst, src, want []byte
	}{
		{nil, []byte{2, 1, 6, 0, 0}, []byte{2, 1, 6, 0, 0}}, // fits: copied as is, padding included
		{big, big, big}, // does not fit: dropped
		{big, append([]byte{2, 1, 6, 0}, big...), append(append([]byte(nil), big...), 2, 1, 6)}, // zero length stops
		{big, append([]byte{2, 1, 6}, 30, 0xff), append(append([]byte(nil), big...), 2, 1, 6)},  // cut off: stops
	} {
		got := AppendADs(append([]byte(nil), c.dst...), c.src)
		if !bytes.Equal(got, c.want) {
			t.Errorf("AppendADs(%x, %x)\n got %x\nwant %x", c.dst, c.src, got, c.want)
		}
	}
}

func TestAppendADsAllocates(t *testing.T) {
	var buf [MaxAdvert]byte
	src := append([]byte{2, 1, 6}, bytes.Repeat([]byte{0xaa}, 100)...)
	src = append(src, 2, 0x0a, 4)
	if n := testing.AllocsPerRun(100, func() { AppendADs(buf[:0], src) }); n != 0 {
		t.Errorf("%v allocations per call", n)
	}
}
