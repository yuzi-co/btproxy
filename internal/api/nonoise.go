//go:build nonoise

package api

import (
	"bufio"
	"errors"
)

// Built with the nonoise tag: no encrypted API. Leaving out Noise, ChaCha20-Poly1305 and X25519 makes the
// mipsle binary about 260 kB smaller. A client that asks for encryption is told it is not configured, as by a
// default build without a key.
const (
	NoiseProtocol = "Noise_NNpsk0_25519_ChaChaPoly_SHA256"
	Encryption    = false
)

var errNoEncryption = errors.New("encryption is not built in (nonoise build tag)")

type noiseCipher struct{}

func (c *client) noiseHandshake(*bufio.Reader) error { return errNoEncryption }

func (c *client) readNoise(*bufio.Reader) (Frame, error) { return Frame{}, errNoEncryption }

func (c *client) appendNoise(b []byte, _ uint32, _ []byte) ([]byte, error) { return b, errNoEncryption }
