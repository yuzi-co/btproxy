package main

import (
	"log/slog"

	"btproxy/internal/ble"
	"btproxy/internal/ble/bluez"
)

func init() {
	backends["bluez"] = func(name string) (ble.Adapter, error) {
		a, err := bluez.Open(name, bluez.Options{StallTimeout: stallTimeout, RestartCmd: bluetoothdRestart})
		if err != nil {
			return nil, err
		}
		if useAgent {
			if err := a.RegisterAgent(slog.Default()); err != nil {
				slog.Warn("pairing agent not registered; pairing that needs a confirmation will fail", "err", err)
			}
		}
		return a, nil
	}
}
