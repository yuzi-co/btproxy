// Package hci reads advertisements from the controller's HCI events through the kernel's monitor channel, the
// one btmon uses, so they reach Home Assistant as they were sent on the air instead of rebuilt from what BlueZ
// parsed. The parsing here is portable; the socket is Linux only.
package hci

import (
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
)

const (
	evtLEMeta         = 0x3e
	subLEAdvReport    = 0x02
	subLEExtAdvReport = 0x0d

	maxData   = 62                     // an ESP32 proxy's limit: advertisement plus scan response
	holdFor   = 200 * time.Millisecond // how long a scannable advertisement waits for its scan response
	holdSlots = 8
	fragSlots = 4
)

// Extended advertising report event type bits (Core 5.x, Vol 4, Part E, 7.7.65.13).
const (
	extScannable = 1 << 1
	extScanRsp   = 1 << 3
	extLegacy    = 1 << 4
	extStatus    = 3 << 5 // data status: 0 complete, 1 incomplete with more to come, 2 incomplete and truncated
)

const statusMore = 1

// report is one advertising report. data points into the event buffer.
type report struct {
	addr      uint64
	addrType  uint32
	rssi      int8
	scannable bool // a scan request gets a scan response
	scanRsp   bool
	status    int // extended reports' data status
	data      []byte
}

// parseEvent calls fn for every advertising report in an HCI event packet: event code, parameter length,
// parameters. Other events are ignored; a malformed report ends the parsing, after the reports before it.
// Reports without an address (anonymous extended advertising) or without an RSSI are skipped.
func parseEvent(ev []byte, fn func(report)) {
	if len(ev) < 4 || ev[0] != evtLEMeta || int(ev[1]) != len(ev)-2 {
		return
	}
	p := ev[2:]
	sub, n := p[0], int(p[1])
	p = p[2:]
	for i := 0; i < n; i++ {
		var r report
		var at byte
		switch sub {
		case subLEAdvReport:
			// Event type, address type, address, data length, data, RSSI. The specification lists the fields
			// as arrays over all reports, but controllers and the kernel lay them out report by report; with
			// one report per event, as nearly always, the two agree.
			if len(p) < 9 || len(p) < 10+int(p[8]) {
				return
			}
			dl := int(p[8])
			typ := p[0]
			at, r.data, r.rssi = p[1], p[9:9+dl], int8(p[9+dl])
			r.addr = addrOf(p[2:8])
			r.scannable = typ == 0x00 || typ == 0x02 // ADV_IND, ADV_SCAN_IND
			r.scanRsp = typ == 0x04
			p = p[10+dl:]
		case subLEExtAdvReport:
			// Event type (2), address type, address, primary PHY, secondary PHY, SID, TX power, RSSI,
			// periodic interval (2), direct address type, direct address, data length, data.
			if len(p) < 24 || len(p) < 24+int(p[23]) {
				return
			}
			dl := int(p[23])
			typ := int(p[0]) | int(p[1])<<8
			at, r.data, r.rssi = p[2], p[24:24+dl], int8(p[13])
			r.addr = addrOf(p[3:9])
			r.scannable = typ&extScannable != 0
			r.scanRsp = typ&extScanRsp != 0
			r.status = typ & extStatus >> 5
			p = p[24+dl:]
		default:
			return
		}
		if at == 0xff || r.rssi == 127 {
			continue
		}
		// 2 and 3 are public and random identity addresses the controller resolved from a private one.
		r.addrType = ble.AddrPublic
		if at&1 != 0 {
			r.addrType = ble.AddrRandom
		}
		fn(r)
	}
}

// addrOf turns a little-endian device address into the form ble uses (0xAABBCCDDEEFF for AA:BB:...:FF).
func addrOf(b []byte) uint64 {
	return uint64(b[5])<<40 | uint64(b[4])<<32 | uint64(b[3])<<24 | uint64(b[2])<<16 | uint64(b[1])<<8 | uint64(b[0])
}

// key identifies an advertiser: address and address type, never 0.
func (r *report) key() uint64 { return r.addr | uint64(r.addrType+1)<<48 }

// held is a scannable advertisement waiting for its scan response.
type held struct {
	key  uint64 // 0: free
	at   time.Time
	ad   ble.Advert
	data [maxData]byte
}

