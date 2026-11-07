package winrt

import (
	"cmp"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/saltosystems/winrt-go/windows/foundation"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/winrt/gen/windows/devices/bluetooth"
	"github.com/yuzi-co/btproxy/internal/ble/winrt/gen/windows/devices/bluetooth/advertisement"
	"github.com/yuzi-co/btproxy/internal/logx"
)

const (
	callTimeout     = 30 * time.Second // one WinRT operation (lookup, discovery, read, write)
	connectTimeout  = 30 * time.Second // until Windows reports the link up
	pairTimeout     = 60 * time.Second
	mtuSettleWindow = time.Second // how long a new connection may take to finish its MTU exchange
)

// timings are the scanner's waits, which the tests shorten; a zero field takes the default.
type timings struct {
	scanRetry  time.Duration // after the watcher stops on its own, e.g. with the radio off
	scanCheck  time.Duration // how often a running watcher's status is checked
	pauseLimit time.Duration // bounds a pause for a connect, should it hang beyond its own timeouts
}

func (t timings) withDefaults() timings {
	return timings{
		scanRetry:  cmp.Or(t.scanRetry, 5*time.Second),
		scanCheck:  cmp.Or(t.scanCheck, 10*time.Second),
		pauseLimit: cmp.Or(t.pauseLimit, connectTimeout+callTimeout),
	}
}

// Options configures the adapter.
type Options struct {
	ble.ConnectOptions // see retry_windows.go and pause_windows.go
	// Log receives the adapter's log lines; nil discards them.
	Log *logx.Logger
}

// Adapter is the default Bluetooth radio of the machine.
type Adapter struct {
	log       *logx.Logger
	mac       uint64
	tm        timings
	connRetry ble.ConnectRetry
	presets   []preset // connection parameter presets, Windows 11 and later
	// newScanner starts a watcher; a field, so the tests can do without the radio.
	newScanner func(ble.ScanMode) (scanWatcher, error)

	scan  ble.ScanCell  // the mode and status reported
	pause ble.ScanPause // connects in progress, which pause scanning (see pause_windows.go)

	mu       sync.Mutex
	scanning bool                             // scanning is wanted
	fn       atomic.Pointer[func(ble.Advert)] // read without mu: Stop may wait for a running Received handler
	w        scanWatcher
	retry    *time.Timer // restarts a failed watcher
	check    *time.Timer // checks the running watcher's status
	conns    map[uint64]*conn
	uncached map[uint64]bool // devices whose next service discovery bypasses the Windows GATT cache
}

// scanWatcher is a started advertisement watcher.
type scanWatcher interface {
	close()
	// alive is false once the watcher has stopped or aborted on its own.
	alive() bool
}

// Open initializes WinRT and opens the default Bluetooth adapter.
func Open(opt Options) (*Adapter, error) {
	log := opt.Log
	if log == nil {
		log = logx.Discard()
	}
	initCOM()
	op, err := bluetooth.BluetoothAdapterGetDefaultAsync()
	if err != nil {
		return nil, err
	}
	p, err := await(op, bluetooth.SignatureBluetoothAdapter, callTimeout)
	if err != nil {
		return nil, fmt.Errorf("default Bluetooth adapter: %w", err)
	}
	if p == nil {
		return nil, errors.New("no Bluetooth adapter")
	}
	ad := (*bluetooth.BluetoothAdapter)(p)
	defer ad.Release()
	if ok, err := ad.GetIsLowEnergySupported(); err == nil && !ok {
		return nil, errors.New("the Bluetooth adapter does not support Bluetooth LE")
	}
	if ok, err := ad.GetIsCentralRoleSupported(); err == nil && !ok {
		return nil, errors.New("the Bluetooth adapter cannot connect to peripherals (no central role)")
	}
	mac, err := ad.GetBluetoothAddress()
	if err != nil {
		return nil, err
	}
	a := &Adapter{log: log, mac: mac, tm: timings{}.withDefaults(), connRetry: opt.Retry(retryFast),
		conns: map[uint64]*conn{}, uncached: map[uint64]bool{}}
	a.initScan(ble.ScanActive, opt.PauseOnConnect)
	if a.presets, err = loadPresets(); err != nil {
		log.Debug("no connection parameter presets (Windows 11 needed), not offering connection parameters", "err", err)
	} else {
		for _, p := range a.presets {
			log.Debug("connection parameter preset", "preset", p.name, "min", p.MinInterval, "max", p.MaxInterval,
				"latency", p.Latency, "timeout", p.Timeout)
		}
	}
	a.newScanner = func(m ble.ScanMode) (scanWatcher, error) {
		w, err := a.newWatcher(m)
		if err != nil {
			return nil, err
		}
		return w, nil
	}
	return a, nil
}

