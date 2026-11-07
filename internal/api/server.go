package api

import (
	"errors"
	"net"
	"sync"
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
	Restart        func()         // restarts the proxy, for the restart button; nil leaves the button out
	LastProblem    func() string  // the latest warning or error; nil leaves the sensor out
}

// A client that opens a connection and stalls is dropped, and quickly while it has proved nothing, so that
// any host on the network cannot hold the client slots: helloTimeout bounds the time from accepting a client
// to its HelloRequest, or with encryption to the end of the handshake, which proves the key. A client that has
// proved it gets setupTimeout for its Hello. After Hello there is no read deadline.
const (
	helloTimeout = 5 * time.Second
	setupTimeout = 30 * time.Second
)

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
	scanOff  bool        // the scanning switch is off
	pending  []byte      // the advertisements of the batch being collected, encoded
	pendingN int         // and their number
	pendingT time.Time   // when the batch started
	flushT   *time.Timer // armed by the first advertisement of a batch

	scanKick chan struct{} // scanner status changed: tell the subscribers

	stats    stats    // see stats.go
	entities []entity // in the order they are listed; fixed by NewServer

	helloTimeout time.Duration // fields so the tests can shorten them
	setupTimeout time.Duration
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
		helloTimeout: helloTimeout, setupTimeout: setupTimeout,
	}
	s.initScanner()
	s.initStats()
	s.initEntities()
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

// subscribeAdverts starts or stops the advertisement stream to c, and the scanner with it when c is the first
// or last subscriber.
func (s *Server) subscribeAdverts(c *client, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.advSubscribed = on
	s.updateScan()
}

// subscribeStates has c sent the entity states from now on.
func (s *Server) subscribeStates(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.statesSubscribed = true
}

// updateScan starts scanning when the first client subscribes to advertisements and stops after the last
// one leaves, or while the scanning switch is off. Called with s.mu held.
func (s *Server) updateScan() {
	want := s.advSubscribed() && !s.scanOff
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
	s.eachClient(func(c *client) {
		if c.advSubscribed && !c.sendDroppable(msgBTRawAdvertisementsResponse, msg) {
			s.stats.advDropped.Add(uint64(n))
		}
	})
}

// eachClient calls f for every client. Called with s.mu held, so f must not block or take the lock: queueing
// a message is fine.
func (s *Server) eachClient(f func(*client)) {
	for c := range s.clients {
		f(c)
	}
}

// fanOut calls f for every client under s.mu. Everything that goes to all clients at once, or to all the
// subscribers of something, comes through here.
func (s *Server) fanOut(f func(*client)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eachClient(f)
}

// broadcast sends a Bluetooth connection message to every client. Home Assistant is normally the only one.
func (s *Server) broadcast(typ uint32, payload []byte) {
	s.fanOut(func(c *client) { c.send(typ, payload) })
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

// connect makes the connection c asked for. A connect from a client that has gone, as when the request and
// the close arrive together, is refused: its slot would otherwise be taken after disconnectAll has run for
// the client, and the connection would be held for nobody.
func (s *Server) connect(c *client, addr uint64, addrType uint32) {
	s.mu.Lock()
	if bc, ok := s.conns[addr]; ok {
		conn := bc.conn
		s.mu.Unlock()
		if conn != nil { // already connected: confirm again
			s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, true, conn.MTU(), 0))
		}
		return
	}
	if !s.clients[c] {
		s.mu.Unlock()
		c.log.Info("connect requested by a client that has left, refusing", "addr", ble.FormatMAC(addr))
		c.send(msgBTDeviceConnectionResponse, encConnection(addr, false, 0, gattError))
		return
	}
	if len(s.conns) >= s.cfg.MaxConnections {
		s.mu.Unlock()
		s.log.Warn("no free connection slot", "addr", ble.FormatMAC(addr))
		s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, false, 0, gattError))
		return
	}
	a := &connAttempt{s: s, addr: addr, slot: &btConn{}}
	s.conns[addr] = a.slot
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
			if a.lost() {
				log.Info("disconnected", "reason", reason)
				a.ended(0)
			}
		},
		Withdrawn: a.withdrawn,
	})
	if err != nil {
		log.Warn("connect failed", "err", err)
		s.recordConnect(false)
		a.ended(gattError)
		return
	}
	withdrawn, dropped := a.settle(conn)
	if withdrawn { // disconnect requested, or the last client left, while connecting
		// Neither a success nor a failure: nobody wanted the outcome, so the success rate leaves it out.
		log.Info("connected after the request was withdrawn, disconnecting")
		conn.Disconnect()
		return
	}
	s.recordConnect(!dropped)
	if dropped {
		log.Info("link lost while connecting")
		a.ended(0)
		return
	}
	log.Info("connected", "mtu", conn.MTU())
	s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, true, conn.MTU(), 0))
	s.pushStates()
}