// decoder turns advertising reports into advertisements the way an ESP32 proxy reports them. With active
// scanning, Bluedroid (ESP-IDF's default configuration, which ESPHome keeps) holds the advertisement of a
// scannable device until its scan response arrives and reports the two as one, data concatenated, with the
// response's RSSI; everything else is reported as it arrives. The decoder does the same, except that a held
// advertisement whose response does not come within holdFor, or that a newer one from the same device
// replaces, is reported alone instead of being dropped. Its memory is fixed: holdSlots held advertisements
// and fragSlots fragmented extended advertisements in progress. Advertisements are passed to emit with their
// data in the decoder's own buffers, as ble.Advert allows, so decoding allocates nothing.
type decoder struct {
	emit   func(ble.Advert)
	active bool // the scan is active, so scan responses come

	held  [holdSlots]held
	frags [fragSlots]uint64 // keys of devices whose extended advertisement continues in the next report
	fragN int
	buf   [maxData]byte // data of the advertisement being emitted
}

// event handles one HCI event received at now.
func (d *decoder) event(ev []byte, now time.Time) {
	d.expire(now)
	parseEvent(ev, func(r report) { d.report(r, now) })
}

func (d *decoder) report(r report, now time.Time) {
	k := r.key()
	// An extended advertisement longer than one event arrives in fragments. Only the first is used: the
	// result is capped at maxData anyway, and AD structures cut at the fragment's end are dropped.
	if r.status == statusMore {
		if d.inFragment(k, false) {
			return
		}
		d.frags[d.fragN] = k
		d.fragN = (d.fragN + 1) % fragSlots
	} else if d.inFragment(k, true) {
		return
	}

	ad := ble.Advert{Addr: r.addr, AddrType: r.addrType, RSSI: r.rssi}
	switch {
	case r.scanRsp:
		var prev []byte
		if h := d.find(k); h != nil {
			prev = h.ad.Data
			h.key = 0
		}
		ad.Data = appendAD(appendAD(d.buf[:0], prev), r.data)
	case d.active && r.scannable:
		if h := d.find(k); h != nil {
			d.release(h)
		}
		h := d.slot()
		h.key, h.at, h.ad = k, now, ad
		h.ad.Data = appendAD(h.data[:0], r.data)
		return
	default:
		ad.Data = appendAD(d.buf[:0], r.data)
	}
	d.emit(ad)
}

// inFragment tells whether k has a fragmented advertisement in progress and, if done, forgets it.
func (d *decoder) inFragment(k uint64, done bool) bool {
	for i, f := range d.frags {
		if f == k {
			if done {
				d.frags[i] = 0
			}
			return true
		}
	}
	return false
}

func (d *decoder) find(k uint64) *held {
	for i := range d.held {
		if d.held[i].key == k {
			return &d.held[i]
		}
	}
	return nil
}

// slot returns a free slot, releasing the oldest held advertisement if there is none.
func (d *decoder) slot() *held {
	var oldest *held
	for i := range d.held {
		h := &d.held[i]
		if h.key == 0 {
			return h
		}
		if oldest == nil || h.at.Before(oldest.at) {
			oldest = h
		}
	}
	d.release(oldest)
	return oldest
}

// release reports a held advertisement alone and frees its slot. Its data stays in the slot until emit
// returns, since nothing takes the slot before then.
func (d *decoder) release(h *held) {
	h.key = 0
	d.emit(h.ad)
}

// expire releases the advertisements whose scan response is overdue. Called for every event and, while no
// events arrive, periodically.
func (d *decoder) expire(now time.Time) {
	for i := range d.held {
		if h := &d.held[i]; h.key != 0 && (!d.active || now.Sub(h.at) >= holdFor) {
			d.release(h)
		}
	}
}

// appendAD appends src to dst within maxData bytes. If it does not fit whole (only possible for extended
// advertisements), the AD structures that fit are kept and the others dropped; a zero length or a structure
// running past the end stops the parsing there.
func appendAD(dst, src []byte) []byte {
	if len(dst)+len(src) <= maxData {
		return append(dst, src...)
	}
	for len(src) > 0 {
		n := 1 + int(src[0])
		if n == 1 || n > len(src) {
			break
		}
		if len(dst)+n <= maxData {
			dst = append(dst, src[:n]...)
		}
		src = src[n:]
	}
	return dst
}
