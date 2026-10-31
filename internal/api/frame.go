// Package api implements the server side of the ESPHome native API, limited to what a Bluetooth proxy needs.
package api

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxMessageSize matches ESPHome's limit; larger frames are rejected.
const MaxMessageSize = 32768

// ErrEncrypted is returned when a client opens with the Noise indicator byte (0x01).
var ErrEncrypted = errors.New("client requested encryption, which is not configured")

// Frame is one decoded API message: its type id and protobuf payload.
type Frame struct {
	Type    uint32
	Payload []byte
}

// ReadFrame reads one plaintext frame: 0x00, varint payload size, varint message type, payload.
func ReadFrame(r *bufio.Reader) (Frame, error) {
	ind, err := r.ReadByte()
	if err != nil {
		return Frame{}, err
	}
	switch ind {
	case 0x00:
	case 0x01:
		return Frame{}, ErrEncrypted
	default:
		return Frame{}, fmt.Errorf("bad indicator byte 0x%02x", ind)
	}
	size, err := binary.ReadUvarint(r)
	if err != nil {
		return Frame{}, err
	}
	if size > MaxMessageSize {
		return Frame{}, fmt.Errorf("message of %d bytes is too large", size)
	}
	typ, err := binary.ReadUvarint(r)
	if err != nil {
		return Frame{}, err
	}
	if typ > 0xffff {
		return Frame{}, fmt.Errorf("message type %d out of range", typ)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}
	return Frame{Type: uint32(typ), Payload: payload}, nil
}

// AppendFrame appends the plaintext encoding of a frame to b.
func AppendFrame(b []byte, typ uint32, payload []byte) []byte {
	b = append(b, 0x00)
	b = binary.AppendUvarint(b, uint64(len(payload)))
	b = binary.AppendUvarint(b, uint64(typ))
	return append(b, payload...)
}
