//go:build !windows

package main

import (
	"io"
	"os"
	"syscall"

	"github.com/yuzi-co/btproxy/internal/logx"
	"github.com/yuzi-co/btproxy/internal/ui"
)

// The hooks main and the settings page call for the Windows tray, log file, single instance check and
// autostart (platform_windows.go). Elsewhere they do nothing and compile away.

var (
	defaultConfig = ""    // -config default
	defaultUI     = ""    // -ui default: off
	uiToken       = false // the settings page asks for a token without a password (Windows: from the tray)
)

func logOutput() io.Writer             { return os.Stderr }
func oneInstance(*logx.Logger, string) {}
func startTray(*logx.Logger)           {}
func platformFatal(string, []any)      {}
func beforeExit()                      {}
func autostartField() (ui.Field, bool) { return ui.Field{}, false }
func setAutostartFromUI(bool) error    { return nil }

// makeConfigDir does nothing: the settings file's directory (/etc) exists.
func makeConfigDir(string) error { return nil }

// protectFile does nothing: the file was created with mode 0600.
func protectFile(string) error { return nil }

// restartSelf replaces the process image with a fresh start of the same executable and arguments. The
// process ID stays, so procd or systemd do not notice; every descriptor is close-on-exec, so the API port,
// D-Bus and HCI sockets close with the old image.
func restartSelf(env []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, env)
}
