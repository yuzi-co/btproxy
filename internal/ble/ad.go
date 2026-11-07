package ble

import "iter"

// MaxAdvert is the most AD data an advertisement carries to Home Assistant: an ESP32 proxy's limit for an
// advertisement plus its scan response.
const MaxAdvert = 62

// ADs walks the AD structures in data (length, type, value; the length counts the type) and yields the type
// and value of each. A zero length or a structure running past the end of data stops the walk there. The
// walk allocates nothing: the scan path is built on it.
func ADs(data []byte) iter.Seq2[byte, []byte] {
	return func(yield func(byte, []byte) bool) {
		for len(data) >= 2 {
			n := 1 + int(data[0])
			if n == 1 || n > len(data) {
				return
			}
			if !yield(data[1], data[2:n]) {
				return
			}
			data = data[n:]
		}
	}
}

// WholeAD is the part of data made of complete AD structures: a fragment of an extended advertisement can end
// inside one.
func WholeAD(data []byte) []byte {
	n := 0
	for _, v := range ADs(data) {
		n += 2 + len(v)
	}
	return data[:n]
}

// AppendAD appends one AD structure to dst, as it is on air. A structure that would take dst past MaxAdvert,
// or whose value is too long to encode, is left out, so the result always parses.
func AppendAD(dst []byte, typ byte, v []byte) []byte {
	if len(v) > 254 || len(dst)+2+len(v) > MaxAdvert {
		return dst
	}
	dst = append(dst, byte(len(v)+1), typ)
	return append(dst, v...)
}

// AppendADs appends the AD structures in src to dst within MaxAdvert bytes. If src does not fit whole (only
// possible for extended advertisements), the structures that fit are kept and the others dropped, with ADs'
// rules for where the parsing stops.
func AppendADs(dst, src []byte) []byte {
	if len(dst)+len(src) <= MaxAdvert {
		return append(dst, src...)
	}
	for typ, v := range ADs(src) {
		dst = AppendAD(dst, typ, v)
	}
	return dst
}
