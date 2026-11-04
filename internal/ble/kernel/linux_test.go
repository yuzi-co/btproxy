//go:build linux

package kernel

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// A link that drops in its first seconds reports ENOTCONN after as long as it was up, and is worth retrying however
// long that took. HCI reason 0x3e has no errno of its own and reports ENOSYS within half a second.
func TestClassifyConnect(t *testing.T) {
	if err := classifyConnect(unix.ENOTCONN); !errors.Is(err, errLinkLost) {
		t.Errorf("ENOTCONN: %v", err)
	}
	var perm permanentError
	if err := classifyConnect(unix.EPERM); !errors.As(err, &perm) {
		t.Errorf("EPERM: %v", err)
	}
	if err := classifyConnect(unix.ETIMEDOUT); errors.Is(err, errLinkLost) || errors.Is(err, errRadio) || errors.As(err, &perm) {
		t.Errorf("ETIMEDOUT: %v", err)
	}
}

func TestBdaddr(t *testing.T) {
	if got := bdaddr(0xf424a206a947); got != [6]byte{0xf4, 0x24, 0xa2, 0x06, 0xa9, 0x47} {
		t.Errorf("% x", got)
	}
}

func TestClassifyRadioFailure(t *testing.T) {
	if err := classifyConnect(unix.ENOSYS); !errors.Is(err, errRadio) {
		t.Errorf("ENOSYS: %v", err)
	}
}
