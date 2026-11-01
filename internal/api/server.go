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

// Config describes the proxy as Home Assistant sees it.
type Config struct {
	Name           string // node name, lower case with dashes, like an ESPHome node
	FriendlyName   string
	MAC            string // network MAC, Home Assistant's unique id for the device
	SuggestedArea  string
	Version        string // reported as esphome_version
	ProjectVersion string
	MaxConnections int
	ScanMode       ble.ScanMode // configured scanner mode; adapters without ble.ModeScanner scan actively
	Logs           *Logs        // log stream for clients that subscribe to logs; nil sends none
	Backend        string       // backend and adapter names, for the configuration dump
	Adapter        string
	EncryptionKey  []byte         // 32-byte API encryption key; nil serves plaintext only
	AdvFilter      *ble.AdvFilter // advertisements to forward; nil forwards all
}

// setupTimeout bounds the time from accepting a client to its HelloRequest, including the encrypted handshake,
// so a client that opens a connection and stalls is dropped. After Hello there is no read deadline.
const setupTimeout = 30 * time.Second

// Limits on what clients can make the proxy hold. A client over maxClientWorkers is closed; an ordinary one
// has a few requests waiting for the radio. A GATT request over the queue limits fails with a GATT error.
const (
	maxClients        = 16
	maxClientWorkers  = 32 // device and connection parameter requests running for one client
	maxGATTQueue      = 256
	maxGATTQueueBytes = 1 << 20 // of write data, per connection
)

// advBatchSize and advFlushInterval follow ESPHome's batching of raw advertisements: a batch is sent when it
// is full or advFlushInterval after its first advertisement.
const (
	advBatchSize     = 16
	advFlushInterval = 100 * time.Millisecond
	advEntrySize     = 80 // an encoded advertisement with 62 bytes of data, to size a batch's buffer
)

// Server accepts API clients and shares one Bluetooth adapter among them.
type Server struct {
	cfg     Config
	adapter ble.Adapter
	log     *logx.Logger

	mu       sync.Mutex
	clients  map[*client]bool
	conns    map[uint64]*btConn
	stopScan func()
	pending  []byte      // the advertisements of the batch being collected, encoded
	pendingN int         // and their number
	pendingT time.Time   // when the batch started
	flushT   *time.Timer // armed by the first advertisement of a batch

	scanKick chan struct{} // scanner status changed: tell the subscribers

	stats   stats    // see stats.go
	sensors []sensor // fixed by NewServer

	setupTimeout time.Duration // a field so the tests can shorten it
}

// btConn is a connection slot. conn is nil while the connection is being made; dropped records a link that
// went down before that. GATT requests for the connection run one at a time from queue, in arrival order, as
// ESPHome handles them; queue and running are guarded by Server.mu.
type btConn struct {
	conn       ble.Conn
	dropped    bool
	queue      []gattJob
	queueBytes int // write data in queue
	running    bool
}

type gattJob struct {
	c   *client
	typ uint32
	r   request
}

func NewServer(cfg Config, adapter ble.Adapter, log *logx.Logger) *Server {
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 3
	}
	s := &Server{
		cfg: cfg, adapter: adapter, log: log,
		clients: map[*client]bool{}, conns: map[uint64]*btConn{}, scanKick: make(chan struct{}, 1),
		setupTimeout: setupTimeout,
	}
	s.initScanner()
	s.initStats()
	return s
}

