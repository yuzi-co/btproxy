//go:build linux

package bluez

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// resetUSBDevice is a variable so the tests can watch the step without a USB device.
var resetUSBDevice = usbReset

// usbReset resets the USB device of an adapter. The kernel unbinds the driver and binds it again, which is like
// pulling the stick out and back in, for that device alone.
func usbReset(adapter string) error {
	path, err := usbDevicePath("/sys", adapter)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), usbResetRequest(runtime.GOARCH), 0); errno != 0 {
		return fmt.Errorf("USBDEVFS_RESET on %s: %w", path, errno)
	}
	return nil
}

// usbDevicePath finds the device node of an adapter: its sysfs device is a USB interface, whose parent has the
// bus and device numbers.
func usbDevicePath(sys, adapter string) (string, error) {
	iface, err := filepath.EvalSymlinks(filepath.Join(sys, "class", "bluetooth", adapter, "device"))
	if err != nil {
		return "", err
	}
	number := func(name string) (int, error) {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(iface), name))
		if err != nil {
			return 0, fmt.Errorf("%s is not on USB: %w", adapter, err)
		}
		return strconv.Atoi(strings.TrimSpace(string(b)))
	}
	bus, err := number("busnum")
	if err != nil {
		return "", err
	}
	dev, err := number("devnum")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, dev), nil
}

// usbResetRequest is USBDEVFS_RESET, _IO('U', 20). MIPS and PowerPC encode "no data" as a direction bit.
func usbResetRequest(goarch string) uintptr {
	if strings.HasPrefix(goarch, "mips") || strings.HasPrefix(goarch, "ppc") {
		return 0x20005514
	}
	return 0x5514
}
