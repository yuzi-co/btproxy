package winrt

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/saltosystems/winrt-go"
	"github.com/saltosystems/winrt-go/windows/foundation"
	"github.com/saltosystems/winrt-go/windows/storage/streams"
	"golang.org/x/sys/windows"

	"github.com/yuzi-co/btproxy/internal/ble"
)

var comOnce sync.Once

// initCOM puts the process in the multithreaded apartment. A thread that holds the MTA open for good lets every
// other thread, including the ones Go schedules goroutines on and the WinRT threads that deliver events, use
// WinRT objects through the implicit MTA without initializing COM itself.
func initCOM() {
	comOnce.Do(func() {
		ready := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			// S_FALSE (already initialized) is reported as an error by go-ole; either way the MTA is up.
			_ = ole.RoInitialize(1)
			close(ready)
			select {}
		}()
		<-ready
	})
}

// await waits for an IAsyncOperation and returns its result, which the caller owns. The operation is released.
// resultSig is the WinRT signature of the result type, which the completion handler's IID is derived from.
func await(op *foundation.IAsyncOperation, resultSig string, timeout time.Duration) (unsafe.Pointer, error) {
	defer op.Release()
	iid := winrt.ParameterizedInstanceGUID(foundation.GUIDAsyncOperationCompletedHandler, resultSig)
	done := make(chan foundation.AsyncStatus, 1)
	h := foundation.NewAsyncOperationCompletedHandler(ole.NewGUID(iid),
		func(_ *foundation.AsyncOperationCompletedHandler, _ *foundation.IAsyncOperation, st foundation.AsyncStatus) {
			select {
			case done <- st:
			default:
			}
		})
	defer h.Release()
	if err := op.SetCompleted(h); err != nil {
		return nil, err
	}
	var st foundation.AsyncStatus
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case st = <-done:
	case <-t.C:
		withAsyncInfo(op, func(ai *foundation.IAsyncInfo) { ai.Cancel() })
		select { // a cancelled operation completes with AsyncStatusCanceled, normally at once
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return nil, errTimeout
	}
	if st != foundation.AsyncStatusCompleted {
		var err error = fmt.Errorf("async operation ended with status %d", st)
		withAsyncInfo(op, func(ai *foundation.IAsyncInfo) {
			if hr, e := ai.GetErrorCode(); e == nil && hr.Value != 0 {
				err = hresultError(uint32(hr.Value))
			}
		})
		return nil, err
	}
	return op.GetResults()
}

var errTimeout = errors.New("timed out")

func withAsyncInfo(op *foundation.IAsyncOperation, fn func(*foundation.IAsyncInfo)) {
	var ai *foundation.IAsyncInfo
	hr, _, _ := syscall.SyscallN(op.VTable().QueryInterface, uintptr(unsafe.Pointer(op)),
		uintptr(unsafe.Pointer(ole.NewGUID(foundation.GUIDIAsyncInfo))), uintptr(unsafe.Pointer(&ai)))
	if hr != 0 || ai == nil {
		return
	}
	defer ai.Release()
	fn(ai)
}

// hresultError describes a failed HRESULT; an ATT error (FACILITY_BLUETOOTH_ATT) keeps its ATT code.
func hresultError(hr uint32) error {
	facility, code := hr>>16&0x1fff, hr&0xffff
	if facility == 0x65 {
		return fmt.Errorf("ATT error 0x%02x", code)
	}
	msgID := hr
	if facility == 7 { // FACILITY_WIN32
		msgID = code
	}
	buf := make([]uint16, 512)
	n, err := windows.FormatMessage(windows.FORMAT_MESSAGE_FROM_SYSTEM|windows.FORMAT_MESSAGE_IGNORE_INSERTS,
		0, msgID, 0, buf, nil)
	if err == nil && n > 0 {
		return hresult{hr, fmt.Sprintf("0x%08x: %s", hr, strings.TrimRight(windows.UTF16ToString(buf[:n]), " .\r\n"))}
	}
	return hresult{hr, fmt.Sprintf("HRESULT 0x%08x", hr)}
}

// hresult is a failed HRESULT with its system message, so callers can tell errors apart by code.
type hresult struct {
	code uint32
	msg  string
}

func (h hresult) Error() string { return h.msg }

// eventHandler makes a TypedEventHandler<sender, args> for an event; the signatures give its IID.
func eventHandler(senderSig, argsSig string, fn func(sender, args unsafe.Pointer)) *foundation.TypedEventHandler {
	iid := winrt.ParameterizedInstanceGUID(foundation.GUIDTypedEventHandler, senderSig, argsSig)
	return foundation.NewTypedEventHandler(ole.NewGUID(iid), func(_ *foundation.TypedEventHandler, s, a unsafe.Pointer) {
		fn(s, a)
	})
}

// inspectableSig is the signature of an event argument typed as object.
const inspectableSig = "cinterface(IInspectable)"

// IBufferByteAccess exposes the memory behind an IBuffer without copying it through a DataReader.
var iidBufferByteAccess = ole.NewGUID("905a0fef-bc53-11df-8c49-001e4fc686da")

// appendBuffer appends the contents of b to dst. b stays owned by the caller.
func appendBuffer(dst []byte, b *streams.IBuffer) ([]byte, error) {
	n, err := b.GetLength()
	if err != nil || n == 0 {
		return dst, err
	}
	var acc *ole.IUnknown
	hr, _, _ := syscall.SyscallN(b.VTable().QueryInterface, uintptr(unsafe.Pointer(b)),
		uintptr(unsafe.Pointer(iidBufferByteAccess)), uintptr(unsafe.Pointer(&acc)))
	if hr == 0 && acc != nil {
		defer acc.Release()
		vt := (*[4]uintptr)(unsafe.Pointer(acc.RawVTable))
		var p *byte
		hr, _, _ = syscall.SyscallN(vt[3], uintptr(unsafe.Pointer(acc)), uintptr(unsafe.Pointer(&p)))
		if hr == 0 && p != nil {
			return append(dst, unsafe.Slice(p, n)...), nil
		}
	}
	r, err := streams.DataReaderFromBuffer(b)
	if err != nil {
		return dst, err
	}
	defer r.Release()
	data, err := r.ReadBytes(n)
	return append(dst, data...), err
}

// newBuffer copies data into a new IBuffer, which the caller releases.
func newBuffer(data []byte) (*streams.IBuffer, error) {
	w, err := streams.NewDataWriter()
	if err != nil {
		return nil, err
	}
	defer w.Release()
	if len(data) > 0 {
		if err := w.WriteBytes(uint32(len(data)), data); err != nil {
			return nil, err
		}
	}
	return w.DetachBuffer()
}

// uuidOf converts a WinRT GUID to the ESPHome form.
func uuidOf(g syscall.GUID) ble.UUID {
	hi := uint64(g.Data1)<<32 | uint64(g.Data2)<<16 | uint64(g.Data3)
	var lo uint64
	for _, b := range g.Data4 {
		lo = lo<<8 | uint64(b)
	}
	return ble.UUID{Hi: hi, Lo: lo}
}

// protocolError reads the ATT error code from a GATT result's ProtocolError reference.
func protocolError(ref *foundation.IReference, err error) error {
	if err != nil || ref == nil {
		return errors.New("GATT protocol error")
	}
	defer ref.Release()
	v, err := ref.GetValue() // IReference<UInt8>: the byte comes back in the pointer-sized out slot
	if err != nil {
		return errors.New("GATT protocol error")
	}
	return fmt.Errorf("ATT error 0x%02x", byte(uintptr(v)))
}
