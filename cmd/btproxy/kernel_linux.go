package main

import (
	"flag"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/kernel"
	"github.com/yuzi-co/btproxy/internal/logx"
)

var bondDir = flag.String("bond-dir", "/etc/btproxy", "directory for the keys of bonded devices (kernel backend)")

func init() {
	backends["kernel"] = func(name string, log *logx.Logger) (ble.Adapter, error) {
		return kernel.Open(name, kernel.Options{BondDir: *bondDir, StallTimeout: stallTimeout, USBReset: usbReset,
			PauseOnConnect: scanPause, ConnectRetries: connectRetries, Log: log})
	}
}
