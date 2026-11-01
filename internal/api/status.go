package api

import "btproxy/internal/ble"

// Status is a summary of a server's state for a local user interface (the Windows tray and settings page).
type Status struct {
	Clients     int     // API clients connected
	Connections int     // established Bluetooth connections
	AdvertRate  float32 // advertisements per second, 0 until known
	ScanFailed  bool    // the adapter reports its scanner failed, e.g. with the radio off
}

// Status returns the server's current state.
func (s *Server) Status() Status {
	var st Status
	st.Connections, _ = s.slots()
	st.AdvertRate, _ = s.advertRate()
	if ms, ok := s.adapter.(ble.ModeScanner); ok {
		st.ScanFailed = ms.ScanStatus().State == ble.ScanFailed
	}
	s.mu.Lock()
	st.Clients = len(s.clients)
	s.mu.Unlock()
	return st
}
