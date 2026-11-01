package hci

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// HCI events as the monitor delivers them (btmon -w captures hold the same bytes after the btsnoop header),
// laid out after the Core specification and btmon's decoding of them.
const (
	// LE Advertising Report, ADV_IND, random address F0:9E:4A:12:34:56, RSSI -67: Flags 0x06 and an
	// iBeacon (Apple manufacturer data).
	evAdvInd = "3e 2a 02 01 00 01 563412 4a9ef0 1e" +
		"020106 1aff4c000215 e2c56db5dffb48d2b060d0f5a71096e0 0001 0002 c5" + "bd"
	// LE Advertising Report, SCAN_RSP from the same device, RSSI -66: name "Test" and TX power 4.
	evScanRsp = "3e 15 02 01 04 01 563412 4a9ef0 09 0509 54657374 020a04 be"
	// LE Extended Advertising Report, legacy ADV_NONCONN_IND (0x0010), public address 00:11:22:33:44:55,
	// 1M PHY, no SID, TX power not available, RSSI -60: Flags and the Eddystone service UUID.
	evExtNonconn = "3e 21 0d 01 1000 00 554433221100 01 00 ff 7f c4 0000 00 000000000000 07 020106 0303aafe"
	// LE Extended Advertising Report, legacy SCAN_RSP to an ADV_IND (0x001b), from the same device.
	evExtScanRsp = "3e 1f 0d 01 1b00 00 554433221100 01 00 ff 7f c5 0000 00 000000000000 05 0409 616263"
	// LE Extended Advertising Report, legacy ADV_IND (0x0013) from the same device.
	evExtAdvInd = "3e 1d 0d 01 1300 00 554433221100 01 00 ff 7f c3 0000 00 000000000000 03 020106"
)

func collect(t *testing.T, events ...string) []report {
	var out []report
	for _, e := range events {
		ev := unhex(t, e)
		parseEvent(ev, func(r report) {
			r.data = append([]byte(nil), r.data...)
			out = append(out, r)
		})
	}
	return out
}

func TestParseLegacy(t *testing.T) {
	rs := collect(t, evAdvInd, evScanRsp)
	if len(rs) != 2 {
		t.Fatalf("%d reports", len(rs))
	}
	adv, rsp := rs[0], rs[1]
	if adv.addr != 0xF09E4A123456 || adv.addrType != ble.AddrRandom || adv.rssi != -67 || !adv.scannable ||
		adv.scanRsp || len(adv.data) != 30 || adv.data[3] != 0x1a {
		t.Errorf("ADV_IND: %+v", adv)
	}
	if rsp.addr != adv.addr || rsp.rssi != -66 || !rsp.scanRsp ||
		!bytes.Equal(rsp.data, unhex(t, "050954657374020a04")) {
		t.Errorf("SCAN_RSP: %+v", rsp)
	}
}

func TestParseExtended(t *testing.T) {
	rs := collect(t, evExtNonconn, evExtScanRsp, evExtAdvInd)
	if len(rs) != 3 {
		t.Fatalf("%d reports", len(rs))
	}
	if r := rs[0]; r.addr != 0x001122334455 || r.addrType != ble.AddrPublic || r.rssi != -60 || r.scannable ||
		r.scanRsp || !bytes.Equal(r.data, unhex(t, "0201060303aafe")) {
		t.Errorf("ADV_NONCONN_IND: %+v", r)
	}
	if r := rs[1]; !r.scanRsp || r.rssi != -59 || string(r.data[2:]) != "abc" {
		t.Errorf("SCAN_RSP: %+v", r)
	}
	if r := rs[2]; !r.scannable || r.scanRsp || r.rssi != -61 {
		t.Errorf("ADV_IND: %+v", r)
	}
}

// extReport builds one extended advertising report.
func extReport(typ uint16, addrType byte, addr string, rssi byte, data []byte) []byte {
	b := []byte{byte(typ), byte(typ >> 8), addrType}
	a, _ := hex.DecodeString(addr)
	for i := len(a) - 1; i >= 0; i-- {
		b = append(b, a[i])
	}
	b = append(b, 1, 3, 1, 0x7f, rssi, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(len(data)))
	return append(b, data...)
}

// event wraps reports into an LE Meta event.
func event(sub byte, reports ...[]byte) []byte {
	p := []byte{sub, byte(len(reports))}
	for _, r := range reports {
		p = append(p, r...)
	}
	return append([]byte{evtLEMeta, byte(len(p))}, p...)
}

