package main

//go:generate go run gen_icon.go

import (
	_ "embed"
	"encoding/binary"
	"runtime"
	"sync"
	"unsafe"

	"github.com/yuzi-co/btproxy/internal/logx"

	"golang.org/x/sys/windows"
)

// The tray icon is plain Win32: a hidden window receives the icon's mouse messages and shows a popup menu.

//go:embed btproxy.ico
var iconData []byte

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	shell32                 = windows.NewLazySystemDLL("shell32.dll")
	pRegisterClassEx        = user32.NewProc("RegisterClassExW")
	pCreateWindowEx         = user32.NewProc("CreateWindowExW")
	pDefWindowProc          = user32.NewProc("DefWindowProcW")
	pGetMessage             = user32.NewProc("GetMessageW")
	pTranslateMessage       = user32.NewProc("TranslateMessage")
	pDispatchMessage        = user32.NewProc("DispatchMessageW")
	pPostMessage            = user32.NewProc("PostMessageW")
	pCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	pAppendMenu             = user32.NewProc("AppendMenuW")
	pTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	pDestroyMenu            = user32.NewProc("DestroyMenu")
	pSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	pGetCursorPos           = user32.NewProc("GetCursorPos")
	pRegisterWindowMessage  = user32.NewProc("RegisterWindowMessageW")
	pCreateIconFromResource = user32.NewProc("CreateIconFromResourceEx")
	pGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	pShellNotifyIcon        = shell32.NewProc("Shell_NotifyIconW")
)

const (
	wmNull            = 0x0000
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	wmLButtonUp       = 0x0202
	wmRButtonUp       = 0x0205
	wmApp             = 0x8000
	wmTray            = wmApp + 1

	nimAdd     = 0
	nimModify  = 1
	nimDelete  = 2
	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4

	mfString    = 0x0
	mfGrayed    = 0x1
	mfChecked   = 0x8
	mfSeparator = 0x800

	tpmRightButton = 0x2
	tpmNoNotify    = 0x80
	tpmReturnCmd   = 0x100

	smCxSmIcon = 49
)

// Menu item ids.
const (
	idStatus = iota + 1
	idAutostart
	idSettings
	idOpenLog
	idQuit
)

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type notifyIconData struct {
	size            uint32
	wnd             uintptr
	id              uint32
	flags           uint32
	callbackMessage uint32
	icon            uintptr
	tip             [128]uint16
	state           uint32
	stateMask       uint32
	info            [256]uint16
	version         uint32
	infoTitle       [64]uint16
	infoFlags       uint32
	guidItem        windows.GUID
	balloonIcon     uintptr
}

type point struct{ x, y int32 }

type msg struct {
	wnd     uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
	private uint32
}

// tray is the icon's state; mu guards nid against removeTrayIcon from other goroutines.
var tray struct {
	mu             sync.Mutex
	nid            notifyIconData
	added          bool
	taskbarCreated uint32
	app            *app
	log            *logx.Logger
}

// runTray adds the icon and runs its window's message loop; it does not return.
func runTray(a *app) {
	runtime.LockOSThread()
	tray.app, tray.log = a, a.log
	log := a.log
	var inst windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &inst); err != nil {
		log.Error("tray", "err", err)
		return
	}
	cls := windows.StringToUTF16Ptr("btproxyTray")
	wc := wndClassEx{wndProc: windows.NewCallback(trayWndProc), instance: inst, className: cls}
	wc.size = uint32(unsafe.Sizeof(wc))
	if r, _, err := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		log.Error("tray: register window class", "err", err)
		return
	}
	// A hidden top-level window rather than a message-only one: the popup menu needs a window that can come
	// to the foreground, or it does not close when clicking elsewhere.
	wnd, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(cls)), 0,
		0, 0, 0, 0, 0, 0, uintptr(inst), 0)
	if wnd == 0 {
		log.Error("tray: create window", "err", err)
		return
	}
	tray.taskbarCreated = uint32(callUTF16(pRegisterWindowMessage, "TaskbarCreated"))

	tray.mu.Lock()
	tray.nid = notifyIconData{wnd: wnd, id: 1, flags: nifMessage | nifIcon | nifTip, callbackMessage: wmTray,
		icon: loadIcon()}
	tray.nid.size = uint32(unsafe.Sizeof(tray.nid))
	copy(tray.nid.tip[:len(tray.nid.tip)-1], windows.StringToUTF16(noNUL(tooltip())))
	tray.mu.Unlock()
	addIcon()

	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func addIcon() {
	tray.mu.Lock()
	defer tray.mu.Unlock()
	if r, _, err := pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&tray.nid))); r == 0 {
		tray.log.Warn("tray: add icon", "err", err)
		return
	}
	tray.added = true
	tray.log.Debug("tray icon added", "hwnd", tray.nid.wnd)
}

// removeTrayIcon takes the icon away before the process exits, so it does not linger until hovered.
func removeTrayIcon() {
	tray.mu.Lock()
	defer tray.mu.Unlock()
	if tray.added {
		pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&tray.nid)))
		tray.added = false
	}
}

