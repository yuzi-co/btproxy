package main

import (
	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/kernel"
	"github.com/yuzi-co/btproxy/internal/logx"
)

func init() {
	backends["kernel"] = func(name string, log *logx.Logger) (ble.Adapter, error) {
		return kernel.Open(name, kernel.Options{BondDir: bondDir,
			WatchdogOptions: ble.WatchdogOptions{StallTimeout: stallTimeout, USBReset: usbReset},
			ConnectOptions: ble.ConnectOptions{PauseOnConnect: scanPause, ConnectRetries: connectRetries,
				ConnectRetryDelay: connectRetryDelay},
			Log: log})
	}
	autoBackend = func(log *logx.Logger) string {
		running, err := kernel.StackRunning()
		_, haveBluez := backends["bluez"]
		backend, why := chooseAuto(running, err, haveBluez)
		kv := []any{"backend", backend, "reason", why}
		if err != nil {
			kv = append(kv, "err", err)
		}
		log.Info("backend chosen", kv...)
		return backend
	}
}

// chooseAuto picks the backend for -backend auto: the kernel backend, unless bluetoothd has the controller or the
// kernel cannot be asked, as for a program that reaches BlueZ over D-Bus without CAP_NET_RAW.
func chooseAuto(running bool, err error, haveBluez bool) (backend, why string) {
	switch {
	case !haveBluez:
		return "kernel", "this build has no BlueZ"
	case running:
		return "bluez", "bluetoothd is running"
	case err != nil:
		return "bluez", "cannot read the HCI monitor, which the kernel backend needs"
	}
	return "kernel", "bluetoothd is not running"
}