// Serve accepts clients until the listener is closed. Other accept errors, such as running out of file
// descriptors, are retried after a pause that doubles from 5 ms to 1 s, as net/http does.
func (s *Server) Serve(l net.Listener) error {
	go s.scanStatusLoop()
	go s.statsLoop()
	var delay time.Duration
	var lastFull time.Time
	for {
		c, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return err
		}
		if err != nil {
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			s.log.Warn("accept failed, retrying", "err", err, "in", delay)
			time.Sleep(delay)
			continue
		}
		delay = 0
		s.mu.Lock()
		full := len(s.clients) >= maxClients // only this loop adds clients, so the count cannot grow past the check
		s.mu.Unlock()
		if full {
			if time.Since(lastFull) > time.Minute { // a flood of connections must not flood the log too
				s.log.Warn("too many clients, refusing connections", "max", maxClients, "from", c.RemoteAddr().String())
				lastFull = time.Now()
			}
			c.Close()
			continue
		}
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
			// Clients only ping while they receive nothing, and a scanning proxy sends all the time, so
			// silence from the client is normal; TCP keepalive finds dead peers instead of a read timeout.
			tc.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second,
				Interval: 10 * time.Second, Count: 3})
		}
		cl := newClient(s, c, s.log.With("client", c.RemoteAddr().String()))
		s.mu.Lock()
		s.clients[cl] = true
		s.mu.Unlock()
		go cl.run()
	}
}

func (s *Server) featureFlags() uint32 {
	f := uint32(featurePassiveScan | featureRawAdvertisements | featureStateAndMode |
		featureActiveConnections | featureRemoteCaching | featureCacheClearing | featurePairing)
	if _, ok := ble.CanSetConnParams(s.adapter); ok {
		f |= featureConnParams
	}
	return f
}

// advSubscribed reports whether any client receives advertisements. Called with s.mu held.
func (s *Server) advSubscribed() bool {
	for c := range s.clients {
		if c.advSubscribed {
			return true
		}
	}
	return false
}

// updateScan starts scanning when the first client subscribes to advertisements and stops after the last
// one leaves. Called with s.mu held.
func (s *Server) updateScan() {
	want := s.advSubscribed()
	switch {
	case want && s.stopScan == nil:
		stop, err := s.adapter.Scan(s.onAdvert)
		if err != nil {
			s.log.Error("start scan", "err", err)
			return
		}
		s.stopScan = stop
		s.log.Info("scanning started")
	case !want && s.stopScan != nil:
		s.stopScan()
		s.stopScan = nil
		s.pending, s.pendingN = nil, 0
		if s.flushT != nil {
			s.flushT.Stop()
		}
		s.log.Info("scanning stopped")
	}
}

// onAdvert encodes an advertisement into the batch being collected, copying its data, which is only valid
// during the call. Advertisements the filter drops stop here, before the lock. The adapter has counted them
// by then, so its stall watchdog and raw advertisement checks see every advertisement, filtered or not.
func (s *Server) onAdvert(a ble.Advert) {
	if !s.cfg.AdvFilter.Allow(a) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = make([]byte, 0, advBatchSize*advEntrySize)
	}
	s.pending = appendRawAdvert(s.pending, a)
	s.stats.adverts.Add(1)
	if s.pendingN++; s.pendingN >= advBatchSize {
		s.flush()
		return
	}
	if s.pendingN == 1 { // nothing runs while no advertisements arrive
		s.pendingT = time.Now()
		if s.flushT == nil {
			s.flushT = time.AfterFunc(advFlushInterval, s.flushDue)
		} else {
			s.flushT.Reset(advFlushInterval)
		}
	}
}

// flushDue sends the batch whose time is up. A batch sent when it filled up may have been followed by a new
// one by the time this runs; the timer has been armed again for that one.
func (s *Server) flushDue() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingN > 0 && time.Since(s.pendingT) >= advFlushInterval {
		s.flush()
	}
}

// flush queues the batch for the subscribers, which share it: it is not changed after this. The next batch
// gets a new buffer. Called with s.mu held; queueing never blocks.
func (s *Server) flush() {
	if s.pendingN == 0 {
		return
	}
	msg, n := s.pending, s.pendingN
	s.pending, s.pendingN = nil, 0
	if s.flushT != nil {
		s.flushT.Stop()
	}
	for c := range s.clients {
		if c.advSubscribed && !c.sendDroppable(msgBTRawAdvertisementsResponse, msg) {
			s.stats.advDropped.Add(uint64(n))
		}
	}
}

// broadcast sends a Bluetooth connection message to every client. Home Assistant is normally the only one.
func (s *Server) broadcast(typ uint32, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		c.send(typ, payload)
	}
}