func (a *Adapter) MAC() uint64 { return a.mac }

// initScan sets up the scanner state with the mode and whether connects pause scanning; the tests call it on
// an Adapter without a radio.
func (a *Adapter) initScan(mode ble.ScanMode, pauseOnConnect bool) {
	a.scan.SetMode(mode)
	a.scan.Log = a.log
	a.pause = ble.ScanPause{Pause: pauseOnConnect, Limit: a.tm.pauseLimit, Kick: a.applyPause, Log: a.log}
}

// unlockPublish releases a.mu and then reports what changed under it to the status callback, which is not
// to be called holding a lock of the adapter's: every path that changes the status ends with it.
func (a *Adapter) unlockPublish() {
	a.mu.Unlock()
	a.scan.Publish()
}

// Scan starts an advertisement watcher. Advertisements and scan responses arrive as separate events, each
// delivered as its own Advert, as an ESP32 reports them.
func (a *Adapter) Scan(fn func(ble.Advert)) (func(), error) {
	a.mu.Lock()
	defer a.unlockPublish()
	if a.scanning {
		return nil, errors.New("already scanning")
	}
	a.scanning = true
	a.fn.Store(&fn)
	if a.pause.Paused() {
		a.scan.StoreState(ble.ScanIdle) // paused: the last connect to finish starts the watcher
	} else if err := a.startLocked(); err != nil {
		// Not an error of the call: the radio may be off while Home Assistant subscribes, and the API only
		// asks again when a client comes or goes. The start is retried like a watcher that stops later,
		// and the status reports the failure meanwhile.
		a.log.Warn("starting scan failed, retrying", "err", err, "in", a.tm.scanRetry)
		a.failedLocked()
	}
	a.scheduleCheckLocked()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.unlockPublish()
			a.scanning = false
			a.fn.Store(nil)
			a.stopLocked()
			if a.check != nil {
				a.check.Stop()
				a.check = nil
			}
			a.scan.StoreState(ble.ScanIdle)
		})
	}, nil
}

// scheduleCheckLocked arms the periodic check that a running watcher still runs. The Stopped event normally
// reports a watcher that stopped on its own, but a watcher can also end up Aborted without one.
func (a *Adapter) scheduleCheckLocked() {
	var t *time.Timer
	t = time.AfterFunc(a.tm.scanCheck, func() {
		a.mu.Lock()
		defer a.unlockPublish()
		if a.check != t || !a.scanning {
			return
		}
		if a.w != nil && !a.w.alive() {
			a.log.Warn("scanning stopped without notice, retrying", "in", a.tm.scanRetry)
			a.failedLocked()
		}
		a.scheduleCheckLocked()
	})
	a.check = t
}

func (a *Adapter) SetScanMode(m ble.ScanMode) {
	a.mu.Lock()
	defer a.unlockPublish()
	if m == a.scan.Mode() {
		return
	}
	a.scan.SetMode(m)
	if !a.scanning || a.w == nil {
		a.scan.StoreState(a.scan.Status().State)
		return
	}
	// The scanning mode of a started watcher cannot change: restart it.
	a.stopLocked()
	if err := a.startLocked(); err != nil {
		a.log.Warn("restarting scan in the new mode failed", "err", err)
		a.failedLocked()
	}
}

func (a *Adapter) ScanStatus() ble.ScanStatus { return a.scan.Status() }

func (a *Adapter) OnScanStatus(fn func(ble.ScanStatus)) { a.scan.OnStatus(fn) }

func (a *Adapter) startLocked() error {
	if a.retry != nil {
		a.retry.Stop()
		a.retry = nil
	}
	a.scan.StoreState(ble.ScanStarting)
	w, err := a.newScanner(a.scan.Mode())
	if err != nil {
		return err
	}
	a.w = w
	a.scan.StoreState(ble.ScanRunning)
	return nil
}

