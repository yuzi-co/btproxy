//go:build linux

package hci

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUSBDevicePath(t *testing.T) {
	sys := t.TempDir()
	dev := filepath.Join(sys, "devices", "platform", "usb2", "2-1")
	iface := filepath.Join(dev, "2-1:1.0")
	if err := os.MkdirAll(filepath.Join(iface, "bluetooth", "hci0"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dev, "busnum"), []byte("2\n"), 0o644)
	os.WriteFile(filepath.Join(dev, "devnum"), []byte("13\n"), 0o644)
	class := filepath.Join(sys, "class", "bluetooth", "hci0")
	if err := os.MkdirAll(class, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(iface, filepath.Join(class, "device")); err != nil {
		t.Fatal(err)
	}
	got, err := usbDevicePath(sys, "hci0")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/dev/bus/usb/002/013" {
		t.Errorf("path %q", got)
	}
	if _, err := usbDevicePath(sys, "hci1"); err == nil {
		t.Error("no error for a missing adapter")
	}

	// An adapter that is not on USB, such as one on a UART.
	uart := filepath.Join(sys, "devices", "serial0")
	os.MkdirAll(uart, 0o755)
	class = filepath.Join(sys, "class", "bluetooth", "hci2")
	os.MkdirAll(class, 0o755)
	os.Symlink(uart, filepath.Join(class, "device"))
	if _, err := usbDevicePath(sys, "hci2"); err == nil {
		t.Error("no error for an adapter that is not on USB")
	}
}

// _IO('U', 20) has a different direction bit on MIPS, which is what most routers run.
func TestUSBResetRequest(t *testing.T) {
	want := uintptr(0x5514)
	if strings.HasPrefix(runtime.GOARCH, "mips") || strings.HasPrefix(runtime.GOARCH, "ppc") {
		want = 0x20005514
	}
	if got := usbResetRequest(runtime.GOARCH); got != want {
		t.Errorf("request %#x, want %#x", got, want)
	}
	if got := usbResetRequest("mipsle"); got != 0x20005514 {
		t.Errorf("mipsle request %#x", got)
	}
	if got := usbResetRequest("arm64"); got != 0x5514 {
		t.Errorf("arm64 request %#x", got)
	}
}
