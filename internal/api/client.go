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

// client is one API connection. Messages are queued on out and written by the client's own writer, so a
// client that stops reading holds up nothing but itself. The reader, run, is the only goroutine that reads
// from the connection; requests that wait for the radio run in their own goroutines, see spawn.
type client struct {
	s   *Server
	c   net.Conn
	log *logx.Logger
	out outbox

	workers    atomic.Int32  // requests running in their own goroutines, see spawn
	apiVersion atomic.Uint64 // the client's API version from its first HelloRequest, major<<32 | minor

	// Used by run only.
	recv  *noiseCipher // decrypts what is read once the handshake is done
	hello bool         // a HelloRequest has been handled

	// Guarded by Server.mu.
	advSubscribed    bool
	statesSubscribed bool
}

// apiMajorMinor returns the client's API version, 0.0 before its Hello. The GATT worker reads it while the
// client's reader may be handling another Hello, so it is kept atomically.
func (c *client) apiMajorMinor() (major, minor uint32) {
	v := c.apiVersion.Load()
	return uint32(v >> 32), uint32(v)
}

// newClient makes a client and starts its writer.
func newClient(s *Server, c net.Conn, log *logx.Logger) *client {
	cl := &client{s: s, c: c, log: log}
	cl.out.init(c, log, s.cfg.EncryptionKey != nil)
	go cl.out.writeLoop()
	return cl
}

// send queues one message; it never blocks. With encryption on, messages before the end of the handshake are
// dropped, like the broadcasts that reach every client.
func (c *client) send(typ uint32, payload []byte) {
	c.out.queue(outMsg{typ: typ, payload: payload}, false)
}

// sendDroppable queues a message that may be lost, an advertisement batch or a log line, unless dropLimit
// messages are already waiting. It reports whether the message was queued.
func (c *client) sendDroppable(typ uint32, payload []byte) bool {
	return c.out.queue(outMsg{typ: typ, payload: payload}, true)
}

// writeRaw queues bytes to be sent as they are, without framing or encryption.
func (c *client) writeRaw(b []byte) { c.out.queue(outMsg{payload: b, raw: true}, false) }

// outMsg is a queued message. Raw ones are sent as they are, without framing or encryption.
type outMsg struct {
	typ     uint32
	payload []byte
	raw     bool
}

// outbox is a client's write side: the queue to the client's writer and the state the writer shares with
// the goroutines that queue messages, all guarded by mu. The writer alone takes from the queue and uses
// the cipher.
type outbox struct {
	conn      net.Conn     // closed when a write fails or the client is too slow
	log       *logx.Logger // never logs from the writer's path, see writeLoop
	encrypted bool         // nothing but raw bytes goes out until the cipher is set
	logDrops  atomic.Int64 // log lines dropped since the writer last reported drops

	mu     sync.Mutex
	ch     chan outMsg  // to the writer; closed by close once the reader has ended
	closed bool         // ch is closed
	dead   bool         // a write failed or the queue overflowed, nothing more is queued
	cipher *noiseCipher // set when the encrypted handshake is done
}

func (o *outbox) init(conn net.Conn, log *logx.Logger, encrypted bool) {
	o.conn, o.log, o.encrypted = conn, log, encrypted
	o.ch = make(chan outMsg, outQueue)
}

// queue adds a message for the writer; it never blocks. Droppable messages are left out once dropLimit are
// waiting; any other message that finds the queue full marks the client dead and closes it. With encryption
// on, framed messages are dropped until the handshake sets the cipher. It reports whether m was queued.
func (o *outbox) queue(m outMsg, droppable bool) bool {
	o.mu.Lock()
	if o.dead || o.closed || !m.raw && o.encrypted && o.cipher == nil {
		o.mu.Unlock()
		return false
	}
	switch n := len(o.ch); {
	case droppable && n >= dropLimit:
		o.mu.Unlock()
		return false
	case n == cap(o.ch):
		o.dead = true // before logging: with logs subscribed, the line would be queued for this client too
		o.mu.Unlock()
		o.conn.Close()
		o.log.Warn("client too slow, closing", "type", m.typ)
		return false
	}
	o.ch <- m // never blocks: only queue sends, under mu, and there is room
	o.mu.Unlock()
	return true
}

// setCipher turns encryption on for everything queued from now on. The handshake queues its last raw
// message before calling it, so nothing encrypted can go ahead of that message.
func (o *outbox) setCipher(cipher *noiseCipher) {
	o.mu.Lock()
	o.cipher = cipher
	o.mu.Unlock()
}

// alive reports whether messages are still queued: false once a write failed or the queue overflowed.
func (o *outbox) alive() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.dead
}

// close lets the writer end once it has sent what is queued. Called by run when it returns.
func (o *outbox) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	close(o.ch)
}