// stopLocked cancels a pending restart and lets go of the watcher, which is closed on the side: closing may
// wait for a running Received handler, and that handler may be waiting for the API server's lock, which the
// server holds when it stops the scan, or for a.mu, which a pause or a mode change holds here. A watcher is
// never closed under a.mu, then. The watcher's own events cannot reach a later watcher: the Stopped handler
// checks that its watcher is still a.w, and a.w is replaced before the close starts.
func (a *Adapter) stopLocked() {
	if a.retry != nil {
		a.retry.Stop()
		a.retry = nil
	}
	if w := a.w; w != nil {
		a.w = nil
		go w.close()
	}
}

// failedLocked reports the scanner failed and tries again later while scanning is still wanted.
func (a *Adapter) failedLocked() {
	a.stopLocked()
	a.scan.StoreState(ble.ScanFailed)
	var t *time.Timer
	t = time.AfterFunc(a.tm.scanRetry, func() {
		a.mu.Lock()
		defer a.unlockPublish()
		if a.retry != t || !a.scanning {
			return
		}
		a.retry = nil
		if a.pause.Paused() {
			a.scan.StoreState(ble.ScanIdle) // paused: the last connect to finish starts the watcher
			return
		}
		if err := a.startLocked(); err != nil {
			a.log.Debug("scan restart failed", "err", err)
			a.failedLocked()
		} else {
			a.log.Info("scanning again")
		}
	})
	a.retry = t
}

// watcher is one started BluetoothLEAdvertisementWatcher. A watcher's mode is fixed once it starts, so a mode
// change replaces it.
type watcher struct {
	w                *advertisement.BluetoothLEAdvertisementWatcher
	recv, stopped    *foundation.TypedEventHandler
	recvTok, stopTok foundation.EventRegistrationToken
	mu               sync.Mutex
	closed           bool
}

func (a *Adapter) newWatcher(mode ble.ScanMode) (*watcher, error) {
	ww, err := advertisement.NewBluetoothLEAdvertisementWatcher()
	if err != nil {
		return nil, err
	}
	w := &watcher{w: ww}
	sm := advertisement.BluetoothLEScanningModeActive
	if mode == ble.ScanPassive {
		sm = advertisement.BluetoothLEScanningModePassive
	}
	if err := ww.SetScanningMode(sm); err != nil {
		ww.Release()
		return nil, err
	}
	// Extended advertisements (Bluetooth 5) are left out unless asked for; the property is on
	// IBluetoothLEAdvertisementWatcher2, Windows 10 2004 and later.
	if itf, err := ww.QueryInterface(ole.NewGUID(advertisement.GUIDiBluetoothLEAdvertisementWatcher2)); err == nil {
		itf.Release()
		if err := ww.SetAllowExtendedAdvertisements(true); err != nil {
			a.log.Debug("extended advertisements not enabled", "err", err)
		}
	}
	w.recv = eventHandler(advertisement.SignatureBluetoothLEAdvertisementWatcher,
		advertisement.SignatureBluetoothLEAdvertisementReceivedEventArgs, func(_, args unsafe.Pointer) {
			a.received((*advertisement.BluetoothLEAdvertisementReceivedEventArgs)(args))
		})
	w.stopped = eventHandler(advertisement.SignatureBluetoothLEAdvertisementWatcher,
		advertisement.SignatureBluetoothLEAdvertisementWatcherStoppedEventArgs, func(_, args unsafe.Pointer) {
			code, _ := (*advertisement.BluetoothLEAdvertisementWatcherStoppedEventArgs)(args).GetError()
			a.watcherStopped(w, code)
		})
	if w.recvTok, err = ww.AddReceived(w.recv); err == nil {
		if w.stopTok, err = ww.AddStopped(w.stopped); err == nil {
			err = ww.Start()
		}
	}
	if err != nil {
		w.close()
		return nil, err
	}
	if st, err := ww.GetStatus(); err == nil && st == advertisement.BluetoothLEAdvertisementWatcherStatusAborted {
		w.close()
		return nil, errors.New("watcher aborted at start (Bluetooth off?)")
	}
	return w, nil
}

