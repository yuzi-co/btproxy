//go:build nobluez

package main

// A build with -tags nobluez leaves BlueZ and D-Bus out, for routers short of flash. The kernel backend is then the
// default, since it is the only one that can work.
func init() { defaultBackend = "kernel" }
