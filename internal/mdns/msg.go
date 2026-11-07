package mdns

import (
	"encoding/binary"
	"errors"
	"net"
	"slices"
	"strings"
)

// DNS record types and classes used here.
const (
	typeA     = 1
	typePTR   = 12
	typeTXT   = 16
	typeSRV   = 33
	typeANY   = 255
	classIN   = 1
	topBit    = 0x8000 // cache-flush in an answer's class, unicast-response (QU) in a question's
	ttlHost   = 120    // A and SRV, as ESPHome and RFC 6762 use
	ttlOther  = 4500   // PTR and TXT
	ttlLegacy = 10     // answers to legacy unicast queries (RFC 6762 section 6.7)
)

const (
	servicesName = "_services._dns-sd._udp.local."
	serviceType  = "_esphomelib._tcp.local."
)

// Limits on a query. The answer to a legacy unicast query repeats its questions, so these also bound how much
// larger than the query that answer can be. Real queriers ask a few questions at most.
const (
	maxNameBytes = 255 // RFC 1035 section 3.1
	maxQuestions = 16
)

var errMalformed = errors.New("malformed packet")

type question struct {
	name    string // lower case, ending in a dot (see readName)
	wire    []byte // the name as the querier sent it, uncompressed, for the echo in a legacy response
	qtype   uint16
	qclass  uint16 // without the QU bit
	unicast bool   // QU bit: the querier asks for a unicast response
}

type query struct {
	id        uint16
	questions []question
	known     []record // the answer section: records the querier already holds (RFC 6762 section 7.1)
}

// parseQuery reads the questions and the known answers of a query. Responses, and queries with an opcode or
// rcode, are errors. The authority and additional sections are ignored.
func parseQuery(b []byte) (query, error) {
	if len(b) < 12 {
		return query{}, errMalformed
	}
	flags := binary.BigEndian.Uint16(b[2:])
	if flags&0xf80f != 0 { // QR, opcode, rcode
		return query{}, errors.New("not a standard query")
	}
	q := query{id: binary.BigEndian.Uint16(b)}
	n := int(binary.BigEndian.Uint16(b[4:]))
	if n > maxQuestions {
		return query{}, errMalformed
	}
	off := 12
	for range n {
		wire, next, err := readLabels(b, off)
		if err != nil {
			return query{}, err
		}
		if next+4 > len(b) {
			return query{}, errMalformed
		}
		class := binary.BigEndian.Uint16(b[next+2:])
		q.questions = append(q.questions, question{name: dotted(wire), wire: wire,
			qtype: binary.BigEndian.Uint16(b[next:]), qclass: class &^ topBit, unicast: class&topBit != 0})
		off = next + 4
	}
	for range binary.BigEndian.Uint16(b[6:]) {
		name, next, err := readName(b, off)
		if err != nil {
			return query{}, err
		}
		if next+10 > len(b) {
			return query{}, errMalformed
		}
		rec := record{name: name, rtype: binary.BigEndian.Uint16(b[next:]), ttl: binary.BigEndian.Uint32(b[next+4:])}
		class := binary.BigEndian.Uint16(b[next+2:]) &^ topBit
		l := int(binary.BigEndian.Uint16(b[next+8:]))
		off = next + 10
		if off+l > len(b) {
			return query{}, errMalformed
		}
		if rec.data, err = readData(b, off, l, rec.rtype); err != nil {
			return query{}, err
		}
		if class == classIN {
			q.known = append(q.known, rec)
		}
		off += l
	}
	return q, nil
}

// readData returns the l bytes of record data at off. The name in PTR and SRV data is decompressed and
// lower-cased, so that the data compares with ours.
func readData(b []byte, off, l int, rtype uint16) ([]byte, error) {
	skip := 0
	switch rtype {
	case typePTR:
	case typeSRV:
		skip = 6
	default:
		return slices.Clone(b[off : off+l]), nil
	}
	if l < skip {
		return nil, errMalformed
	}
	wire, next, err := readLabels(b, off+skip)
	if err != nil {
		return nil, err
	}
	if next != off+l {
		return nil, errMalformed
	}
	return append(slices.Clone(b[off:off+skip]), lower(string(wire))...), nil
}

// readName decodes a possibly compressed name at off and returns it lower-cased, as dotted labels ending in a
// dot, and the offset after it. A dot inside a label is escaped, so that the label cannot read as several.
func readName(b []byte, off int) (string, int, error) {
	wire, next, err := readLabels(b, off)
	if err != nil {
		return "", 0, err
	}
	return dotted(wire), next, nil
}