func (w *watcher) alive() bool {
	st, err := w.w.GetStatus()
	return err != nil || st != advertisement.BluetoothLEAdvertisementWatcherStatusAborted &&
		st != advertisement.BluetoothLEAdvertisementWatcherStatusStopped
}

func (w *watcher) close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()
	w.w.Stop()
	w.w.RemoveReceived(w.recvTok)
	w.w.RemoveStopped(w.stopTok)
	w.recv.Release()
	w.stopped.Release()
	w.w.Release()
}

// watcherStopped handles a watcher stopping on its own: the radio went off, or another app took it.
func (a *Adapter) watcherStopped(w *watcher, code bluetooth.BluetoothError) {
	w.mu.Lock()
	ours := w.closed
	w.mu.Unlock()
	if ours {
		return
	}
	go func() {
		a.mu.Lock()
		defer a.unlockPublish()
		if a.w != scanWatcher(w) {
			return
		}
		a.log.Warn("scanning stopped by Windows, retrying", "bluetooth_error", int(code), "in", a.tm.scanRetry)
		a.failedLocked()
	}()
}

// received turns one WinRT advertisement event into an Advert. It runs on a WinRT thread.
func (a *Adapter) received(args *advertisement.BluetoothLEAdvertisementReceivedEventArgs) {
	if a.fn.Load() == nil {
		return
	}
	addr, err := args.GetBluetoothAddress()
	if err != nil || addr == 0 {
		return // anonymous extended advertisements have no address
	}
	rssi, _ := args.GetRawSignalStrengthInDBm()
	at := ble.AddrPublic
	if t, err := args.GetBluetoothAddressType(); err == nil && t == bluetooth.BluetoothAddressTypeRandom {
		at = ble.AddrRandom
	}
	var buf [ble.MaxAdvert]byte
	data := buf[:0]
	if adv, err := args.GetAdvertisement(); err == nil && adv != nil {
		data = appendSections(data, adv)
		adv.Release()
	}
	a.deliver(ble.Advert{Addr: addr, AddrType: at, RSSI: int8(max(rssi, -128)), Data: data})
}

// deliver passes an advertisement on, unless it is empty. Windows reports a scan response as an entry of its own
// in active mode, and an empty one tells Home Assistant nothing: the device's advertisements arrive anyway.
func (a *Adapter) deliver(ad ble.Advert) {
	fn := a.fn.Load()
	if fn == nil || len(ad.Data) == 0 {
		return
	}
	(*fn)(ad)
}

// appendSections rebuilds the raw AD structures of an advertisement from its data sections, as they were on
// air: WinRT hands advertisements over parsed.
func appendSections(dst []byte, adv *advertisement.BluetoothLEAdvertisement) []byte {
	v, err := adv.GetDataSections()
	if err != nil || v == nil {
		return dst
	}
	defer v.Release()
	n, _ := v.GetSize()
	var scratch [ble.MaxAdvert]byte
	for i := range n {
		p, err := v.GetAt(i)
		if err != nil || p == nil {
			continue
		}
		s := (*advertisement.BluetoothLEAdvertisementDataSection)(p)
		typ, err1 := s.GetDataType()
		b, err2 := s.GetData()
		if err1 == nil && err2 == nil && b != nil {
			// Sections of an extended advertisement can be long; one that cannot fit is not copied.
			if n, err := b.GetLength(); err == nil && len(dst)+2+int(n) <= ble.MaxAdvert {
				if d, err := appendBuffer(scratch[:0], b); err == nil {
					dst = ble.AppendAD(dst, typ, d)
				}
			}
		}
		if b != nil {
			b.Release()
		}
		s.Release()
	}
	return dst
}

// ClearCache makes the next service discovery for addr read the services from the device instead of the
// Windows GATT cache, which also refreshes that cache. Bonds are kept.
func (a *Adapter) ClearCache(addr uint64) error {
	a.mu.Lock()
	a.uncached[addr] = true
	a.mu.Unlock()
	return nil
}

var (
	_ ble.Adapter     = (*Adapter)(nil)
	_ ble.ModeScanner = (*Adapter)(nil)
)
