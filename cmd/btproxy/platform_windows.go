package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
	"github.com/yuzi-co/btproxy/internal/ui"

	"golang.org/x/sys/windows"
)

// On Windows btproxy can run without a console (built with -H windowsgui, started at logon): it then logs to
// a file and shows a tray icon whose menu opens the settings page.

var (
	trayOn  bool
	logFile string

	defaultConfig = filepath.Join(dataDir(), "config.json")
	defaultUI     = "127.0.0.1:0" // a free port, opened from the tray
	uiToken       = true
)

// envWaitPID names the process a restarted btproxy waits for before it takes the port.
const envWaitPID = "BTPROXY_WAIT_PID"

var kernel32GetConsoleWindow = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")

// hasConsole tells whether the process has a console window: not when built with -H windowsgui and started
// from Explorer, the Run key or a shortcut.
func hasConsole() bool {
	h, _, _ := kernel32GetConsoleWindow.Call()
	return h != 0
}

func init() {
	flag.BoolVar(&trayOn, "tray", !hasConsole(), "show a tray icon (Windows; default: when there is no console)")
	flag.StringVar(&logFile, "log-file", "",
		`log file (Windows; default: %LOCALAPPDATA%\btproxy\btproxy.log when there is no console to log to)`)
}

// dataDir is %LOCALAPPDATA%\btproxy, where the settings and the log file live.
func dataDir() string {
	d := os.Getenv("LOCALAPPDATA")
	if d == "" {
		d, _ = os.UserConfigDir()
	}
	return filepath.Join(d, "btproxy")
}

// makeConfigDir creates %LOCALAPPDATA%\btproxy or wherever the settings file goes.
func makeConfigDir(path string) error { return os.MkdirAll(filepath.Dir(path), 0o755) }

// logOutput is stderr, plus or instead the log file: with -log-file, or by default when stderr goes nowhere.
func logOutput() io.Writer {
	stderr := stdHandleOK(windows.STD_ERROR_HANDLE)
	var w io.Writer
	if stderr {
		w = os.Stderr
	}
	path := logFile
	if path == "" && !stderr {
		path = filepath.Join(dataDir(), "btproxy.log")
	}
	if path == "" {
		return w
	}
	var onOpen func(*os.File)
	if !stderr {
		// A crash's stack trace goes to the process's stderr handle: make that the log file too.
		onOpen = func(f *os.File) { windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd())) }
	}
	f, err := openRotating(path, logMaxSize, onOpen)
	switch {
	case err != nil && stderr:
		fmt.Fprintln(os.Stderr, "log file:", err)
	case err != nil:
	case w != nil:
		app.logPath = path
		w = io.MultiWriter(w, f)
	default:
		app.logPath = path
		w = f
	}
	return w
}

// stdHandleOK tells whether a standard handle leads somewhere: a console, file or pipe.
func stdHandleOK(which uint32) bool {
	h, err := windows.GetStdHandle(which)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		return false
	}
	t, err := windows.GetFileType(h)
	return err == nil && t != windows.FILE_TYPE_UNKNOWN
}

// oneInstance makes sure only one btproxy serves a listen address: a second one started at logon or by hand
// would fail on the port anyway, so it says so and exits. After a restart from the settings page it first
// waits for the old process to exit.
func oneInstance(log *logx.Logger, listen string) {
	if s := os.Getenv(envWaitPID); s != "" {
		os.Unsetenv(envWaitPID)
		if pid, err := strconv.Atoi(s); err == nil {
			waitExit(uint32(pid), 20*time.Second)
		}
	}
	name := `Local\btproxy ` + strings.ReplaceAll(listen, `\`, "/")
	_, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(name))
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		msg := "btproxy is already running (listen " + listen + ")"
		log.Error(msg)
		if trayOn {
			messageBox(msg, false)
		}
		os.Exit(1)
	} else if err != nil {
		log.Warn("single instance check", "err", err)
	}
	// The mutex handle stays open until the process exits.
}

// waitExit waits up to timeout for a process to exit.
func waitExit(pid uint32, timeout time.Duration) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return // gone already
	}
	defer windows.CloseHandle(h)
	windows.WaitForSingleObject(h, uint32(timeout/time.Millisecond))
}

func startTray(log *logx.Logger) {
	if !trayOn {
		if u := settingsURL(); u != "" {
			log.Info("settings page", "url", u)
		}
		return
	}
	go runTray(log)
}

func beforeExit() { removeTrayIcon() }

// restartSelf starts the same executable with the same arguments and returns; the caller exits. The new
// process waits for this one to exit before it takes the port and the single instance mutex.
func restartSelf(env []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	env = append(env, envWaitPID+"="+strconv.Itoa(os.Getpid()))
	p, err := os.StartProcess(exe, append([]string{exe}, os.Args[1:]...), &os.ProcAttr{
		Env: env, Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return err
	}
	return p.Release()
}

// platformFatal shows why btproxy stops when nobody would see the log otherwise.
func platformFatal(msg string, args []any) {
	if !trayOn {
		return
	}
	removeTrayIcon()
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, "\n%v: %v", args[i], args[i+1])
	}
	messageBox("btproxy stopped: "+b.String(), true)
}

func messageBox(text string, isError bool) {
	flags := uint32(windows.MB_OK | windows.MB_SETFOREGROUND)
	if isError {
		flags |= windows.MB_ICONERROR
	} else {
		flags |= windows.MB_ICONINFORMATION
	}
	windows.MessageBox(0, windows.StringToUTF16Ptr(text), windows.StringToUTF16Ptr("btproxy"), flags)
}

func autostartField() (ui.Field, bool) {
	v := "false"
	if autostartEnabled(autostartValue) {
		v = "true"
	}
	return ui.Field{Name: "autostart", Label: "Start with Windows", Kind: ui.Bool, Value: v,
		Help: `Runs btproxy at logon (HKCU\` + runKey + `). It then reads this page's settings file.`}, true
}

func setAutostartFromUI(on bool) error {
	if on == autostartEnabled(autostartValue) {
		return nil
	}
	cmd, err := autostartCommand()
	if err != nil {
		return err
	}
	return setAutostart(autostartValue, on, cmd)
}
