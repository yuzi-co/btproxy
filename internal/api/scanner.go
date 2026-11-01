package api

import "btproxy/internal/ble"

// initScanner applies the configured scanner mode and has status changes reported to the subscribers.
func (s *Server) initScanner() {
	ms, ok := s.adapter.(ble.ModeScanner)
	if !ok {
		return
	}
	ms.SetScanMode(s.cfg.ScanMode)
	ms.OnScanStatus(func(ble.ScanStatus) {
		select {
		case s.scanKick <- struct{}{}:
		default: // a report is already due and will carry the latest status
		}
	})
}

// scannerState encodes the current BluetoothScannerStateResponse. Adapters without ble.ModeScanner scan
// actively and are running whenever someone is subscribed.
func (s *Server) scannerState() []byte {
	if ms, ok := s.adapter.(ble.ModeScanner); ok {
		st := ms.ScanStatus()
		return encScannerState(st.State, st.Mode, s.cfg.ScanMode)
	}
	s.mu.Lock()
	state := ble.ScanIdle
	if s.stopScan != nil {
		state = ble.ScanRunning
	}
	s.mu.Unlock()
	return encScannerState(state, ble.ScanActive, ble.ScanActive)
}

// sendScannerState reports the scanner state to the advertisement subscribers, as ESPHome does.
func (s *Server) sendScannerState() {
	msg := s.scannerState()
	s.mu.Lock()
	subs := s.advSubscribers()
	s.mu.Unlock()
	for _, c := range subs {
		c.send(msgBTScannerStateResponse, msg)
	}
}

func (s *Server) scanStatusLoop() {
	for range s.scanKick {
		s.sendScannerState()
	}
}

// setScanMode handles BluetoothScannerSetModeRequest. Home Assistant switches modes at run time (its auto
// mode opens short active windows); the configured mode it reads back stays the one from the command line.
func (s *Server) setScanMode(m ble.ScanMode) {
	if m != ble.ScanPassive && m != ble.ScanActive {
		s.log.Warn("unknown scanner mode", "mode", m)
	} else if ms, ok := s.adapter.(ble.ModeScanner); ok {
		s.log.Debug("scanner mode requested", "mode", m)
		ms.SetScanMode(m)
	}
	s.sendScannerState() // also when nothing changed, so the client sees the mode really in use
}
