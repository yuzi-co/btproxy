package api

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// enc builds a protobuf message field by field. Zero values are skipped, as proto3 does.
type enc struct{ b []byte }

func (e *enc) uint(num protowire.Number, v uint64) {
	if v != 0 {
		e.b = protowire.AppendTag(e.b, num, protowire.VarintType)
		e.b = protowire.AppendVarint(e.b, v)
	}
}

func (e *enc) int32(num protowire.Number, v int32) {
	if v != 0 {
		e.b = protowire.AppendTag(e.b, num, protowire.VarintType)
		e.b = protowire.AppendVarint(e.b, uint64(int64(v)))
	}
}

func (e *enc) sint(num protowire.Number, v int64) {
	if v != 0 {
		e.b = protowire.AppendTag(e.b, num, protowire.VarintType)
		e.b = protowire.AppendVarint(e.b, protowire.EncodeZigZag(v))
	}
}

func (e *enc) bool(num protowire.Number, v bool) {
	if v {
		e.uint(num, 1)
	}
}

func (e *enc) bytes(num protowire.Number, v []byte) {
	if len(v) > 0 {
		e.b = protowire.AppendTag(e.b, num, protowire.BytesType)
		e.b = protowire.AppendBytes(e.b, v)
	}
}

func (e *enc) str(num protowire.Number, v string) { e.bytes(num, []byte(v)) }

// msg embeds a sub-message, even an empty one, because repeated messages must keep their position.
func (e *enc) msg(num protowire.Number, sub []byte) {
	e.b = protowire.AppendTag(e.b, num, protowire.BytesType)
	e.b = protowire.AppendBytes(e.b, sub)
}

// field is one decoded protobuf field: varints in v, length-delimited data in data.
type field struct {
	num  protowire.Number
	v    uint64
	data []byte
}

// fields decodes a flat message. Fixed-width fields are read into v as well.
func fields(b []byte) ([]field, error) {
	var out []field
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("bad tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		f := field{num: num}
		switch typ {
		case protowire.VarintType:
			f.v, n = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			f.data, n = protowire.ConsumeBytes(b)
		case protowire.Fixed32Type:
			var v uint32
			v, n = protowire.ConsumeFixed32(b)
			f.v = uint64(v)
		case protowire.Fixed64Type:
			f.v, n = protowire.ConsumeFixed64(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return nil, fmt.Errorf("bad field %d: %w", num, protowire.ParseError(n))
		}
		b = b[n:]
		out = append(out, f)
	}
	return out, nil
}