// readLabels decodes a possibly compressed name at off into its uncompressed wire form, with the label bytes as
// sent, and returns the offset after it.
func readLabels(b []byte, off int) ([]byte, int, error) {
	var wire []byte
	end := -1
	for jumps := 0; ; {
		if off >= len(b) {
			return nil, 0, errMalformed
		}
		l := int(b[off])
		switch {
		case l == 0:
			if end < 0 {
				end = off + 1
			}
			return append(wire, 0), end, nil
		case l&0xc0 == 0xc0:
			if off+1 >= len(b) || jumps > 16 {
				return nil, 0, errMalformed
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(b[off:]) & 0x3fff)
			jumps++
		case l&0xc0 != 0:
			return nil, 0, errMalformed
		default:
			if off+1+l > len(b) || len(wire)+l+1 >= maxNameBytes {
				return nil, 0, errMalformed
			}
			wire = append(wire, b[off:off+1+l]...)
			off += 1 + l
		}
	}
}

// dotted turns an uncompressed wire name into the lower-cased dotted form the records use, with a dot inside a
// label escaped.
func dotted(wire []byte) string {
	var sb strings.Builder
	for off := 0; off < len(wire) && wire[off] != 0; off += 1 + int(wire[off]) {
		for _, c := range wire[off+1 : off+1+int(wire[off])] {
			if c == '.' {
				sb.WriteByte('\\')
			}
			sb.WriteByte(c)
		}
		sb.WriteByte('.')
	}
	if sb.Len() == 0 {
		return "."
	}
	return lower(sb.String())
}

// lower lower-cases the ASCII letters of s and nothing else, as DNS names compare (RFC 6762 section 16). Unlike
// strings.ToLower it keeps every byte count, which the wire form of a name depends on.
func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// appendName writes name uncompressed; labels must not contain dots.
func appendName(b []byte, name string) []byte {
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if l == "" {
			continue
		}
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

type record struct {
	name  string
	rtype uint16
	flush bool // unique record: set the cache-flush bit
	ttl   uint32
	data  []byte
}

func (r record) key() string { return r.name + "/" + string(rune(r.rtype)) + "/" + string(r.data) }

// rdataKey returns r's data for comparison, with the name in PTR and SRV data lower-cased.
func (r record) rdataKey() string {
	switch {
	case r.rtype == typePTR:
		return lower(string(r.data))
	case r.rtype == typeSRV && len(r.data) >= 6:
		return string(r.data[:6]) + lower(string(r.data[6:]))
	}
	return string(r.data)
}

// knows reports whether the querier lists r among its known answers with a TTL of at least half of r's, in
// which case r must not be sent again (RFC 6762 section 7.1).
func (q query) knows(r record) bool {
	for _, k := range q.known {
		if k.rtype == r.rtype && k.ttl >= r.ttl/2 && k.name == lower(r.name) && k.rdataKey() == r.rdataKey() {
			return true
		}
	}
	return false
}

// service is what the responder advertises: one ESPHome API instance on one host.
type service struct {
	instance string // "<name>._esphomelib._tcp.local."
	host     string // "<name>.local."
	port     uint16
	txt      []string
}

func newService(name string, port int, txt []string) service {
	return service{instance: name + "." + serviceType, host: name + ".local.", port: uint16(port), txt: txt}
}

func (s service) ptrServices() record {
	return record{name: servicesName, rtype: typePTR, ttl: ttlOther, data: appendName(nil, serviceType)}
}

func (s service) ptr() record {
	return record{name: serviceType, rtype: typePTR, ttl: ttlOther, data: appendName(nil, s.instance)}
}

func (s service) srv() record {
	d := binary.BigEndian.AppendUint16(make([]byte, 4, 64), s.port) // priority and weight 0
	return record{name: s.instance, rtype: typeSRV, flush: true, ttl: ttlHost, data: appendName(d, s.host)}
}

func (s service) txtRecord() record {
	var d []byte
	for _, t := range s.txt {
		if len(t) > 255 {
			t = t[:255]
		}
		d = append(append(d, byte(len(t))), t...)
	}
	if d == nil {
		d = []byte{0}
	}
	return record{name: s.instance, rtype: typeTXT, flush: true, ttl: ttlOther, data: d}
}

func (s service) addrs(ips []net.IP) []record {
	var rs []record
	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil {
			rs = append(rs, record{name: s.host, rtype: typeA, flush: true, ttl: ttlHost, data: []byte(ip4)})
		}
	}
	return rs
}