func TestParseSkips(t *testing.T) {
	var n int
	count := func(report) { n++ }
	// Two reports in one event, one without an RSSI (127), and an anonymous one.
	parseEvent(event(subLEExtAdvReport,
		extReport(0x10, 1, "c00000000001", 0xc0, nil),
		extReport(0x10, 1, "c00000000002", 0x7f, nil),
		extReport(0x00, 0xff, "000000000000", 0xc0, nil)), count)
	if n != 1 {
		t.Errorf("%d reports, want 1", n)
	}
	// Truncated and mismatched events, and other LE Meta subevents.
	n = 0
	for _, e := range []string{evAdvInd[:len(evAdvInd)-2], "3e 2b" + evAdvInd[5:], "3e 03 01 00 00", "0e 04 01 0c 20 00"} {
		parseEvent(unhex(t, e), count)
	}
	if n != 0 {
		t.Errorf("%d reports from malformed events", n)
	}
	// A report whose data runs past the event ends the parsing after the good one before it.
	good := extReport(0x10, 0, "000000000001", 0xc0, []byte{2, 1, 6})
	bad := extReport(0x10, 0, "000000000002", 0xc0, []byte{2, 1, 6})
	parseEvent(event(subLEExtAdvReport, good, bad[:len(bad)-2]), count)
	if n != 1 {
		t.Errorf("%d reports, want 1", n)
	}
}

// keep returns an emit function that appends copies of the advertisements to got, since their data is only
// valid during the call.
func keep(got *[]ble.Advert) func(ble.Advert) {
	return func(a ble.Advert) {
		a.Data = bytes.Clone(a.Data)
		*got = append(*got, a)
	}
}

func TestDecoderPassive(t *testing.T) {
	var got []ble.Advert
	d := decoder{emit: keep(&got)}
	now := time.Unix(1000, 0)
	d.event(unhex(t, evAdvInd), now)
	d.event(unhex(t, evScanRsp), now)
	if len(got) != 2 || len(got[0].Data) != 30 || got[0].RSSI != -67 || len(got[1].Data) != 9 {
		t.Fatalf("passive: %+v", got)
	}
}

func TestDecoderActive(t *testing.T) {
	var got []ble.Advert
	d := decoder{emit: keep(&got), active: true}
	now := time.Unix(1000, 0)

	// Advertisement and scan response are reported together, as an ESP32 does.
	d.event(unhex(t, evAdvInd), now)
	if len(got) != 0 {
		t.Fatalf("scannable advertisement not held: %+v", got)
	}
	d.event(unhex(t, evScanRsp), now.Add(time.Millisecond))
	want := append(unhex(t, evAdvInd)[13:43], unhex(t, evScanRsp)[13:22]...)
	if len(got) != 1 || got[0].RSSI != -66 || got[0].Addr != 0xF09E4A123456 || !bytes.Equal(got[0].Data, want) {
		t.Fatalf("combined: %+v\nwant data %x", got, want)
	}

	// Non-scannable advertisements pass at once; a missing scan response releases the advertisement alone.
	got = nil
	d.event(unhex(t, evExtAdvInd), now)
	d.event(unhex(t, evExtNonconn), now)
	if len(got) != 1 || got[0].RSSI != -60 {
		t.Fatalf("non-scannable: %+v", got)
	}
	d.expire(now.Add(holdFor / 2))
	if len(got) != 1 {
		t.Fatal("released before holdFor")
	}
	d.expire(now.Add(holdFor))
	if len(got) != 2 || got[1].RSSI != -61 || !bytes.Equal(got[1].Data, []byte{2, 1, 6}) {
		t.Fatalf("expired: %+v", got)
	}

	// A newer advertisement from the same device releases the older one; a scan response without a held
	// advertisement is reported alone.
	got = nil
	d.event(unhex(t, evExtAdvInd), now)
	d.event(unhex(t, evExtAdvInd), now.Add(time.Millisecond))
	d.event(unhex(t, evScanRsp), now.Add(2*time.Millisecond))
	if len(got) != 2 || len(got[0].Data) != 3 || len(got[1].Data) != 9 {
		t.Fatalf("replaced: %+v", got)
	}

	// With every slot taken, the oldest is released.
	got = nil
	for i := 0; i <= holdSlots; i++ {
		addr := []byte{0, 0, 0, 0, 0, byte(i)}
		d.event(event(subLEExtAdvReport, extReport(0x13, 0, hex.EncodeToString(addr), 0xc0, nil)),
			now.Add(time.Duration(10+i)*time.Millisecond))
	}
	if len(got) != 2 || got[0].Addr != 0x001122334455 || got[1].Addr != 0 {
		t.Fatalf("eviction: %+v", got)
	}

	// Switching to passive releases everything held.
	got = nil
	d.active = false
	d.expire(now)
	if len(got) != holdSlots {
		t.Fatalf("%d released after switching to passive, want %d", len(got), holdSlots)
	}
}