// writeLoop sends the queued messages in order, as many as are waiting in one write, until the queue is
// closed, then closes the connection. After a failed write the rest is discarded. Log drops are reported to
// the client once it has caught up, not logged: a log line about them would only queue behind them.
func (o *outbox) writeLoop() {
	defer o.conn.Close()
	var buf []byte
	failed := false
	for m := range o.ch {
		if failed {
			continue
		}
		var err error
		buf, err = o.appendMsg(buf[:0], m)
		for more := true; more && err == nil && len(buf) < maxWrite; {
			select {
			case m, ok := <-o.ch:
				if more = ok; ok {
					buf, err = o.appendMsg(buf, m)
				}
			default:
				more = false
				if n := o.logDrops.Swap(0); n > 0 {
					buf, err = o.appendMsg(buf, outMsg{typ: msgSubscribeLogsResponse, payload: encLog(logWarn,
						formatLog(logWarn, "api", 0, fmt.Sprintf("%d log messages dropped, client too slow", n)))})
				}
			}
		}
		if err == nil {
			o.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err = o.conn.Write(buf)
		}
		if err != nil {
			failed = true
			o.mu.Lock()
			o.dead = true // before logging, as in queue
			o.mu.Unlock()
			o.conn.Close()
			o.log.Debug("write", "err", err)
		}
		if cap(buf) > 2*maxWrite { // grown for a large message: not kept
			buf = nil
		}
	}
}

// appendMsg appends a message as it goes on the wire. Called by the writer only.
func (o *outbox) appendMsg(b []byte, m outMsg) ([]byte, error) {
	switch {
	case m.raw:
		return append(b, m.payload...), nil
	case o.cipher != nil:
		return o.appendNoise(b, m.typ, m.payload)
	}
	return AppendFrame(b, m.typ, m.payload), nil
}