// all is the full record set, for announcements and goodbyes.
func (s service) all(ips []net.IP) []record {
	return append([]record{s.ptr(), s.srv(), s.txtRecord(), s.ptrServices()}, s.addrs(ips)...)
}

// answer returns the answers and additional records for q, given the addresses of the interface it came in on,
// leaving out the answers the querier already knows. unicast reports that every question asked for a unicast
// response.
func (s service) answer(q query, ips []net.IP) (answers, extra []record, unicast bool) {
	have := map[string]bool{}
	add := func(list *[]record, rs ...record) {
		for _, r := range rs {
			if !have[r.key()] {
				have[r.key()] = true
				*list = append(*list, r)
			}
		}
	}
	unicast = len(q.questions) > 0
	for _, qs := range q.questions {
		unicast = unicast && qs.unicast
		if qs.qclass != classIN && qs.qclass != typeANY {
			continue
		}
		is := func(t uint16) bool { return qs.qtype == t || qs.qtype == typeANY }
		switch qs.name {
		case servicesName:
			if is(typePTR) {
				add(&answers, s.ptrServices())
			}
		case serviceType:
			if is(typePTR) {
				add(&answers, s.ptr())
			}
		case lower(s.instance):
			if is(typeSRV) {
				add(&answers, s.srv())
			}
			if is(typeTXT) {
				add(&answers, s.txtRecord())
			}
		case lower(s.host):
			if is(typeA) {
				add(&answers, s.addrs(ips)...)
			}
		}
	}
	answers = slices.DeleteFunc(answers, q.knows)
	if len(answers) == 0 {
		return nil, nil, false
	}
	// Additional records as RFC 6763 section 12 suggests, so one round trip resolves the service. have
	// keeps records already in the answers out of them.
	for _, r := range answers {
		switch r.rtype {
		case typePTR:
			if r.name == serviceType {
				add(&extra, s.srv(), s.txtRecord())
				add(&extra, s.addrs(ips)...)
			}
		case typeSRV:
			add(&extra, s.addrs(ips)...)
		}
	}
	return answers, extra, unicast
}

// encodeResponse builds an authoritative response. legacy (a query from a port other than 5353) echoes the id
// and questions as parsed from the query, caps TTLs and drops the cache-flush bit; ttl0 makes goodbye records.
func encodeResponse(id uint16, qs []question, answers, extra []record, legacy, ttl0 bool) []byte {
	b := make([]byte, 12, 512)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[2:], 0x8400) // QR, AA
	if legacy {
		binary.BigEndian.PutUint16(b[4:], uint16(len(qs)))
		for _, q := range qs {
			b = append(b, q.wire...)
			b = binary.BigEndian.AppendUint16(b, q.qtype)
			b = binary.BigEndian.AppendUint16(b, q.qclass)
		}
	}
	binary.BigEndian.PutUint16(b[6:], uint16(len(answers)))
	binary.BigEndian.PutUint16(b[10:], uint16(len(extra)))
	for _, r := range append(answers[:len(answers):len(answers)], extra...) {
		b = appendName(b, r.name)
		b = binary.BigEndian.AppendUint16(b, r.rtype)
		class := uint16(classIN)
		if r.flush && !legacy {
			class |= topBit
		}
		b = binary.BigEndian.AppendUint16(b, class)
		ttl := r.ttl
		if ttl0 {
			ttl = 0
		} else if legacy {
			ttl = min(ttl, ttlLegacy)
		}
		b = binary.BigEndian.AppendUint32(b, ttl)
		b = binary.BigEndian.AppendUint16(b, uint16(len(r.data)))
		b = append(b, r.data...)
	}
	return b
}

// Label turns s into a DNS host label: lower case letters, digits and dashes, at most 63 characters.
func Label(s string) string {
	var sb strings.Builder
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			sb.WriteRune(c)
		case sb.Len() > 0:
			sb.WriteByte('-')
		}
	}
	l := sb.String()
	if len(l) > 63 {
		l = l[:63]
	}
	l = strings.Trim(l, "-")
	if l == "" {
		l = "btproxy"
	}
	return l
}
