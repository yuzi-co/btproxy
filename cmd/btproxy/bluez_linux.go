package main

import (
	"btproxy/internal/ble"
	"btproxy/internal/ble/bluez"
	"btproxy/internal/logx"
)

func init() {
	backends["bluez"] = func(name string, log *logx.Logger) (ble.Adapter, error) {
		raw, err := bluez.ParseRawMode(rawAdverts)
		if err != nil {
			return nil, err
		}
		a, err := bluez.Open(name, bluez.Options{StallTimeout: stallTimeout, RestartCmd: bluetoothdRestart,
			RawAdverts: raw, Log: log})
		if err != nil {
			return nil, err
		}
		if useAgent {
			if err := a.RegisterAgent(log); err != nil {
				log.Warn("pairing agent not registered; pairing that needs a confirmation will fail", "err", err)
			}
		}
		return a, nil
	}
}
