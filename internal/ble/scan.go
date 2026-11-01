package ble

// ScanMode is how the scanner listens, with ESPHome's values.
type ScanMode uint32

const (
	ScanPassive ScanMode = 0 // listen only
	ScanActive  ScanMode = 1 // also ask for scan responses
)

// ScanState is the scanner's state, with ESPHome's values.
type ScanState uint32

const (
	ScanIdle ScanState = iota
	ScanStarting
	ScanRunning
	ScanFailed
	ScanStopping
	ScanStopped
)

// ScanStatus is what the scanner is doing and the mode it really runs in, which differs from the requested
// one when the stack cannot scan passively.
type ScanStatus struct {
	State ScanState
	Mode  ScanMode
}

// ModeScanner is implemented by adapters that can scan in more than one mode and report the scanner's
// state, including the Bluetooth stack going away and coming back. Adapters without it scan actively.
type ModeScanner interface {
	// SetScanMode selects the mode for the running scan and later ones.
	SetScanMode(ScanMode)
	ScanStatus() ScanStatus
	// OnScanStatus sets a function called after every status change. It must not block.
	OnScanStatus(func(ScanStatus))
}
