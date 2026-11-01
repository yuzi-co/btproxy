package api

import "btproxy/internal/ble"

// setConnParams handles BluetoothSetConnectionParamsRequest. As in ESPHome, the device must be connected
// through the proxy, and the reply comes once the controller has taken the request, not when the new
// parameters apply.
func (s *Server) setConnParams(c *client, addr uint64, p ble.ConnParams) {
	s.mu.Lock()
	var conn ble.Conn
	if bc := s.conns[addr]; bc != nil {
		conn = bc.conn
	}
	s.mu.Unlock()
	log := c.log.With("addr", ble.FormatMAC(addr), "min", p.MinInterval, "max", p.MaxInterval,
		"latency", p.Latency, "timeout", p.Timeout)
	cs, ok := s.adapter.(ble.ConnParamsSetter)
	code := int32(0)
	switch {
	case conn == nil:
		log.Warn("connection parameters requested while not connected")
		code = gattNotConnected
	case !ok: // not advertised, so only a client that ignores the feature flags gets here
		log.Warn("connection parameters requested, but the backend cannot change them")
		code = gattError
	default:
		if err := cs.SetConnParams(addr, p); err != nil {
			log.Warn("setting connection parameters failed", "err", err)
			code = gattError
		} else {
			log.Info("connection parameters requested")
		}
	}
	c.send(msgBTSetConnParamsResponse, encConnParamsResult(addr, code))
}
