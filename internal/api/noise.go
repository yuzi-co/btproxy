//go:build !nonoise

package api

import (
	"bufio"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	"golang.org/x/crypto/chacha20poly1305"
)

// The ESPHome API's encrypted transport: Noise_NNpsk0_25519_ChaChaPoly_SHA256 with the device's 32-byte key as
// the PSK. Frames are 0x01, a 16-bit big-endian size and the payload. The client opens with an empty hello
// frame and its handshake message; the server answers with a server hello (0x01, node name, MAC) and its
// handshake message, then both sides switch to the transport ciphers. Only the responder side of NNpsk0 is
// implemented, which is all the server needs:
//
//	-> psk, e
//	<- e, ee
const (
	NoiseProtocol = "Noise_NNpsk0_25519_ChaChaPoly_SHA256"

	// Encryption tells whether the encrypted API is built in. The nonoise build tag leaves it out.
	Encryption = true

	noiseIndicator     = 0x01
	maxHandshakeSize   = 128 // ESPHome's limit for frames before the handshake completes
	noiseTagSize       = chacha20poly1305.Overhead
	maxNoiseFrameSize  = 4 + MaxMessageSize + noiseTagSize
	noisePrologueStart = "NoiseAPIInit"
)

var (
	// ErrPlaintext is returned when a client opens without encryption although a key is configured.
	ErrPlaintext = errors.New("client did not request encryption, which is required")
	// errNoiseMAC is a handshake message that fails authentication: the client has a different key.
	errNoiseMAC = errors.New("handshake MAC failure")
	errTooLarge = errors.New("frame too large")
)

