package api

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

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

// TestConnParamsMessages decodes a request with a value beyond 16 bits and encodes a negative error code,
// which int32 carries as a 10-byte varint.
func TestConnParamsMessages(t *testing.T) {
	var e enc
	e.uint(1, 0xAABBCCDDEEFF)
	e.uint(2, 6)
	e.uint(3, 0x12345)
	e.uint(4, 499)
	e.uint(5, 3200)
	addr, p, err := decConnParams(e.b)
	if want := (ble.ConnParams{MinInterval: 6, MaxInterval: 0xffff, Latency: 499, Timeout: 3200}); err != nil ||
		addr != 0xAABBCCDDEEFF || p != want {
		t.Errorf("decoded %x %+v %v, want %+v", addr, p, err, want)
	}
	fs, err := fields(encConnParamsResult(0xAABBCCDDEEFF, gattNotConnected))
	if err != nil || fieldValue(fs, 1) != 0xAABBCCDDEEFF || int32(fieldValue(fs, 2)) != -1 {
		t.Errorf("encoded %+v %v", fs, err)
	}
	if fs, _ := fields(encConnParamsResult(1, 0)); len(fs) != 1 {
		t.Errorf("success carries an error field: %+v", fs)
	}
}

// TestAppendRawAdvert checks the one-pass encoding against sub-messages built one by one, for zero fields,
// negative and positive RSSI and the largest data.
func TestAppendRawAdvert(t *testing.T) {
	ads := []ble.Advert{
		{Addr: 0xF09E4A123456, AddrType: ble.AddrRandom, RSSI: -67, Data: []byte{2, 1, 6}},
		{},
		{Addr: 1, RSSI: 20, Data: bytes.Repeat([]byte{0xff}, 62)},
		{Addr: 0xFFFFFFFFFFFF, RSSI: -128, AddrType: ble.AddrRandom},
	}
	var got []byte
	var want enc
	for _, a := range ads {
		got = appendRawAdvert(got, a)
		var s enc
		s.uint(1, a.Addr)
		s.sint(2, int64(a.RSSI))
		s.uint(3, uint64(a.AddrType))
		s.bytes(4, a.Data)
		want.msg(1, s.b)
		if len(s.b)+2 > advEntrySize {
			t.Errorf("entry of %d bytes exceeds advEntrySize", len(s.b)+2)
		}
	}
	if !bytes.Equal(got, want.b) {
		t.Fatalf("got  %x\nwant %x", got, want.b)
	}
}

// TestAdvertDataCopied reuses an advertisement's data buffer after the callback, as backends may: the batch
// must keep what the callback saw.
func TestAdvertDataCopied(t *testing.T) {
	s := NewServer(Config{Name: "test"}, testAdapter{}, nil)
	data := []byte{2, 1, 6}
	s.onAdvert(ble.Advert{Addr: 1, Data: data})
	copy(data, []byte{9, 9, 9})
	s.mu.Lock() // the batch's timer runs
	defer s.mu.Unlock()
	if want := appendRawAdvert(nil, ble.Advert{Addr: 1, Data: []byte{2, 1, 6}}); !bytes.Equal(s.pending, want) {
		t.Fatalf("batch %x, want %x", s.pending, want)
	}
}

// TestAdvertBatching checks ESPHome's batching: a full batch goes at once, a partial one advFlushInterval
// after its first advertisement.
func TestAdvertBatching(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	s := NewServer(Config{Name: "test"}, testAdapter{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := newClient(s, a, s.log)
	defer c.closeQueue()
	c.advSubscribed = true
	s.clients[c] = true
	r := bufio.NewReader(b)
	next := func() (int, time.Time) {
		t.Helper()
		b.SetReadDeadline(time.Now().Add(2 * time.Second))
		f, err := ReadFrame(r)
		if err != nil {
			t.Fatal(err)
		}
		fs, err := fields(f.Payload)
		if err != nil {
			t.Fatal(err)
		}
		return len(fs), time.Now()
	}
	start := time.Now()
	for i := range advBatchSize + 1 {
		s.onAdvert(ble.Advert{Addr: uint64(i + 1)})
	}
	if n, _ := next(); n != advBatchSize {
		t.Fatalf("first batch of %d", n)
	}
	n, at := next()
	if n != 1 || at.Sub(start) < advFlushInterval {
		t.Fatalf("second batch of %d after %v", n, at.Sub(start))
	}
}
