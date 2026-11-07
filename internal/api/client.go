package api

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
)

// outQueue bounds the messages waiting for a client's writer. Advertisement batches and log lines, which
// come again or may be missed, are dropped once dropLimit are waiting. Anything else that finds the queue
// full closes the client, as ESPHome drops a client that cannot keep up: a stream missing a GATT response or
// a connection change would mislead it.
const (
	outQueue  = 256
	dropLimit = outQueue / 2
	maxWrite  = 8 << 10 // the writer sends what is waiting in writes of about this size
)

// client is one API connection. Messages are queued by send and written by the client's own writer, so a
// client that stops reading holds up nothing but itself.
type client struct {
	s   *Server
	c   net.Conn
	log *logx.Logger

	wmu              sync.Mutex
	out              chan outMsg   // to the writer; closed by run when it ends
	closed           bool          // out is closed; guarded by wmu
	dead             bool          // a write failed or the queue overflowed, nothing more is queued; guarded by wmu
	sendC            *noiseCipher  // set under wmu when the encrypted handshake is done, then used by the writer only
	logDrops         atomic.Int64  // log lines dropped since the writer last reported drops
	workers          atomic.Int32  // requests running in their own goroutines, see spawn
	recv             *noiseCipher  // used by run only
	advSubscribed    bool          // guarded by s.mu
	statesSubscribed bool          // guarded by s.mu
	hello            bool          // a HelloRequest has been handled; used by run only
	apiVersion       atomic.Uint64 // the client's API version from its first HelloRequest, major<<32 | minor
}

// apiMajorMinor returns the client's API version, 0.0 before its Hello. The GATT worker reads it while the
// client's reader may be handling another Hello, so it is kept atomically.
func (c *client) apiMajorMinor() (major, minor uint32) {
	v := c.apiVersion.Load()
	return uint32(v >> 32), uint32(v)
}

// outMsg is a queued message. Raw ones are sent as they are, without framing or encryption.
type outMsg struct {
	typ     uint32
	payload []byte
	raw     bool
}

// newClient makes a client and starts its writer.
func newClient(s *Server, c net.Conn, log *logx.Logger) *client {
	cl := &client{s: s, c: c, log: log, out: make(chan outMsg, outQueue)}
	go cl.writeLoop()
	return cl
}

// send queues one message; it never blocks. With encryption on, messages before the end of the handshake are
// dropped, like the broadcasts that reach every client.
func (c *client) send(typ uint32, payload []byte) { c.queue(outMsg{typ: typ, payload: payload}, false) }

// sendDroppable queues a message that may be lost, an advertisement batch or a log line, unless dropLimit
// messages are already waiting. It reports whether the message was queued.
func (c *client) sendDroppable(typ uint32, payload []byte) bool {
	return c.queue(outMsg{typ: typ, payload: payload}, true)
}

// writeRaw queues bytes to be sent as they are.
func (c *client) writeRaw(b []byte) { c.queue(outMsg{payload: b, raw: true}, false) }

func (c *client) queue(m outMsg, droppable bool) bool {
	c.wmu.Lock()
	if c.dead || c.closed || !m.raw && c.s.cfg.EncryptionKey != nil && c.sendC == nil {
		c.wmu.Unlock()
		return false
	}
	switch n := len(c.out); {
	case droppable && n >= dropLimit:
		c.wmu.Unlock()
		return false
	case n == cap(c.out):
		c.dead = true // before logging: with logs subscribed, the line would be queued for this client too
		c.wmu.Unlock()
		c.c.Close()
		c.log.Warn("client too slow, closing", "type", m.typ)
		return false
	}
	c.out <- m // never blocks: only queue sends, under wmu, and there is room
	c.wmu.Unlock()
	return true
}

// closeQueue lets the writer end once it has sent what is queued. Called by run when it returns.
func (c *client) closeQueue() {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.closed = true
	close(c.out)
}