func (c *client) run() {
	defer c.s.removeClient(c)
	defer c.out.close() // the writer sends what is queued, such as a refusal or DisconnectResponse, then closes
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

// handler handles one message; false closes the connection.
type handler func(c *client, f Frame) bool

// handlers lists the messages the proxy takes, by type. Any other message is logged and ignored.
var handlers = map[uint32]handler{
	msgHelloRequest:                 (*client).helloRequest,
	msgDisconnectRequest:            (*client).disconnectRequest,
	msgDisconnectResponse:           (*client).closeRequest,
	msgPingRequest:                  (*client).pingRequest,
	msgPingResponse:                 (*client).ignore,
	msgGetTimeResponse:              (*client).ignore,
	3:                               (*client).ignore, // AuthenticationRequest, sent by older clients
	msgDeviceInfoRequest:            (*client).deviceInfoRequest,
	msgDeviceCapabilitiesRequest:    (*client).capabilitiesRequest,
	msgListEntitiesRequest:          (*client).listEntitiesRequest,
	msgSubscribeStatesRequest:       (*client).subscribeStatesRequest,
	msgSwitchCommandRequest:         (*client).switchCommand,
	msgButtonCommandRequest:         (*client).buttonCommand,
	msgSubscribeHAServicesRequest:   (*client).ignore, // the proxy has no services
	msgSubscribeHAStatesRequest:     (*client).ignore, // and needs no Home Assistant states
	msgSubscribeLogsRequest:         (*client).subscribeLogsRequest,
	msgNoiseEncryptionSetKeyRequest: (*client).setKeyRequest,
	msgSubscribeBTAdvertisements:    (*client).subscribeAdvertisements,
	msgUnsubscribeBTAdvertisements:  (*client).unsubscribeAdvertisements,
	msgBTScannerSetModeRequest:      (*client).setScanModeRequest,
	msgSubscribeBTConnectionsFree:   (*client).connectionsFreeRequest,
	msgBTSetConnParamsRequest:       (*client).setConnParamsRequest,
	msgBTDeviceRequest:              (*client).bluetoothDeviceRequest,
	msgBTGATTGetServicesRequest:     (*client).gattRequest,
	msgBTGATTReadRequest:            (*client).gattRequest,
	msgBTGATTWriteRequest:           (*client).gattRequest,
	msgBTGATTReadDescriptorRequest:  (*client).gattRequest,
	msgBTGATTWriteDescriptorRequest: (*client).gattRequest,
	msgBTGATTNotifyRequest:          (*client).gattRequest,
}

// handle processes one message; false closes the connection.
func (c *client) handle(f Frame) bool {
	h, ok := handlers[f.Type]
	if !ok {
		c.log.Info("ignoring unsupported message", "type", f.Type)
		return true
	}
	return h(c, f)
}

// bad logs a message that could not be decoded. It returns false: the client is closed, as ESPHome closes a
// client whose message does not parse.
func (c *client) bad(typ uint32, err error) bool {
	c.log.Warn("bad message", "type", typ, "err", err)
	return false
}

func (c *client) ignore(Frame) bool { return true }

func (c *client) closeRequest(Frame) bool { return false }

func (c *client) helloRequest(f Frame) bool {
	major, minor, err := decHello(f.Payload)
	if err != nil {
		return c.bad(f.Type, err)
	}
	c.c.SetReadDeadline(time.Time{})
	// Every Hello is answered, as ESPHome answers it, but the version comes from the first one: a client
	// has no reason to change it, and requests already running keep the version they were sent with.
	if !c.hello {
		c.hello = true
		c.apiVersion.Store(uint64(major)<<32 | uint64(minor))
	}
	c.log.Debug("hello", "api", fmt.Sprintf("%d.%d", major, minor))
	c.send(msgHelloResponse, encHelloResponse("btproxy "+c.s.cfg.ProjectVersion, c.s.cfg.Name))
	return true
}

func (c *client) disconnectRequest(Frame) bool {
	c.send(msgDisconnectResponse, nil)
	return false
}

func (c *client) pingRequest(Frame) bool {
	c.send(msgPingResponse, nil)
	return true
}

func (c *client) deviceInfoRequest(Frame) bool {
	s := c.s
	c.send(msgDeviceInfoResponse, encDeviceInfo(deviceInfo{
		Name: s.cfg.Name, FriendlyName: s.cfg.FriendlyName, MAC: s.cfg.MAC,
		BTMAC: ble.FormatMAC(s.adapter.MAC()), Version: s.cfg.Version, Model: osModel(),
		Manufacturer: "btproxy", ProjectName: "yuzi.btproxy", ProjectVersion: s.cfg.ProjectVersion,
		SuggestedArea: s.cfg.SuggestedArea, FeatureFlags: s.featureFlags(),
		EncryptionSupported: s.cfg.EncryptionKey != nil,
	}))
	return true
}

func (c *client) capabilitiesRequest(Frame) bool {
	c.send(msgDeviceCapabilitiesResponse, encCapabilities(c.s.featureFlags(), ble.FormatMAC(c.s.adapter.MAC())))
	return true
}

func (c *client) listEntitiesRequest(Frame) bool {
	c.sendEntities()
	return true
}

func (c *client) subscribeStatesRequest(Frame) bool {
	c.s.subscribeStates(c)
	for _, m := range c.s.states() {
		c.send(m.typ, m.payload)
	}
	return true
}

func (c *client) subscribeLogsRequest(f Frame) bool {
	level, dump, err := decSubscribeLogs(f.Payload)
	if err != nil {
		return c.bad(f.Type, err)
	}
	if c.s.cfg.Logs == nil {
		return true
	}
	c.s.cfg.Logs.subscribe(c, level)
	if dump && level >= logConfig {
		c.send(msgSubscribeLogsResponse, encLog(logConfig, formatLog(logConfig, "btproxy", 0, c.s.configDump())))
	}
	return true
}

func (c *client) setKeyRequest(Frame) bool {
	c.send(msgNoiseEncryptionSetKeyResponse, nil) // success = false
	return true
}

func (c *client) subscribeAdvertisements(f Frame) bool {
	r, err := decRequest(f.Type, f.Payload)
	if err != nil {
		return c.bad(f.Type, err)
	}
	if r.kind&1 == 0 {
		c.log.Warn("client wants legacy (parsed) advertisements, sending raw ones anyway")
	}
	c.s.subscribeAdverts(c, true)
	c.send(msgBTScannerStateResponse, c.s.scannerState())
	return true
}

func (c *client) unsubscribeAdvertisements(Frame) bool {
	c.s.subscribeAdverts(c, false)
	return true
}

func (c *client) setScanModeRequest(f Frame) bool {
	r, err := decRequest(f.Type, f.Payload)
	if err != nil {
		return c.bad(f.Type, err)
	}
	c.s.setScanMode(ble.ScanMode(r.kind))
	return true
}

func (c *client) connectionsFreeRequest(Frame) bool {
	c.send(msgBTConnectionsFreeResponse, c.s.connectionsFree())
	return true
}

func (c *client) setConnParamsRequest(f Frame) bool {
	addr, p, err := decConnParams(f.Payload)
	if err != nil {
		return c.bad(f.Type, err)
	}
	// Waits for the controller, like a device request.
	return c.spawn(func() { c.s.setConnParams(c, addr, p) })
}

// bluetoothDeviceRequest runs a connect, disconnect, cache clearing or pairing request in its own goroutine.
// Connects and disconnects block on the radio too, but must not wait behind a stuck GATT request.
func (c *client) bluetoothDeviceRequest(f Frame) bool {
	r, err := decRequest(f.Type, f.Payload)
	if err != nil {
		return c.bad(f.Type, err)
	}
	return c.spawn(func() { c.deviceRequest(r) })
}

// gattRequest queues a GATT request on its connection.
func (c *client) gattRequest(f Frame) bool {
	r, err := decRequest(f.Type, f.Payload)
	if err != nil {
		return c.bad(f.Type, err)
	}
	c.s.enqueue(gattJob{c: c, typ: f.Type, r: r})
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
