package api

import (
	"errors"

	fe "filippo.io/edwards25519/field"
)

// x25519 computes the X25519 function of RFC 7748, the Montgomery ladder as Go's crypto/ecdh runs it. It is
// here instead of crypto/ecdh because that package links the NIST curves too, over 100 kB on mipsle.
func x25519(scalar, point []byte) ([]byte, error) {
	if len(scalar) != 32 || len(point) != 32 {
		return nil, errors.New("x25519: bad input length")
	}
	var e [32]byte
	copy(e[:], scalar)
	e[0] &= 248
	e[31] &= 127
	e[31] |= 64

	var x1, x2, z2, x3, z3, tmp0, tmp1 fe.Element
	if _, err := x1.SetBytes(point); err != nil { // ignores the top bit, as RFC 7748 asks
		return nil, err
	}
	x2.One()
	x3.Set(&x1)
	z3.One()

	swap := 0
	for pos := 254; pos >= 0; pos-- {
		b := int(e[pos/8]>>uint(pos&7)) & 1
		swap ^= b
		x2.Swap(&x3, swap)
		z2.Swap(&z3, swap)
		swap = b

		tmp0.Subtract(&x3, &z3)
		tmp1.Subtract(&x2, &z2)
		x2.Add(&x2, &z2)
		z2.Add(&x3, &z3)
		z3.Multiply(&tmp0, &x2)
		z2.Multiply(&z2, &tmp1)
		tmp0.Square(&tmp1)
		tmp1.Square(&x2)
		x3.Add(&z3, &z2)
		z2.Subtract(&z3, &z2)
		x2.Multiply(&tmp1, &tmp0)
		tmp1.Subtract(&tmp1, &tmp0)
		z2.Square(&z2)

		z3.Mult32(&tmp1, 121666)
		x3.Square(&x3)
		tmp0.Add(&tmp0, &z3)
		z3.Multiply(&x1, &z2)
		z2.Multiply(&tmp1, &tmp0)
	}
	x2.Swap(&x3, swap)
	z2.Swap(&z3, swap)

	z2.Invert(&z2)
	x2.Multiply(&x2, &z2)
	out := x2.Bytes()
	var zero [32]byte
	if [32]byte(out) == zero { // a low-order point: refuse, as crypto/ecdh does
		return nil, errors.New("x25519: low order point")
	}
	return out, nil
}

// x25519Base is the public key of a private key.
func x25519Base(scalar []byte) ([]byte, error) {
	base := [32]byte{9}
	return x25519(scalar, base[:])
}