// writeLoop sends the queued messages in order, as many as are waiting in one write, until the queue is
// closed, then closes the connection. After a failed write the rest is discarded. Log drops are reported to
// the client once it has caught up, not logged: a log line about them would only queue behind them.
func (c *client) writeLoop() {
	defer c.c.Close()
	var buf []byte
	failed := false
	for m := range c.out {
		if failed {
			continue
		}
		var err error
		buf, err = c.appendMsg(buf[:0], m)
		for more := true; more && err == nil && len(buf) < maxWrite; {
			select {
			case m, ok := <-c.out:
				if more = ok; ok {
					buf, err = c.appendMsg(buf, m)
				}
			default:
				more = false
				if n := c.logDrops.Swap(0); n > 0 {
					buf, err = c.appendMsg(buf, outMsg{typ: msgSubscribeLogsResponse, payload: encLog(logWarn,
						formatLog(logWarn, "api", 0, fmt.Sprintf("%d log messages dropped, client too slow", n)))})
				}
			}
		}
		if err == nil {
			c.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err = c.c.Write(buf)
		}
		if err != nil {
			failed = true
			c.wmu.Lock()
			c.dead = true // before logging, as in queue
			c.wmu.Unlock()
			c.c.Close()
			c.log.Debug("write", "err", err)
		}
		if cap(buf) > 2*maxWrite { // grown for a large message: not kept
			buf = nil
		}
	}
}

// appendMsg appends a message as it goes on the wire. Called by the writer only.
func (c *client) appendMsg(b []byte, m outMsg) ([]byte, error) {
	switch {
	case m.raw:
		return append(b, m.payload...), nil
	case c.sendC != nil:
		return c.appendNoise(b, m.typ, m.payload)
	}
	return AppendFrame(b, m.typ, m.payload), nil
}

func (c *client) run() {
	defer c.s.removeClient(c)
	defer c.closeQueue() // the writer sends what is queued, such as a refusal or DisconnectResponse, then closes
	c.log.Info("client connected")
	r := bufio.NewReader(c.c)
	read := ReadFrame
	c.c.SetReadDeadline(time.Now().Add(c.s.helloTimeout)) // HelloRequest clears it
	if c.s.cfg.EncryptionKey != nil {
		if err := c.noiseHandshake(r); err != nil {
			c.log.Warn("handshake failed", "err", err)
			return
		}
		c.log.Debug("encrypted session established")
		c.c.SetReadDeadline(time.Now().Add(c.s.setupTimeout)) // the key is proved: the usual time for Hello
		read = c.readNoise
	}
	for {
		f, err := read(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.log.Info("client disconnected")
			} else {
				c.log.Warn("client dropped", "err", err)
			}
			if errors.Is(err, ErrEncrypted) {
				// As ESPHome answers: a Noise client recognises the plaintext indicator and reports that the
				// device does not use encryption.
				c.writeRaw([]byte("\x00Bad indicator byte"))
			}
			return
		}
		if !c.handle(f) {
			return
		}
	}
}

