//go:build linux

package bluez

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/hci"
)

var _ ble.ConnParamsSetter = (*Adapter)(nil)

// SetConnParams implements ble.ConnParamsSetter. BlueZ has no call for it, so it goes to the controller directly.
func (a *Adapter) SetConnParams(addr uint64, p ble.ConnParams) error {
	dev, err := strconv.ParseUint(strings.TrimPrefix(string(a.path), "/org/bluez/hci"), 10, 16)
	if err != nil {
		return fmt.Errorf("adapter %s has no HCI device number", a.path)
	}
	return hci.ConnUpdate(uint16(dev), addr, p, a.log)
}

// resetUSBDevice is a variable so the tests can watch the step without a USB device.
var resetUSBDevice = hci.ResetUSB
