package ble

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
)

// AdvFilter decides which advertisements are forwarded to Home Assistant. An advertisement is dropped when it
// matches a deny entry, or when there are allow entries and it matches none of them. Entries match the
// address (a whole MAC or a prefix such as an OUI), a 16-, 32- or 128-bit service UUID in the UUID lists or
// the service data, or the company id of manufacturer data. Only advertisements are filtered: connections are
// made whatever the filter says. A nil *AdvFilter forwards everything. Allow does not allocate.
type AdvFilter struct {
	allow, deny matcher
	denyRandom  bool
}

// matcher holds the entries of one list.
type matcher struct {
	addrs []addrPrefix
	short []uint32 // 16- and 32-bit UUIDs, and 128-bit ones on the Bluetooth base UUID
	long  []UUID
	mfr   []uint16
}

// addrPrefix matches the addresses a with a&mask == v.
type addrPrefix struct{ v, mask uint64 }

// ParseAdvFilter builds a filter from comma-separated allow and deny entries. Entries are
// "AA:BB:CC:DD:EE:FF" (an address), "AA:BB:CC" (an OUI) or "AA:*", "AA:BB:CC:DD:*" (an address prefix),
// "uuid:fe95", "uuid:0000fe95" or "uuid:<36-character UUID>" (a service UUID) and "mfr:0x004c" or "mfr:76"
// (a company id). denyRandom drops advertisements from private addresses, resolvable and non-resolvable;
// random static addresses are kept. It returns nil when nothing is configured.
func ParseAdvFilter(allow, deny string, denyRandom bool) (*AdvFilter, error) {
	f := &AdvFilter{denyRandom: denyRandom}
	if err := f.allow.parse(allow); err != nil {
		return nil, err
	}
	if err := f.deny.parse(deny); err != nil {
		return nil, err
	}
	if f.allow.empty() && f.deny.empty() && !denyRandom {
		return nil, nil
	}
	return f, nil
}

// Allow tells whether ad is forwarded.
func (f *AdvFilter) Allow(ad Advert) bool {
	if f == nil {
		return true
	}
	// The two most significant bits of a random address tell its kind: 11 static, 01 resolvable private,
	// 00 non-resolvable private.
	if f.denyRandom && ad.AddrType == AddrRandom && ad.Addr>>46 != 3 {
		return false
	}
	if f.deny.match(ad) {
		return false
	}
	return f.allow.empty() || f.allow.match(ad)
}

func (f *AdvFilter) String() string {
	if f == nil {
		return "none"
	}
	s := "allow " + f.allow.String() + "; deny " + f.deny.String()
	if f.denyRandom {
		s += " and private addresses"
	}
	return s
}

func (m *matcher) empty() bool {
	return len(m.addrs) == 0 && !m.wantsData()
}

// wantsData tells whether matching needs the advertisement's data.
func (m *matcher) wantsData() bool {
	return len(m.short) != 0 || len(m.long) != 0 || len(m.mfr) != 0
}

func (m *matcher) match(ad Advert) bool {
	for _, p := range m.addrs {
		if ad.Addr&p.mask == p.v {
			return true
		}
	}
	if !m.wantsData() {
		return false
	}
	for t, v := range ADs(ad.Data) {
		switch t {
		case 0x02, 0x03: // incomplete and complete lists of 16-bit service UUIDs
			for ; len(v) >= 2; v = v[2:] {
				if m.hasShort(uint32(binary.LittleEndian.Uint16(v))) {
					return true
				}
			}
		case 0x04, 0x05: // 32-bit
			for ; len(v) >= 4; v = v[4:] {
				if m.hasShort(binary.LittleEndian.Uint32(v)) {
					return true
				}
			}
		case 0x06, 0x07: // 128-bit
			for ; len(v) >= 16; v = v[16:] {
				if m.hasLong(v) {
					return true
				}
			}
		case 0x16: // service data, 16-bit UUID
			if len(v) >= 2 && m.hasShort(uint32(binary.LittleEndian.Uint16(v))) {
				return true
			}
		case 0x20: // service data, 32-bit UUID
			if len(v) >= 4 && m.hasShort(binary.LittleEndian.Uint32(v)) {
				return true
			}
		case 0x21: // service data, 128-bit UUID
			if len(v) >= 16 && m.hasLong(v) {
				return true
			}
		case 0xff: // manufacturer data, company id first
			if len(v) >= 2 {
				id := binary.LittleEndian.Uint16(v)
				for _, c := range m.mfr {
					if c == id {
						return true
					}
				}
			}
		}
	}
	return false
}

