package main

import (
	"btproxy/internal/ble"
	"btproxy/internal/ble/bluez"
)

func init() {
	backends["bluez"] = func(name string) (ble.Adapter, error) { return bluez.Open(name) }
}
