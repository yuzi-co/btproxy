//go:build !windows

package ui

import (
	"io"
	"os"
)

// randomBytes reads the kernel's random source. crypto/rand is not linked into the router build (it is
// built without encryption), and this costs nothing.
func randomBytes(b []byte) error {
	f, err := os.Open("/dev/urandom")
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.ReadFull(f, b)
	return err
}