// handle processes one message; false closes the connection.
func (c *client) handle(f Frame) bool {
	s := c.s
	switch f.Type {
	case msgHelloRequest:
		major, minor, err := decHello(f.Payload)
		if err != nil {
			c.log.Warn("bad message", "type", f.Type, "err", err)
			return false
		}
		c.c.SetReadDeadline(time.Time{})
		// Every Hello is answered, as ESPHome answers it, but the version comes from the first one: a client
		// has no reason to change it, and requests already running keep the version they were sent with.
		if !c.hello {
			c.hello = true
			c.apiVersion.Store(uint64(major)<<32 | uint64(minor))
		}
		c.log.Debug("hello", "api", fmt.Sprintf("%d.%d", major, minor))
		c.send(msgHelloResponse, encHelloResponse("btproxy "+s.cfg.ProjectVersion, s.cfg.Name))
	case msgDisconnectRequest:
		c.send(msgDisconnectResponse, nil)
		return false
	case msgDisconnectResponse:
		return false
	case msgPingRequest:
		c.send(msgPingResponse, nil)
	case msgPingResponse, msgGetTimeResponse, 3: // 3: AuthenticationRequest, sent by older clients
	case msgDeviceInfoRequest:
		c.send(msgDeviceInfoResponse, encDeviceInfo(deviceInfo{
			Name: s.cfg.Name, FriendlyName: s.cfg.FriendlyName, MAC: s.cfg.MAC,
			BTMAC: ble.FormatMAC(s.adapter.MAC()), Version: s.cfg.Version, Model: osModel(),
			Manufacturer: "btproxy", ProjectName: "yuzi.btproxy", ProjectVersion: s.cfg.ProjectVersion,
			SuggestedArea: s.cfg.SuggestedArea, FeatureFlags: s.featureFlags(),
			EncryptionSupported: s.cfg.EncryptionKey != nil,
		}))
	case msgDeviceCapabilitiesRequest:
		c.send(msgDeviceCapabilitiesResponse, encCapabilities(s.featureFlags(), ble.FormatMAC(s.adapter.MAC())))
	case msgListEntitiesRequest:
		c.sendEntities()
	case msgSubscribeStatesRequest:
		s.mu.Lock()
		c.statesSubscribed = true
		s.mu.Unlock()
		for _, m := range s.states() {
			c.send(m.typ, m.payload)
		}
	case msgSwitchCommandRequest:
		if err := c.switchCommand(f.Payload); err != nil {
			c.log.Warn("bad message", "type", f.Type, "err", err)
			return false
		}
	case msgButtonCommandRequest:
		ok, err := c.buttonCommand(f.Payload)
		if err != nil {
			c.log.Warn("bad message", "type", f.Type, "err", err)
			return false
		}
		if !ok {
			return false
		}
	case msgSubscribeHAServicesRequest, msgSubscribeHAStatesRequest:
		// Nothing to report: the proxy has no services and needs no Home Assistant states.
	case msgSubscribeLogsRequest:
		level, dump, err := decSubscribeLogs(f.Payload)
		if err != nil {
			c.log.Warn("bad message", "type", f.Type, "err", err)
			return false
		}
		if s.cfg.Logs == nil {
			break
		}
		s.cfg.Logs.subscribe(c, level)
		if dump && level >= logConfig {
			c.send(msgSubscribeLogsResponse, encLog(logConfig, formatLog(logConfig, "btproxy", 0, s.configDump())))
		}
	case msgNoiseEncryptionSetKeyRequest:
		c.send(msgNoiseEncryptionSetKeyResponse, nil) // success = false
	case msgSubscribeBTAdvertisements:
		r, err := decRequest(f.Type, f.Payload)
		if err != nil {
			c.log.Warn("bad message", "type", f.Type, "err", err)
			return false
		}
		if r.kind&1 == 0 {
			c.log.Warn("client wants legacy (parsed) advertisements, sending raw ones anyway")
		}
		s.mu.Lock()
		c.advSubscribed = true
		s.updateScan()
		s.mu.Unlock()
		c.send(msgBTScannerStateResponse, s.scannerState())
	case msgUnsubscribeBTAdvertisements:
		s.mu.Lock()
		c.advSubscribed = false
		s.updateScan()
		s.mu.Unlock()
	case msgBTScannerSetModeRequest:
		r, err := decRequest(f.Type, f.Payload)
		if err != nil {
			c.log.Warn("bad message", "type", f.Type, "err", err)
			return false
		}
		s.setScanMode(ble.ScanMode(r.kind))
	case msgSubscribeBTConnectionsFree:
		c.send(msgBTConnectionsFreeResponse, s.connectionsFree())
	case msgBTSetConnParamsRequest:
		addr, p, err := decConnParams(f.Payload)
		if err != nil {
			c.log.Warn("bad message", "type", f.Type, "err", err)
			return false
		}
		// Waits for the controller, like a device request.
		if !c.spawn(func() { s.setConnParams(c, addr, p) }) {
			return false
		}
	case msgBTDeviceRequest, msgBTGATTGetServicesRequest, msgBTGATTReadRequest, msgBTGATTWriteRequest,
		msgBTGATTReadDescriptorRequest, msgBTGATTWriteDescriptorRequest, msgBTGATTNotifyRequest:
		r, err := decRequest(f.Type, f.Payload)
		if err != nil {
			c.log.Warn("bad message", "type", f.Type, "err", err)
			return false
		}
		if f.Type == msgBTDeviceRequest {
			// Connects and disconnects block on the radio too, but must not wait behind a stuck GATT request.
			if !c.spawn(func() { c.deviceRequest(r) }) {
				return false
			}
		} else {
			s.enqueue(gattJob{c: c, typ: f.Type, r: r})
		}
	default:
		c.log.Info("ignoring unsupported message", "type", f.Type)
	}
	return true
}

