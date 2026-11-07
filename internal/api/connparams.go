package api

import "github.com/yuzi-co/btproxy/internal/ble"

// setConnParams handles BluetoothSetConnectionParamsRequest. As in ESPHome, the device must be connected
// through the proxy, and the reply comes once the controller has taken the request, not when the new
// parameters apply.
func (s *Server) setConnParams(c *client, addr uint64, p ble.ConnParams) {
	_, conn := s.attempt(addr)
	log := c.log.With("addr", ble.FormatMAC(addr), "min", p.MinInterval, "max", p.MaxInterval,
		"latency", p.Latency, "timeout", p.Timeout)
	cs, ok := ble.CanSetConnParams(s.adapter)
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
