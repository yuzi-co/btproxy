//go:build linux

package bluez

import (
	"testing"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

func TestSetConnParamsNoHCINumber(t *testing.T) {
	a := &Adapter{path: "/org/bluez/usb0", log: logx.Discard()}
	if err := a.SetConnParams(1, ble.ConnParams{}); err == nil {
		t.Error("no error for an adapter without an HCI number")
	}
}