// spawn runs a request that blocks on the radio in its own goroutine. It reports false, and runs nothing, if
// the client already has maxClientWorkers running.
func (c *client) spawn(f func()) bool {
	if c.workers.Add(1) > maxClientWorkers {
		c.workers.Add(-1)
		c.log.Warn("too many requests running, closing the client", "max", maxClientWorkers)
		return false
	}
	go func() {
		defer c.workers.Add(-1)
		f()
	}()
	return true
}

// deviceRequest runs a connect, disconnect, cache clearing or pairing request.
func (c *client) deviceRequest(r request) {
	s := c.s
	switch r.kind {
	case reqConnectV3WithCache, reqConnectV3WithoutCache:
		s.connect(c, r.addr, r.addrType)
	case reqDisconnect:
		s.disconnect(r.addr)
	case reqClearCache:
		err := s.adapter.ClearCache(r.addr)
		code := int32(0)
		if err != nil {
			c.log.Warn("clear cache", "addr", ble.FormatMAC(r.addr), "err", err)
			code = gattError
		}
		c.send(msgBTDeviceClearCacheResponse, encAddrResult(r.addr, err == nil, code))
	case reqPair:
		s.pair(c, r.addr)
	case reqUnpair:
		s.unpair(c, r.addr)
	}
}

// gatt runs one GATT request on conn, which is nil if the device is not connected.
func (c *client) gatt(conn ble.Conn, typ uint32, r request) {
	gattErr := func(err error) {
		c.log.Warn("gatt request failed", "type", typ, "addr", ble.FormatMAC(r.addr), "handle", r.handle, "err", err)
		c.send(msgBTGATTErrorResponse, encGATTError(r.addr, r.handle, gattError))
	}
	if conn == nil {
		gattErr(ble.ErrNotConnected)
		return
	}
	switch typ {
	case msgBTGATTGetServicesRequest:
		svcs, err := conn.Services()
		if err != nil {
			gattErr(err)
			return
		}
		efficient := efficientUUIDs(c.apiMajorMinor())
		for i := range svcs { // one service per message, like ESPHome
			c.send(msgBTGATTGetServicesResponse, encServices(r.addr, svcs[i:i+1], efficient))
		}
		c.send(msgBTGATTGetServicesDoneResponse, encAddrHandle(r.addr, 0))
	case msgBTGATTReadRequest, msgBTGATTReadDescriptorRequest:
		read := conn.Read
		if typ == msgBTGATTReadDescriptorRequest {
			read = conn.ReadDescriptor
		}
		data, err := read(r.handle)
		if err != nil {
			gattErr(err)
			return
		}
		c.send(msgBTGATTReadResponse, encAddrHandleData(r.addr, r.handle, data))
	case msgBTGATTWriteRequest:
		if err := conn.Write(r.handle, r.data, r.flag); err != nil {
			gattErr(err)
			return
		}
		if r.flag {
			c.send(msgBTGATTWriteResponse, encAddrHandle(r.addr, r.handle))
		}
	case msgBTGATTWriteDescriptorRequest:
		if err := conn.WriteDescriptor(r.handle, r.data); err != nil {
			gattErr(err)
			return
		}
		c.send(msgBTGATTWriteResponse, encAddrHandle(r.addr, r.handle))
	case msgBTGATTNotifyRequest:
		if err := conn.Notify(r.handle, r.flag); err != nil {
			gattErr(err)
			return
		}
		c.send(msgBTGATTNotifyResponse, encAddrHandle(r.addr, r.handle))
	}
}

// osModel is the model DeviceInfo reports, which Home Assistant shows on the proxy's Bluetooth scanner device.
func osModel() string {
	if runtime.GOOS == "windows" {
		return "Windows"
	}
	return "Linux"
}
