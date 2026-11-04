//go:build !nobluez

package main

import (
	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/bluez"
	"github.com/yuzi-co/btproxy/internal/logx"
)

// agentDone is set once an adapter has registered the pairing agent.
var agentDone bool

func init() {
	backends["bluez"] = func(name string, log *logx.Logger) (ble.Adapter, error) {
		raw, err := bluez.ParseRawMode(rawAdverts)
		if err != nil {
			return nil, err
		}
		a, err := bluez.Open(name, bluez.Options{StallTimeout: stallTimeout, RestartCmd: bluetoothdRestart,
			USBReset: usbReset, RawAdverts: raw, PauseOnConnect: scanPause, ConnectRetries: connectRetries, Log: log})
		if err != nil {
			return nil, err
		}
		if useAgent && !agentDone { // one agent serves every adapter (see bluez.RegisterAgent)
			agentDone = true
			if err := a.RegisterAgent(log); err != nil {
				log.Warn("pairing agent not registered; pairing that needs a confirmation will fail", "err", err)
			}
		}
		return a, nil
	}
}
