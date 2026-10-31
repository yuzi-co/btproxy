//go:build !linux && !windows

package mdns

import (
	"errors"
	"syscall"
)

var errUnsupported = errors.New("mdns: not supported on this OS")

func reuse(_, _ string, _ syscall.RawConn) error { return errUnsupported }
func setup(uintptr) error                        { return errUnsupported }
func join(uintptr, iface) error                  { return errUnsupported }
func setIf(uintptr, iface) error                 { return errUnsupported }
