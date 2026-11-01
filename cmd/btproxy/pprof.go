//go:build pprof

package main

import (
	"net/http"
	_ "net/http/pprof"
)

// Built with -tags pprof, the proxy serves Go's profiler on localhost:6060, for measuring it on the target.
func init() {
	go http.ListenAndServe("127.0.0.1:6060", nil)
}