func (s *Server) connectionsFree() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	allocated := make([]uint64, 0, len(s.conns))
	for a := range s.conns {
		allocated = append(allocated, a)
	}
	return encConnectionsFree(uint32(s.cfg.MaxConnections-len(s.conns)), uint32(s.cfg.MaxConnections), allocated)
}

func (s *Server) sendConnectionsFree() {
	s.broadcast(msgBTConnectionsFreeResponse, s.connectionsFree())
	s.pushStates() // the connection sensors changed
}

func (s *Server) connect(addr uint64, addrType uint32) {
	s.mu.Lock()
	if bc, ok := s.conns[addr]; ok {
		conn := bc.conn
		s.mu.Unlock()
		if conn != nil { // already connected: confirm again
			s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, true, conn.MTU(), 0))
		}
		return
	}
	if len(s.conns) >= s.cfg.MaxConnections {
		s.mu.Unlock()
		s.log.Warn("no free connection slot", "addr", ble.FormatMAC(addr))
		s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, false, 0, gattError))
		return
	}
	slot := &btConn{}
	s.conns[addr] = slot
	s.mu.Unlock()
	s.sendConnectionsFree()

	log := s.log.With("addr", ble.FormatMAC(addr))
	log.Info("connecting")
	s.stats.connTried.Add(1)
	conn, err := s.adapter.Connect(addr, addrType, ble.Events{
		Notification: func(h uint16, data []byte) {
			s.broadcast(msgBTGATTNotifyDataResponse, encAddrHandleData(addr, h, data))
		},
		Disconnected: func(reason error) {
			s.mu.Lock()
			mine := s.conns[addr] == slot
			if mine && slot.conn == nil { // Connect has not returned yet: it frees the slot
				slot.dropped = true
				mine = false
			} else if mine {
				delete(s.conns, addr)
			}
			s.mu.Unlock()
			if mine {
				log.Info("disconnected", "reason", reason)
				s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, false, 0, 0))
				s.sendConnectionsFree()
			}
		},
		Withdrawn: func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.conns[addr] != slot
		},
	})
	if err != nil {
		log.Warn("connect failed", "err", err)
		s.recordConnect(false)
		s.release(addr, slot)
		s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, false, 0, gattError))
		s.sendConnectionsFree()
		return
	}
	s.mu.Lock()
	withdrawn, dropped := s.conns[addr] != slot, slot.dropped
	if !withdrawn && !dropped {
		slot.conn = conn
	}
	s.mu.Unlock()
	s.recordConnect(!dropped)
	if withdrawn { // disconnect requested, or the last client left, while connecting
		log.Info("connected after the request was withdrawn, disconnecting")
		conn.Disconnect()
		return
	}
	if dropped {
		log.Info("link lost while connecting")
		s.release(addr, slot)
		s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, false, 0, 0))
		s.sendConnectionsFree()
		return
	}
	log.Info("connected", "mtu", conn.MTU())
	s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, true, conn.MTU(), 0))
	s.pushStates()
}

// release frees a connection slot unless it has been replaced.
func (s *Server) release(addr uint64, slot *btConn) {
	s.mu.Lock()
	if s.conns[addr] == slot {
		delete(s.conns, addr)
	}
	s.mu.Unlock()
}

func (s *Server) disconnect(addr uint64) {
	s.mu.Lock()
	bc := s.conns[addr]
	delete(s.conns, addr)
	s.mu.Unlock()
	if bc != nil && bc.conn != nil {
		if err := bc.conn.Disconnect(); err != nil {
			s.log.Warn("disconnect", "addr", ble.FormatMAC(addr), "err", err)
		} else {
			s.log.Info("disconnected on request", "addr", ble.FormatMAC(addr))
		}
	}
	s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, false, 0, 0))
	s.sendConnectionsFree()
}

// disconnectAll drops every peripheral connection, as an ESP32 proxy does when its last client goes away.
// Connects still in progress are withdrawn too: connect disconnects them when they complete.
func (s *Server) disconnectAll() {
	s.mu.Lock()
	addrs := make([]uint64, 0, len(s.conns))
	for a := range s.conns {
		addrs = append(addrs, a)
	}
	s.mu.Unlock()
	for _, a := range addrs {
		s.disconnect(a)
	}
}