// tooltip names the node and its API port.
func tooltip() string {
	_, nodes := tray.app.status()
	if len(nodes) == 0 {
		return "btproxy"
	}
	t := nodes[0].name
	for i, n := range nodes {
		if i > 0 {
			t += ","
		}
		_, port, _ := splitPort(n.addr)
		t += " port " + port
	}
	if len(t) > 127 {
		t = t[:127]
	}
	return t
}

func splitPort(addr string) (host, port string, ok bool) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], true
		}
	}
	return addr, "", false
}

func trayWndProc(wnd, message, wParam, lParam uintptr) uintptr {
	switch {
	case message == wmTray && (lParam&0xffff == wmRButtonUp || lParam&0xffff == wmLButtonUp):
		showMenu(wnd)
		return 0
	case uint32(message) == tray.taskbarCreated && tray.taskbarCreated != 0:
		addIcon() // Explorer restarted
		return 0
	case message == wmQueryEndSession:
		return 1
	case message == wmEndSession && wParam != 0:
		tray.app.stop("Windows is shutting down", false)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(wnd, message, wParam, lParam)
	return r
}

func showMenu(wnd uintptr) {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)
	st, nodes := tray.app.status()
	appendMenu(menu, mfString|mfGrayed, idStatus, statusText(st, len(nodes)))
	appendMenu(menu, mfSeparator, 0, "")
	auto := uint32(mfString)
	if autostartEnabled(autostartValue) {
		auto |= mfChecked
	}
	appendMenu(menu, auto, idAutostart, "Start with Windows")
	if tray.app.settingsURL() != "" {
		appendMenu(menu, mfString, idSettings, "Settings...")
	}
	if tray.app.logPath != "" {
		appendMenu(menu, mfString, idOpenLog, "Open log")
	}
	appendMenu(menu, mfSeparator, 0, "")
	appendMenu(menu, mfString, idQuit, "Quit")

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(wnd)
	cmd, _, _ := pTrackPopupMenu.Call(menu, tpmReturnCmd|tpmNoNotify|tpmRightButton,
		uintptr(pt.x), uintptr(pt.y), 0, wnd, 0)
	pPostMessage.Call(wnd, wmNull, 0, 0) // so the menu closes properly next time (KB135788)

	switch cmd {
	case idAutostart:
		if err := setAutostartFromUI(!autostartEnabled(autostartValue)); err != nil {
			tray.log.Error("autostart", "err", err)
			messageBox("Start with Windows: "+err.Error(), true)
		}
	case idSettings:
		shellOpen(tray.app.settingsURL())
	case idOpenLog:
		shellOpen(tray.app.logPath)
	case idQuit:
		tray.app.stop("quit from the tray", false)
	}
}

func appendMenu(menu uintptr, flags uint32, id int, text string) {
	var p *uint16
	if text != "" {
		p = windows.StringToUTF16Ptr(noNUL(text))
	}
	pAppendMenu.Call(menu, uintptr(flags), uintptr(id), uintptr(unsafe.Pointer(p)))
}

// shellOpen opens a file or URL with its default program.
func shellOpen(target string) {
	err := windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(target), nil, nil,
		windows.SW_SHOWNORMAL)
	if err != nil {
		tray.log.Warn("open", "target", target, "err", err)
	}
}

func callUTF16(p *windows.LazyProc, s string) uintptr {
	r, _, _ := p.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(s))))
	return r
}

// loadIcon makes an icon of the small icon size from the embedded .ico, picking the smallest image at
// least that large.
func loadIcon() uintptr {
	want, _, _ := pGetSystemMetrics.Call(smCxSmIcon)
	if want == 0 {
		want = 16
	}
	img, ok := pickIcon(iconData, int(want))
	if !ok {
		return 0
	}
	h, _, _ := pCreateIconFromResource.Call(uintptr(unsafe.Pointer(&img[0])), uintptr(len(img)), 1, 0x00030000,
		want, want, 0)
	return h
}

// pickIcon returns the image data of the .ico entry closest to size: the smallest at least that large, else
// the largest.
func pickIcon(ico []byte, size int) ([]byte, bool) {
	if len(ico) < 6 {
		return nil, false
	}
	n := int(binary.LittleEndian.Uint16(ico[4:]))
	best, bestSize := -1, 0
	for i := 0; i < n; i++ {
		e := 6 + 16*i
		if e+16 > len(ico) {
			return nil, false
		}
		w := int(ico[e])
		if w == 0 {
			w = 256
		}
		better := best < 0 ||
			w >= size && (bestSize < size || w < bestSize) ||
			w < size && bestSize < size && w > bestSize
		if better {
			best, bestSize = i, w
		}
	}
	if best < 0 {
		return nil, false
	}
	e := 6 + 16*best
	ln := int(binary.LittleEndian.Uint32(ico[e+8:]))
	off := int(binary.LittleEndian.Uint32(ico[e+12:]))
	if off < 0 || ln <= 0 || off+ln > len(ico) {
		return nil, false
	}
	return ico[off : off+ln], true
}
