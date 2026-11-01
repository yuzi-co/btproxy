package ble

import "testing"

var (
	// iBeacon from an Apple device: Flags, then manufacturer data with company id 0x004c.
	adIBeacon = []byte{0x02, 0x01, 0x06, 0x1a, 0xff, 0x4c, 0x00, 0x02, 0x15, 0xe2, 0xc5, 0x6d, 0xb5, 0xdf, 0xfb,
		0x48, 0xd2, 0xb0, 0x60, 0xd0, 0xf5, 0xa7, 0x10, 0x96, 0xe0, 0x00, 0x01, 0x00, 0x02, 0xc5}
	// Xiaomi MiBeacon: service data with 16-bit UUID 0xfe95.
	adMiBeacon = []byte{0x02, 0x01, 0x06, 0x0a, 0x16, 0x95, 0xfe, 0x30, 0x58, 0x5b, 0x05, 0x01, 0x02, 0x03}
	// A complete list of 16-bit UUIDs (0x180f, 0x181a) and one of 128-bit UUIDs (6e400001-b5a3-f393-e0a9-e50e24dcca9e).
	adLists = []byte{0x05, 0x03, 0x0f, 0x18, 0x1a, 0x18,
		0x11, 0x07, 0x9e, 0xca, 0xdc, 0x24, 0x0e, 0xe5, 0xa9, 0xe0, 0x93, 0xf3, 0xa3, 0xb5, 0x01, 0x00, 0x40, 0x6e}
	// A 128-bit UUID on the Bluetooth base UUID (0000fe95-0000-1000-8000-00805f9b34fb) in a 128-bit list.
	adLongBase = []byte{0x11, 0x06, 0xfb, 0x34, 0x9b, 0x5f, 0x80, 0x00, 0x00, 0x80, 0x00, 0x10, 0x00, 0x00, 0x95, 0xfe,
		0x00, 0x00}
	// Service data with a 32-bit UUID 0x12345678.
	adSD32 = []byte{0x07, 0x20, 0x78, 0x56, 0x34, 0x12, 0xaa, 0xbb}
	// Malformed: the second structure runs past the end.
	adBroken = []byte{0x02, 0x01, 0x06, 0x09, 0xff, 0x4c}
)