func (s *Server) removeClient(c *client) {
	if s.cfg.Logs != nil {
		s.cfg.Logs.unsubscribe(c)
	}
	s.mu.Lock()
	delete(s.clients, c)
	s.updateScan()
	last := len(s.clients) == 0
	s.mu.Unlock()
	if last {
		s.disconnectAll()
	}
}

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
	out              chan outMsg  // to the writer; closed by run when it ends
	closed           bool         // out is closed; guarded by wmu
	dead             bool         // a write failed or the queue overflowed, nothing more is queued; guarded by wmu
	sendC            *noiseCipher // set under wmu when the encrypted handshake is done, then used by the writer only
	logDrops         atomic.Int64 // log lines dropped since the writer last reported drops
	workers          atomic.Int32 // requests running in their own goroutines, see spawn
	recv             *noiseCipher // used by run only
	advSubscribed    bool         // guarded by s.mu
	statesSubscribed bool         // guarded by s.mu
	apiMajor         uint32
	apiMinor         uint32 // set by HelloRequest, before any Bluetooth request
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
	c.c.SetReadDeadline(time.Now().Add(c.s.setupTimeout)) // HelloRequest clears it
	if c.s.cfg.EncryptionKey != nil {
		if err := c.noiseHandshake(r); err != nil {
			c.log.Warn("handshake failed", "err", err)
			return
		}
		c.log.Debug("encrypted session established")
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
		var err error
		if c.apiMajor, c.apiMinor, err = decHello(f.Payload); err != nil {
			c.log.Warn("bad message", "type", f.Type, "err", err)
			return false
		}
		c.c.SetReadDeadline(time.Time{})
		c.log.Debug("hello", "api", fmt.Sprintf("%d.%d", c.apiMajor, c.apiMinor))
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
			c.send(msgSensorStateResponse, m)
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
		s.connect(r.addr, r.addrType)
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

// enqueue queues a GATT request on its connection and starts the connection's worker if it is idle. Requests
// for a device that is not connected, or that find the queue full, fail at once.
func (s *Server) enqueue(j gattJob) {
	s.mu.Lock()
	bc := s.conns[j.r.addr]
	if bc == nil || bc.conn == nil {
		s.mu.Unlock()
		j.c.gatt(nil, j.typ, j.r)
		return
	}
	if len(bc.queue) >= maxGATTQueue || bc.queueBytes+len(j.r.data) > maxGATTQueueBytes {
		s.mu.Unlock()
		j.c.log.Warn("gatt queue full", "addr", ble.FormatMAC(j.r.addr), "handle", j.r.handle)
		j.c.send(msgBTGATTErrorResponse, encGATTError(j.r.addr, j.r.handle, gattError))
		return
	}
	bc.queue = append(bc.queue, j)
	bc.queueBytes += len(j.r.data)
	start := !bc.running
	bc.running = true
	s.mu.Unlock()
	if start {
		go s.runQueue(bc)
	}
}

// runQueue runs a connection's GATT requests until its queue is empty. Requests still queued when the
// connection goes away fail with ErrNotConnected instead of reaching the old connection.
func (s *Server) runQueue(bc *btConn) {
	for {
		s.mu.Lock()
		if len(bc.queue) == 0 {
			bc.running = false
			s.mu.Unlock()
			return
		}
		j := bc.queue[0]
		bc.queue[0] = gattJob{}
		bc.queue = bc.queue[1:]
		bc.queueBytes -= len(j.r.data)
		var conn ble.Conn
		if s.conns[j.r.addr] == bc {
			conn = bc.conn
		}
		s.mu.Unlock()
		j.c.gatt(conn, j.typ, j.r)
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
		for i := range svcs { // one service per message, like ESPHome
			c.send(msgBTGATTGetServicesResponse, encServices(r.addr, svcs[i:i+1], efficientUUIDs(c.apiMajor, c.apiMinor)))
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
