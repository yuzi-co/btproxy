//go:build !windows && nonoise

package ui

import (
	"io"
	"os"
)

// randomBytes reads the kernel's random source. Only a build without encryption links no crypto/rand
// otherwise, and reading /dev/urandom keeps about 130 KB off it (mipsle, 2026-11-07).
func randomBytes(b []byte) error {
	f, err := os.Open("/dev/urandom")
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.ReadFull(f, b)
	return err
}
