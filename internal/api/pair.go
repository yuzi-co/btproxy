package api

import "btproxy/internal/ble"

// pair bonds with a connected device. As in ESPHome, the device must be connected through the proxy.
func (s *Server) pair(c *client, addr uint64) {
	s.mu.Lock()
	var conn ble.Conn
	if bc := s.conns[addr]; bc != nil {
		conn = bc.conn
	}
	s.mu.Unlock()
	log := c.log.With("addr", ble.FormatMAC(addr))
	if conn == nil {
		log.Warn("pairing requested while not connected")
		c.send(msgBTDevicePairingResponse, encAddrResult(addr, false, gattNotConnected))
		return
	}
	log.Info("pairing")
	if err := conn.Pair(); err != nil {
		log.Warn("pairing failed", "err", err)
		c.send(msgBTDevicePairingResponse, encAddrResult(addr, false, gattError))
		return
	}
	log.Info("paired")
	c.send(msgBTDevicePairingResponse, encAddrResult(addr, true, 0))
}

// unpair removes a device's bond; the device need not be connected. The backend drops the connection with the
// bond (BlueZ removes the whole device), so the slot is released here and the disconnection reported after
// the unpairing response: Home Assistant's client fails the request if the connection state changes first.
func (s *Server) unpair(c *client, addr uint64) {
	s.mu.Lock()
	bc := s.conns[addr]
	if bc != nil && bc.conn != nil {
		delete(s.conns, addr)
	} else {
		bc = nil
	}
	s.mu.Unlock()
	log := c.log.With("addr", ble.FormatMAC(addr))
	err := s.adapter.Unpair(addr)
	code := int32(0)
	if err != nil {
		log.Warn("unpairing failed", "err", err)
		code = gattError
	} else {
		log.Info("unpaired")
	}
	c.send(msgBTDeviceUnpairingResponse, encAddrResult(addr, err == nil, code))
	if bc == nil {
		return
	}
	if err != nil { // the slot is already released: do not leave the link up without one
		bc.conn.Disconnect()
	}
	s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, false, 0, 0))
	s.sendConnectionsFree()
}
