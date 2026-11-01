package mdns

import (
	"encoding/binary"
	"net"
	"slices"
	"strings"
	"testing"
)

var testSvc = newService("btproxy-bedroom", 6053, []string{"mac=0123456789ab", "version=2026.10.0"})

var testIPs = []net.IP{net.IPv4(10, 0, 10, 5).To4()}

// buildQuery encodes questions, compressing names that end in a previous question's name, like real browsers.
func buildQuery(id uint16, qs ...question) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b, id)
	binary.BigEndian.PutUint16(b[4:], uint16(len(qs)))
	offs := map[string]int{}
	for _, q := range qs {
		name := q.name
		for name != "" && name != "." {
			if o, ok := offs[name]; ok {
				b = binary.BigEndian.AppendUint16(b, 0xc000|uint16(o))
				break
			}
			offs[name] = len(b)
			l, rest, _ := cut(name)
			b = append(append(b, byte(len(l))), l...)
			name = rest
		}
		if name == "" || name == "." {
			b = append(b, 0)
		}
		class := q.qclass
		if q.unicast {
			class |= topBit
		}
		b = binary.BigEndian.AppendUint16(b, q.qtype)
		b = binary.BigEndian.AppendUint16(b, class)
	}
	return b
}

func cut(name string) (string, string, bool) {
	for i := range name {
		if name[i] == '.' {
			return name[:i], name[i+1:], true
		}
	}
	return name, "", false
}

type rr struct {
	name  string
	rtype uint16
	class uint16
	ttl   uint32
	data  []byte
}

func parseResponse(t *testing.T, b []byte) (id uint16, qd int, an, ar []rr) {
	t.Helper()
	if binary.BigEndian.Uint16(b[2:]) != 0x8400 {
		t.Fatalf("flags %#x", binary.BigEndian.Uint16(b[2:]))
	}
	id, qd = binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[4:]))
	nan, nar := int(binary.BigEndian.Uint16(b[6:])), int(binary.BigEndian.Uint16(b[10:]))
	off := 12
	for range qd {
		_, next, err := readName(b, off)
		if err != nil {
			t.Fatal(err)
		}
		off = next + 4
	}
	var all []rr
	for range nan + nar {
		name, next, err := readName(b, off)
		if err != nil {
			t.Fatal(err)
		}
		r := rr{name: name, rtype: binary.BigEndian.Uint16(b[next:]), class: binary.BigEndian.Uint16(b[next+2:]),
			ttl: binary.BigEndian.Uint32(b[next+4:])}
		l := int(binary.BigEndian.Uint16(b[next+8:]))
		r.data = b[next+10 : next+10+l]
		off = next + 10 + l
		all = append(all, r)
	}
	if off != len(b) {
		t.Fatalf("%d trailing bytes", len(b)-off)
	}
	return id, qd, all[:nan], all[nan:]
}

func types(rs []rr) []uint16 {
	var ts []uint16
	for _, r := range rs {
		ts = append(ts, r.rtype)
	}
	return ts
}

func TestBrowse(t *testing.T) {
	// What python-zeroconf sends when Home Assistant browses: PTR for the service type, multicast response.
	q, err := parseQuery(buildQuery(0, question{name: "_esphomelib._tcp.local.", qtype: typePTR, qclass: classIN}))
	if err != nil {
		t.Fatal(err)
	}
	an, ex, unicast := testSvc.answer(q, testIPs)
	if unicast {
		t.Error("unicast")
	}
	_, qd, a, x := parseResponse(t, encodeResponse(0, nil, an, ex, false, false))
	if qd != 0 || !slices.Equal(types(a), []uint16{typePTR}) || !slices.Equal(types(x), []uint16{typeSRV, typeTXT, typeA}) {
		t.Fatalf("qd %d answers %v extra %v", qd, types(a), types(x))
	}
	if ptr, _, _ := readName(a[0].data, 0); ptr != "btproxy-bedroom._esphomelib._tcp.local." || a[0].class != classIN ||
		a[0].ttl != ttlOther {
		t.Errorf("PTR %q class %#x ttl %d", ptr, a[0].class, a[0].ttl)
	}
	srv := x[0]
	host, _, _ := readName(srv.data, 6)
	if srv.name != "btproxy-bedroom._esphomelib._tcp.local." || binary.BigEndian.Uint16(srv.data[4:]) != 6053 ||
		host != "btproxy-bedroom.local." || srv.class != classIN|topBit || srv.ttl != ttlHost {
		t.Errorf("SRV %+v host %q", srv, host)
	}
	if string(x[1].data) != "\x10mac=0123456789ab\x11version=2026.10.0" {
		t.Errorf("TXT %q", x[1].data)
	}
	if x[2].name != "btproxy-bedroom.local." || !net.IP(x[2].data).Equal(testIPs[0]) {
		t.Errorf("A %+v", x[2])
	}
}

