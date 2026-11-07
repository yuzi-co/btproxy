//go:build windows || !nonoise

package ui

import "crypto/rand"

// randomBytes fills b from crypto/rand, which every build with encryption links anyway for the Noise handshake.
func randomBytes(b []byte) error {
	_, err := rand.Read(b)
	return err
}
