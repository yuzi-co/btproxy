package main

import (
	"btproxy/internal/ble"
	"btproxy/internal/ble/winrt"
	"btproxy/internal/logx"
)

func init() {
	defaultBackend = "winrt"
	backends["winrt"] = func(_ string, log *logx.Logger) (ble.Adapter, error) {
		return winrt.Open(log)
	}
}