func TestQuestions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		qs      []question
		answers []uint16
		extra   []uint16
		unicast bool
	}{
		{"services", []question{{name: servicesName, qtype: typePTR, qclass: classIN}}, []uint16{typePTR}, nil, false},
		{"srv txt QU", []question{
			{name: "BTProxy-Bedroom._esphomelib._tcp.local.", qtype: typeSRV, qclass: classIN, unicast: true},
			{name: "btproxy-bedroom._esphomelib._tcp.local.", qtype: typeTXT, qclass: classIN, unicast: true},
		}, []uint16{typeSRV, typeTXT}, []uint16{typeA}, true},
		{"mixed QU and QM", []question{
			{name: "btproxy-bedroom._esphomelib._tcp.local.", qtype: typeSRV, qclass: classIN, unicast: true},
			{name: "btproxy-bedroom.local.", qtype: typeA, qclass: classIN},
		}, []uint16{typeSRV, typeA}, nil, false},
		{"A", []question{{name: "btproxy-bedroom.local.", qtype: typeA, qclass: classIN}}, []uint16{typeA}, nil, false},
		{"ANY instance", []question{{name: "btproxy-bedroom._esphomelib._tcp.local.", qtype: typeANY, qclass: classIN}},
			[]uint16{typeSRV, typeTXT}, []uint16{typeA}, false},
		{"AAAA", []question{{name: "btproxy-bedroom.local.", qtype: 28, qclass: classIN}}, nil, nil, false},
		{"other host", []question{{name: "other.local.", qtype: typeA, qclass: classIN}}, nil, nil, false},
		{"other service", []question{{name: "_http._tcp.local.", qtype: typePTR, qclass: classIN}}, nil, nil, false},
		{"CHAOS", []question{{name: "btproxy-bedroom.local.", qtype: typeA, qclass: 3}}, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := parseQuery(buildQuery(0, tc.qs...))
			if err != nil {
				t.Fatal(err)
			}
			an, ex, unicast := testSvc.answer(q, testIPs)
			var a, x []uint16
			for _, r := range an {
				a = append(a, r.rtype)
			}
			for _, r := range ex {
				x = append(x, r.rtype)
			}
			if !slices.Equal(a, tc.answers) || !slices.Equal(x, tc.extra) || unicast != tc.unicast {
				t.Errorf("answers %v extra %v unicast %v", a, x, unicast)
			}
		})
	}
}

func TestLegacy(t *testing.T) {
	qs := []question{{name: "btproxy-bedroom.local.", qtype: typeA, qclass: classIN}}
	q, err := parseQuery(buildQuery(0x1234, qs...))
	if err != nil {
		t.Fatal(err)
	}
	an, ex, _ := testSvc.answer(q, testIPs)
	id, qd, a, _ := parseResponse(t, encodeResponse(q.id, q.questions, an, ex, true, false))
	if id != 0x1234 || qd != 1 || len(a) != 1 || a[0].class != classIN || a[0].ttl != ttlLegacy {
		t.Errorf("id %#x qd %d answers %+v", id, qd, a)
	}
}

func TestGoodbye(t *testing.T) {
	_, _, a, _ := parseResponse(t, encodeResponse(0, nil, testSvc.all(testIPs), nil, false, true))
	if !slices.Equal(types(a), []uint16{typePTR, typeSRV, typeTXT, typePTR, typeA}) {
		t.Fatalf("types %v", types(a))
	}
	for _, r := range a {
		if r.ttl != 0 {
			t.Errorf("%s ttl %d", r.name, r.ttl)
		}
	}
}

func TestParseRejects(t *testing.T) {
	resp := encodeResponse(0, nil, testSvc.all(testIPs), nil, false, false)
	loop := buildQuery(0, question{name: "a.local.", qtype: typeA, qclass: classIN})
	loop = append(loop[:12], 0xc0, 12, 0, 1, 0, 1) // name pointing at itself
	label := strings.Repeat("a", 63) + "."
	q := question{name: "a.local.", qtype: typeA, qclass: classIN}
	for name, b := range map[string][]byte{
		"long name":          buildQuery(0, question{name: strings.Repeat(label, 4) + "aaaaaaaaaaaa.", qtype: typeA, qclass: classIN}),
		"too many questions": buildQuery(0, slices.Repeat([]question{q}, maxQuestions+1)...),
		"response":           resp,
		"short":              {0, 0, 0},
		"truncated":          buildQuery(0, question{name: "a.local.", qtype: typeA, qclass: classIN})[:16],
		"loop":               loop,
	} {
		if _, err := parseQuery(b); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseLimitsAllow(t *testing.T) {
	q := question{name: "a.local.", qtype: typeA, qclass: classIN}
	if _, err := parseQuery(buildQuery(0, slices.Repeat([]question{q}, maxQuestions)...)); err != nil {
		t.Errorf("%d questions: %v", maxQuestions, err)
	}
	// The longest name: 254 characters with its trailing dot, 255 bytes on the wire with the root label.
	long := strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("a", 61) + "."
	if _, err := parseQuery(buildQuery(0, question{name: long, qtype: typeA, qclass: classIN})); err != nil {
		t.Errorf("name of %d bytes: %v", len(long), err)
	}
}

func TestLabel(t *testing.T) {
	for in, want := range map[string]string{
		"btproxy-bedroom":  "btproxy-bedroom",
		"BTProxy_Bedroom.": "btproxy-bedroom",
		"--Living room--":  "living-room",
		"":                 "btproxy",
	} {
		if got := Label(in); got != want {
			t.Errorf("Label(%q) = %q, want %q", in, got, want)
		}
	}
}