func (m *matcher) hasShort(u uint32) bool {
	for _, s := range m.short {
		if s == u {
			return true
		}
	}
	return false
}

// hasLong matches a 128-bit UUID as sent on the air, least significant byte first.
func (m *matcher) hasLong(b []byte) bool {
	u := UUIDFromLE(b[:16])
	if s, ok := u.Short(); ok {
		return m.hasShort(s)
	}
	for _, l := range m.long {
		if l == u {
			return true
		}
	}
	return false
}

func (m *matcher) parse(list string) error {
	for _, e := range strings.Split(list, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if err := m.add(strings.ToLower(e)); err != nil {
			return fmt.Errorf("advertisement filter entry %q: %w", e, err)
		}
	}
	return nil
}

func (m *matcher) add(e string) error {
	if s, ok := strings.CutPrefix(e, "mfr:"); ok {
		id, err := strconv.ParseUint(s, 0, 16)
		if err != nil {
			return fmt.Errorf("bad company id")
		}
		m.mfr = append(m.mfr, uint16(id))
		return nil
	}
	if s, ok := strings.CutPrefix(e, "uuid:"); ok {
		s = strings.TrimPrefix(s, "0x")
		if len(s) == 4 || len(s) == 8 {
			v, err := strconv.ParseUint(s, 16, 32)
			if err != nil {
				return fmt.Errorf("bad uuid")
			}
			m.short = append(m.short, uint32(v))
			return nil
		}
		u, err := ParseUUID(s)
		if err != nil {
			return err
		}
		if v, ok := u.Short(); ok {
			m.short = append(m.short, v)
		} else {
			m.long = append(m.long, u)
		}
		return nil
	}
	s := strings.TrimSuffix(strings.TrimSuffix(e, "*"), ":")
	parts := strings.Split(s, ":")
	if len(parts) > 6 {
		return fmt.Errorf("bad address")
	}
	if len(parts) < 6 && !strings.HasSuffix(e, "*") && len(parts) != 3 {
		// Only an OUI may leave out the "*": a shortened MAC is more likely a typo than a prefix.
		return fmt.Errorf("bad address: a prefix other than an OUI needs a trailing \"*\"")
	}
	var p addrPrefix
	for i, o := range parts {
		v, err := strconv.ParseUint(o, 16, 8)
		if err != nil || len(o) != 2 {
			return fmt.Errorf("bad address")
		}
		shift := 8 * (5 - i)
		p.v |= v << shift
		p.mask |= 0xff << shift
	}
	m.addrs = append(m.addrs, p)
	return nil
}

func (m *matcher) String() string {
	var s []string
	for _, p := range m.addrs {
		n := bits.OnesCount64(p.mask) / 8
		a := FormatMAC(p.v)[:3*n-1]
		if n < 6 {
			a += ":*"
		}
		s = append(s, a)
	}
	for _, u := range m.short {
		s = append(s, fmt.Sprintf("uuid:%04x", u))
	}
	for _, u := range m.long {
		s = append(s, "uuid:"+u.String())
	}
	for _, c := range m.mfr {
		s = append(s, fmt.Sprintf("mfr:0x%04x", c))
	}
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, ",")
}
