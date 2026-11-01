//go:build !windows

package main

func autostartFormValue() string { return "" }

// testChildArgs go to every proxy a test starts.
var testChildArgs []string