// connAttempt is one attempt to hold a connection slot: the slot taken for addr, from the connect that takes
// it until it is released. Every way an attempt ends, whether the connect fails, the link drops, a client
// asks for the disconnect or the device is unpaired, goes through ended, so the slot is released once and
// the clients are told once. The slot may change hands meanwhile: a disconnect withdraws a connect still
// running and the address may be connected again, so the attempt only ever touches a slot that is its own.
type connAttempt struct {
	s    *Server
	addr uint64
	slot *btConn // nil for an attempt looked up on an address that holds no slot
}

// attempt returns the attempt holding addr's slot, with a nil slot when there is none, and the connection if
// it is established.
func (s *Server) attempt(addr uint64) (a *connAttempt, conn ble.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a = &connAttempt{s: s, addr: addr, slot: s.conns[addr]}
	if a.slot != nil {
		conn = a.slot.conn
	}
	return a, conn
}

// release frees the slot if the attempt still holds it. Done before a disconnect is asked of the backend, so
// that its report of the link going down finds the slot gone and is not reported to the clients again.
func (a *connAttempt) release() {
	a.s.mu.Lock()
	if a.slot != nil && a.s.conns[a.addr] == a.slot {
		delete(a.s.conns, a.addr)
	}
	a.s.mu.Unlock()
}

// ended reports the end of the attempt: the slot is released if the attempt still holds it, every client is
// told the device is not connected, with code, and the free slots are announced.
func (a *connAttempt) ended(code int32) {
	a.release()
	a.s.broadcast(msgBTDeviceConnectionResponse, encConnection(a.addr, false, 0, code))
	a.s.sendConnectionsFree()
}

// lost handles the backend's report that the link went down. It reports whether the attempt held an
// established connection, which it releases. While the connect is still running the slot stays, marked
// dropped, and connect reports the loss when it returns.
func (a *connAttempt) lost() bool {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if a.s.conns[a.addr] != a.slot {
		return false
	}
	if a.slot.conn == nil {
		a.slot.dropped = true
		return false
	}
	delete(a.s.conns, a.addr)
	return true
}

// withdrawn reports whether the slot has been taken from the attempt: a disconnect was requested, or the
// last client left, while the connect was running. Backends that retry a connect check it between tries.
func (a *connAttempt) withdrawn() bool {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	return a.s.conns[a.addr] != a.slot
}

// settle records the connection the backend made on the slot, unless the attempt was withdrawn meanwhile
// or the link was lost already, which it reports instead.
func (a *connAttempt) settle(conn ble.Conn) (withdrawn, dropped bool) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	withdrawn, dropped = a.s.conns[a.addr] != a.slot, a.slot.dropped
	if !withdrawn && !dropped {
		a.slot.conn = conn
	}
	return withdrawn, dropped
}

// disconnect ends the connection on addr, or withdraws the connect still making it.
func (s *Server) disconnect(addr uint64) {
	a, conn := s.attempt(addr)
	a.release()
	if conn != nil {
		if err := conn.Disconnect(); err != nil {
			s.log.Warn("disconnect", "addr", ble.FormatMAC(addr), "err", err)
		} else {
			s.log.Info("disconnected on request", "addr", ble.FormatMAC(addr))
		}
	}
	a.ended(0)
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
