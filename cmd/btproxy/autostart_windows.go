package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// runKey is where Windows looks for programs to start at logon, per user.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// autostartValue is the value name under runKey.
const autostartValue = "btproxy"

// autostartCommand is the command the Run value holds: the quoted path of this executable. Flags are left
// out: the settings file carries the configuration.
func autostartCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return `"` + exe + `"`, nil
}

// autostartEnabled tells whether the Run value exists.
func autostartEnabled(value string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(value)
	return err == nil
}

// setAutostart writes the Run value with command, or deletes it. Deleting a missing value is no error.
func setAutostart(value string, on bool, command string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if on {
		return k.SetStringValue(value, command)
	}
	if err := k.DeleteValue(value); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return err
	}
	return nil
}
