package ui

import "crypto/rand"

func randomBytes(b []byte) error {
	_, err := rand.Read(b)
	return err
}