func TestAdvFilter(t *testing.T) {
	const (
		pub    = 0x582BDB010203
		static = 0xC12345678901 // random static: top bits 11
		rpa    = 0x512345678901 // resolvable private: 01
		nrpa   = 0x112345678901 // non-resolvable private: 00
	)
	ad := func(addr uint64, typ uint32, data []byte) Advert {
		return Advert{Addr: addr, AddrType: typ, RSSI: -60, Data: data}
	}
	for _, c := range []struct {
		name, allow, deny string
		denyRandom        bool
		ad                Advert
		want              bool
	}{
		{name: "allow mac", allow: "58:2B:DB:01:02:03", ad: ad(pub, AddrPublic, nil), want: true},
		{name: "allow mac lower case", allow: "58:2b:db:01:02:03", ad: ad(pub, AddrPublic, nil), want: true},
		{name: "allow other mac", allow: "58:2B:DB:01:02:04", ad: ad(pub, AddrPublic, nil)},
		{name: "allow oui", allow: "58:2B:DB", ad: ad(pub, AddrPublic, nil), want: true},
		{name: "allow oui star", allow: "58:2B:DB:*", ad: ad(pub, AddrPublic, nil), want: true},
		{name: "allow 4-byte prefix", allow: "58:2B:DB:01:*", ad: ad(pub, AddrPublic, nil), want: true},
		{name: "allow other oui", allow: "58:2B:DC", ad: ad(pub, AddrPublic, nil)},
		{name: "allow mfr", allow: "mfr:0x004c", ad: ad(rpa, AddrRandom, adIBeacon), want: true},
		{name: "allow mfr decimal", allow: "mfr:76", ad: ad(rpa, AddrRandom, adIBeacon), want: true},
		{name: "allow other mfr", allow: "mfr:0x0006", ad: ad(rpa, AddrRandom, adIBeacon)},
		{name: "allow service data", allow: "uuid:fe95", ad: ad(pub, AddrPublic, adMiBeacon), want: true},
		{name: "allow service data long form", allow: "uuid:0000fe95-0000-1000-8000-00805f9b34fb",
			ad: ad(pub, AddrPublic, adMiBeacon), want: true},
		{name: "allow 16-bit list", allow: "uuid:181a", ad: ad(pub, AddrPublic, adLists), want: true},
		{name: "allow 128-bit list", allow: "uuid:6E400001-B5A3-F393-E0A9-E50E24DCCA9E",
			ad: ad(pub, AddrPublic, adLists), want: true},
		{name: "allow 16-bit in 128-bit list", allow: "uuid:fe95", ad: ad(pub, AddrPublic, adLongBase), want: true},
		{name: "allow 32-bit service data", allow: "uuid:12345678", ad: ad(pub, AddrPublic, adSD32), want: true},
		{name: "allow uuid miss", allow: "uuid:180d", ad: ad(pub, AddrPublic, adLists)},
		{name: "malformed data", allow: "mfr:0x004c", ad: ad(pub, AddrPublic, adBroken)},
		{name: "allow several", allow: "uuid:180d, mfr:0x004c", ad: ad(rpa, AddrRandom, adIBeacon), want: true},
		{name: "deny mac", deny: "58:2B:DB:01:02:03", ad: ad(pub, AddrPublic, adMiBeacon)},
		{name: "deny other", deny: "mfr:0x004c", ad: ad(pub, AddrPublic, adMiBeacon), want: true},
		{name: "deny wins", allow: "uuid:fe95", deny: "58:2B:DB", ad: ad(pub, AddrPublic, adMiBeacon)},
		{name: "deny random rpa", denyRandom: true, ad: ad(rpa, AddrRandom, adIBeacon)},
		{name: "deny random nrpa", denyRandom: true, ad: ad(nrpa, AddrRandom, adIBeacon)},
		{name: "deny random keeps static", denyRandom: true, ad: ad(static, AddrRandom, adIBeacon), want: true},
		{name: "deny random keeps public", denyRandom: true, ad: ad(rpa, AddrPublic, adIBeacon), want: true},
		{name: "deny random over allow", allow: "mfr:0x004c", denyRandom: true, ad: ad(rpa, AddrRandom, adIBeacon)},
	} {
		f, err := ParseAdvFilter(c.allow, c.deny, c.denyRandom)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := f.Allow(c.ad); got != c.want {
			t.Errorf("%s: Allow = %v, want %v (filter %v)", c.name, got, c.want, f)
		}
	}
}

func TestParseAdvFilter(t *testing.T) {
	f, err := ParseAdvFilter(" , ", "", false)
	if f != nil || err != nil {
		t.Errorf("empty: %v, %v", f, err)
	}
	if !f.Allow(Advert{}) {
		t.Error("nil filter drops")
	}
	f, err = ParseAdvFilter("58:2b:db, aa:bb:cc:dd:ee:ff, 01:*, uuid:fe95, uuid:6e400001-b5a3-f393-e0a9-e50e24dcca9e",
		"mfr:0x004c", true)
	if err != nil {
		t.Fatal(err)
	}
	const want = "allow 58:2B:DB:*,AA:BB:CC:DD:EE:FF,01:*,uuid:fe95,uuid:6e400001-b5a3-f393-e0a9-e50e24dcca9e; " +
		"deny mfr:0x004c and private addresses"
	if got := f.String(); got != want {
		t.Errorf("String() = %q\nwant %q", got, want)
	}
	for _, bad := range []string{"58:2B", "58:2B:DB:01", "58:2B:DB:01:02:03:04", "5:2B:DB", "zz:2B:DB", "*",
		"uuid:fe9", "uuid:xyz1", "uuid:6e40000", "mfr:0x10000", "mfr:", "foo"} {
		if _, err := ParseAdvFilter(bad, "", false); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// BenchmarkAdvFilter measures the filter on an advertisement it walks to the end: an allow list of UUIDs and
// company ids none of which the advertisement carries, and a deny list of addresses.
func BenchmarkAdvFilter(b *testing.B) {
	f, err := ParseAdvFilter("uuid:fe95,uuid:181a,uuid:fcd2,mfr:0x0499,mfr:0x0006",
		"58:2B:DB,AA:BB:CC:DD:EE:FF", true)
	if err != nil {
		b.Fatal(err)
	}
	ad := Advert{Addr: 0xC12345678901, AddrType: AddrRandom, RSSI: -67, Data: adIBeacon}
	b.ReportAllocs()
	for b.Loop() {
		if f.Allow(ad) {
			b.Fatal("allowed")
		}
	}
}
