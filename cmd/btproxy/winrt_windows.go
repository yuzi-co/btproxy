package main

import (
	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/winrt"
	"github.com/yuzi-co/btproxy/internal/logx"
)

func init() {
	defaultBackend = "winrt"
	backends["winrt"] = func(_ string, log *logx.Logger) (ble.Adapter, error) {
		return winrt.Open(winrt.Options{ConnectOptions: ble.ConnectOptions{PauseOnConnect: scanPause,
			ConnectRetries: connectRetries, ConnectRetryDelay: connectRetryDelay}, Log: log})
	}
}