func TestDecoderFragments(t *testing.T) {
	var got []ble.Advert
	d := decoder{emit: keep(&got)}
	now := time.Unix(1000, 0)
	mfr := append([]byte{48, 0xff}, make([]byte, 47)...)
	svc := append([]byte{48, 0x16}, make([]byte, 47)...)
	name := []byte{5, 0x09, 'A', 'b', 'c', 'd'}
	first := append(append(append([]byte(nil), mfr...), svc...), name...)
	const more, complete = 1 << 5, 0
	d.event(event(subLEExtAdvReport, extReport(more, 1, "c00000000001", 0xc0, first)), now)
	d.event(event(subLEExtAdvReport, extReport(more, 1, "c00000000001", 0xc0, make([]byte, 30))), now)
	d.event(event(subLEExtAdvReport, extReport(complete, 1, "c00000000001", 0xc0, make([]byte, 30))), now)
	d.event(event(subLEExtAdvReport, extReport(complete, 1, "c00000000001", 0xc0, name)), now)
	if len(got) != 2 {
		t.Fatalf("%d advertisements, want 2: %+v", len(got), got)
	}
	// The first fragment's structures that fit into 62 bytes: the manufacturer data and the name.
	if want := append(append([]byte(nil), mfr...), name...); !bytes.Equal(got[0].Data, want) {
		t.Errorf("first fragment\n got %x\nwant %x", got[0].Data, want)
	}
	if !bytes.Equal(got[1].Data, name) {
		t.Errorf("next advertisement %x", got[1].Data)
	}
}

func TestAppendAD(t *testing.T) {
	big := append([]byte{57, 0xff}, make([]byte, 56)...)
	for _, c := range []struct {
		dst, src, want []byte
	}{
		{nil, []byte{2, 1, 6, 0, 0}, []byte{2, 1, 6, 0, 0}}, // fits: copied as is, padding included
		{big, big, big}, // does not fit: dropped
		{big, append([]byte{2, 1, 6, 0}, big...), append(append([]byte(nil), big...), 2, 1, 6)}, // zero length stops
		{big, append([]byte{2, 1, 6}, 30, 0xff), append(append([]byte(nil), big...), 2, 1, 6)},  // cut off: stops
	} {
		got := appendAD(append([]byte(nil), c.dst...), c.src)
		if !bytes.Equal(got, c.want) {
			t.Errorf("appendAD(%x, %x)\n got %x\nwant %x", c.dst, c.src, got, c.want)
		}
	}
}

// runFilter interprets the instructions filterProg uses.
func runFilter(prog []insn, pkt []byte) uint32 {
	var acc uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.code {
		case bpfLdAbsB:
			if int(in.k) >= len(pkt) {
				return 0
			}
			acc = uint32(pkt[in.k])
		case bpfJeqK:
			if acc == in.k {
				pc += int(in.jt)
			} else {
				pc += int(in.jf)
			}
		case bpfRetK:
			return in.k
		default:
			panic("unexpected instruction")
		}
	}
	panic("ran off the end")
}

func TestFilter(t *testing.T) {
	pkt := func(op, idx uint16, ev string) []byte {
		e := unhex(t, ev)
		return append([]byte{byte(op), byte(op >> 8), byte(idx), byte(idx >> 8), byte(len(e)), 0}, e...)
	}
	for _, c := range []struct {
		name  string
		index uint16
		pkt   []byte
		pass  bool
	}{
		{"advertising report", 0, pkt(monEventPk, 0, evAdvInd), true},
		{"extended report", 0, pkt(monEventPk, 0, evExtNonconn), true},
		{"index 258", 258, pkt(monEventPk, 258, evExtNonconn), true},
		{"other controller", 0, pkt(monEventPk, 1, evAdvInd), false},
		{"other controller, high byte", 2, pkt(monEventPk, 258, evAdvInd), false},
		{"ACL data", 0, pkt(5, 0, evAdvInd), false},
		{"opcode high byte", 0, pkt(0x103, 0, evAdvInd), false},
		{"command complete", 0, pkt(monEventPk, 0, "0e 04 01 0c 20 00"), false},
		{"connection complete", 0, pkt(monEventPk, 0, "3e 13 01 00 4000 00 00 554433221100 2800 0000 f401 00"), false},
		{"short", 0, pkt(monEventPk, 0, "3e"), false},
	} {
		if got := runFilter(filterProg(c.index), c.pkt) != 0; got != c.pass {
			t.Errorf("%s: passed %v, want %v", c.name, got, c.pass)
		}
	}
}