// readNoiseFrame reads one encrypted-transport frame of at most limit bytes.
func readNoiseFrame(r *bufio.Reader, limit int) ([]byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:1]); err != nil {
		return nil, err
	}
	switch hdr[0] {
	case noiseIndicator:
	case 0x00:
		return nil, ErrPlaintext
	default:
		return nil, fmt.Errorf("bad indicator byte 0x%02x", hdr[0])
	}
	if _, err := io.ReadFull(r, hdr[1:]); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(hdr[1:]))
	if size > limit {
		return nil, fmt.Errorf("%w: %d bytes", errTooLarge, size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func appendNoiseFrame(b, payload []byte) []byte {
	b = append(b, noiseIndicator)
	b = binary.BigEndian.AppendUint16(b, uint16(len(payload)))
	return append(b, payload...)
}

// noiseReject is the frame ESPHome sends when it refuses a handshake: status 0x01 and the reason. Clients
// show "Handshake MAC failure" as a wrong key.
func noiseReject(reason string) []byte {
	return appendNoiseFrame(nil, append([]byte{0x01}, reason...))
}

// noiseCipher is a Noise CipherState: ChaCha20-Poly1305 with a 64-bit counter nonce.
type noiseCipher struct {
	aead cipher.AEAD
	n    uint64
}

func newNoiseCipher(k []byte) *noiseCipher {
	aead, err := chacha20poly1305.New(k[:32])
	if err != nil {
		panic(err) // the key is always 32 bytes
	}
	return &noiseCipher{aead: aead}
}

func (c *noiseCipher) nonce() []byte {
	var nonce [chacha20poly1305.NonceSize]byte
	binary.LittleEndian.PutUint64(nonce[4:], c.n)
	c.n++
	return nonce[:]
}

func (c *noiseCipher) seal(dst, ad, plain []byte) []byte {
	return c.aead.Seal(dst, c.nonce(), plain, ad)
}

func (c *noiseCipher) open(dst, ad, ct []byte) ([]byte, error) {
	return c.aead.Open(dst, c.nonce(), ct, ad)
}

// noiseSymmetric is the Noise SymmetricState during the handshake.
type noiseSymmetric struct {
	ck, h []byte
	k     *noiseCipher
}

func newNoiseSymmetric(prologue []byte) *noiseSymmetric {
	h := sha256.Sum256([]byte(NoiseProtocol)) // the name is longer than the hash, so it is hashed
	s := &noiseSymmetric{ck: h[:], h: h[:]}
	s.mixHash(prologue)
	return s
}

func (s *noiseSymmetric) mixHash(data []byte) {
	d := sha256.New()
	d.Write(s.h)
	d.Write(data)
	s.h = d.Sum(nil)
}

func (s *noiseSymmetric) mixKey(ikm []byte) {
	out := noiseHKDF(s.ck, ikm, 2)
	s.ck, s.k = out[0], newNoiseCipher(out[1])
}

func (s *noiseSymmetric) mixKeyAndHash(ikm []byte) {
	out := noiseHKDF(s.ck, ikm, 3)
	s.ck = out[0]
	s.mixHash(out[1])
	s.k = newNoiseCipher(out[2])
}

func (s *noiseSymmetric) encryptAndHash(plain []byte) []byte {
	ct := s.k.seal(nil, s.h, plain)
	s.mixHash(ct)
	return ct
}

func (s *noiseSymmetric) decryptAndHash(ct []byte) ([]byte, error) {
	plain, err := s.k.open(nil, s.h, ct)
	if err != nil {
		return nil, errNoiseMAC
	}
	s.mixHash(ct)
	return plain, nil
}

// split returns the initiator-to-responder and responder-to-initiator transport ciphers.
func (s *noiseSymmetric) split() (*noiseCipher, *noiseCipher) {
	out := noiseHKDF(s.ck, nil, 2)
	return newNoiseCipher(out[0]), newNoiseCipher(out[1])
}

// noiseHKDF is the HKDF of the Noise specification with HMAC-SHA256, returning n outputs.
func noiseHKDF(ck, ikm []byte, n int) [][]byte {
	m := hmac.New(sha256.New, ck)
	m.Write(ikm)
	temp := m.Sum(nil)
	out := make([][]byte, n)
	var prev []byte
	for i := range out {
		m = hmac.New(sha256.New, temp)
		m.Write(prev)
		m.Write([]byte{byte(i + 1)})
		prev = m.Sum(nil)
		out[i] = prev
	}
	return out
}

// noiseRespond processes the initiator's handshake message (without ESPHome's status byte) and returns the
// reply and the transport ciphers. e is the responder's ephemeral private key; nil generates one.
func noiseRespond(psk, prologue, msg, e []byte) (reply []byte, recv, send *noiseCipher, err error) {
	if len(msg) < 32+noiseTagSize {
		return nil, nil, nil, fmt.Errorf("handshake message of %d bytes is too short", len(msg))
	}
	s := newNoiseSymmetric(prologue)
	// -> psk, e
	s.mixKeyAndHash(psk)
	re := msg[:32]
	s.mixHash(re)
	s.mixKey(re)
	if _, err := s.decryptAndHash(msg[32:]); err != nil {
		return nil, nil, nil, err
	}
	// <- e, ee
	if e == nil {
		e = make([]byte, 32)
		if _, err := rand.Read(e); err != nil {
			return nil, nil, nil, err
		}
	}
	pub, err := x25519Base(e)
	if err != nil {
		return nil, nil, nil, err
	}
	s.mixHash(pub)
	s.mixKey(pub)
	shared, err := x25519(e, re)
	if err != nil {
		return nil, nil, nil, err
	}
	s.mixKey(shared)
	reply = append(pub, s.encryptAndHash(nil)...)
	recv, send = s.split()
	return reply, recv, send, nil
}

// noiseHandshake runs the server side of ESPHome's encrypted handshake on a new connection. Refusals are sent
// as ESPHome words them, so clients report a wrong key or a missing encryption setting as such.
func (c *client) noiseHandshake(r *bufio.Reader) error {
	s := c.s
	reject := func(reason string, err error) error {
		c.writeRaw(noiseReject(reason))
		return err
	}
	frameErr := func(err error) error {
		switch {
		case errors.Is(err, ErrPlaintext):
			return reject("Bad indicator byte", err)
		case errors.Is(err, errTooLarge):
			return reject("Bad handshake packet len", err)
		}
		return err
	}
	hello, err := readNoiseFrame(r, maxHandshakeSize)
	if err != nil {
		return frameErr(err)
	}
	// The hello's contents are unused (reserved for flags), but they are part of the prologue.
	prologue := binary.BigEndian.AppendUint16([]byte(noisePrologueStart), uint16(len(hello)))
	prologue = append(prologue, hello...)

	// Server hello: the chosen protocol (0x01, the only one), then node name and MAC, each ending in a zero byte.
	// The client checks them against the entry it connects for before it sends anything encrypted.
	hello = append([]byte{0x01}, s.cfg.Name...)
	hello = append(append(hello, 0), noiseMAC(s.cfg.MAC)...)
	c.writeRaw(appendNoiseFrame(nil, append(hello, 0)))

	msg, err := readNoiseFrame(r, maxHandshakeSize)
	switch {
	case err != nil:
		return frameErr(err)
	case len(msg) == 0:
		return reject("Empty handshake message", errors.New("empty handshake message"))
	case msg[0] != 0x00:
		return reject("Bad handshake error byte", fmt.Errorf("handshake error byte 0x%02x", msg[0]))
	}
	reply, recv, send, err := noiseRespond(s.cfg.EncryptionKey, prologue, msg[1:], nil)
	if errors.Is(err, errNoiseMAC) {
		return reject("Handshake MAC failure", errors.New("handshake MAC failure (wrong encryption key)"))
	} else if err != nil {
		return reject("Handshake error", err)
	}
	// The reply is queued before the transport cipher is set, so nothing encrypted can go ahead of it.
	c.recv = recv
	c.writeRaw(appendNoiseFrame(nil, append([]byte{0x00}, reply...)))
	c.out.setCipher(send)
	return nil
}

// readNoise reads and decrypts one message: 16-bit type, 16-bit size, payload.
func (c *client) readNoise(r *bufio.Reader) (Frame, error) {
	ct, err := readNoiseFrame(r, maxNoiseFrameSize)
	if err != nil {
		return Frame{}, err
	}
	m, err := c.recv.open(ct[:0], nil, ct)
	if err != nil {
		return Frame{}, errors.New("decryption failed")
	}
	if len(m) < 4 {
		return Frame{}, fmt.Errorf("decrypted message of %d bytes is too short", len(m))
	}
	size := int(binary.BigEndian.Uint16(m[2:]))
	if size > len(m)-4 {
		return Frame{}, fmt.Errorf("message size %d exceeds the frame", size)
	}
	return Frame{Type: uint32(binary.BigEndian.Uint16(m)), Payload: m[4 : 4+size]}, nil
}

// appendNoise encrypts one message into a frame appended to b, in place. Called by the writer only.
func (o *outbox) appendNoise(b []byte, typ uint32, payload []byte) ([]byte, error) {
	if len(payload) > MaxMessageSize {
		return b, fmt.Errorf("message of %d bytes is too large", len(payload))
	}
	n := 4 + len(payload) + noiseTagSize
	b = slices.Grow(b, 3+n) // room for the tag too, so Seal encrypts within b
	b = binary.BigEndian.AppendUint16(append(b, noiseIndicator), uint16(n))
	off := len(b)
	b = binary.BigEndian.AppendUint16(b, uint16(typ))
	b = binary.BigEndian.AppendUint16(b, uint16(len(payload)))
	b = append(b, payload...)
	ct := o.cipher.seal(b[off:off], nil, b[off:])
	return b[:off+len(ct)], nil
}

// noiseMAC formats a MAC as ESPHome's server hello carries it: lower case hex without separators.
func noiseMAC(mac string) string {
	b := []byte(mac)
	out := b[:0]
	for _, ch := range b {
		switch {
		case ch >= 'A' && ch <= 'F':
			out = append(out, ch+'a'-'A')
		case ch >= 'a' && ch <= 'f', ch >= '0' && ch <= '9':
			out = append(out, ch)
		}
	}
	return string(out)
}
