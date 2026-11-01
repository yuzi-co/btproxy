package api

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// TestX25519RFC7748 runs the test vectors of RFC 7748 section 5.2 and the Diffie-Hellman example of 6.1.
func TestX25519RFC7748(t *testing.T) {
	for _, v := range []struct{ scalar, point, out string }{
		{"a546e36bf0527c9d3b16154b82465edd62144c0ac1fc5a18506a2244ba449ac4",
			"e6db6867583030db3594c1a424b15f7c726624ec26b3353b10a903a6d0ab1c4c",
			"c3da55379de9c6908e94ea4df28d084f32eccf03491c71f754b4075577a28552"},
		{"4b66e9d4d1b4673c5ad22691957d6af5c11b6421e0ea01d42ca4169e7918ba0d",
			"e5210f12786811d3f4b7959d0538ae2c31dbe7106fc03c3efc4cd549c715a493",
			"95cbde9476e8907d7aade45cb4b873f88b595a68799fa152e6f8f7647aac7957"},
		{"77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a",
			"0900000000000000000000000000000000000000000000000000000000000000",
			"8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a"},
		{"77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a",
			"de9edb7d7b7dc1b4d35b61c2ece435373f8343c85b78674dadfc7e146f882b4f",
			"4a5d9d5ba4ce2de1728e3bf480350f25e07e21c947d19e3376f09b3c1e161742"},
	} {
		got, err := x25519(unhex(t, v.scalar), unhex(t, v.point))
		if err != nil || hex.EncodeToString(got) != v.out {
			t.Errorf("x25519(%s, %s) = %x, %v; want %s", v.scalar, v.point, got, err, v.out)
		}
	}
	if _, err := x25519(seq(1), make([]byte, 32)); err == nil {
		t.Error("low order point accepted")
	}
}

// TestX25519MatchesStdlib compares random key agreements with crypto/ecdh.
func TestX25519MatchesStdlib(t *testing.T) {
	for range 50 {
		a, _ := ecdh.X25519().GenerateKey(rand.Reader)
		b, _ := ecdh.X25519().GenerateKey(rand.Reader)
		pub, err := x25519Base(a.Bytes())
		if err != nil || !bytes.Equal(pub, a.PublicKey().Bytes()) {
			t.Fatalf("public key %x, %v; want %x", pub, err, a.PublicKey().Bytes())
		}
		want, _ := a.ECDH(b.PublicKey())
		got, err := x25519(a.Bytes(), b.PublicKey().Bytes())
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("shared %x, %v; want %x", got, err, want)
		}
	}
}
