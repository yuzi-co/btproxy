package api

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"btproxy/internal/ble"
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
}

// advBatchSize and advFlushInterval follow ESPHome's batching of raw advertisements.
const (
	advBatchSize     = 16
	advFlushInterval = 100 * time.Millisecond
)

// Server accepts API clients and shares one Bluetooth adapter among them.
type Server struct {
	cfg     Config
	adapter ble.Adapter
	log     *slog.Logger

	mu       sync.Mutex
	clients  map[*client]bool
	conns    map[uint64]*btConn
	stopScan func()
	pending  []ble.Advert
}

// btConn is a connection slot. conn is nil while the connection is being made; dropped records a link that
// went down before that. GATT requests for the connection run one at a time from queue, in arrival order, as
// ESPHome handles them; queue and running are guarded by Server.mu.
type btConn struct {
	conn    ble.Conn
	dropped bool
	queue   []gattJob
	running bool
}

type gattJob struct {
	c   *client
	typ uint32
	r   request
}

func NewServer(cfg Config, adapter ble.Adapter, log *slog.Logger) *Server {
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = 3
	}
	return &Server{
		cfg: cfg, adapter: adapter, log: log,
		clients: map[*client]bool{}, conns: map[uint64]*btConn{},
	}
}

// Serve accepts clients until the listener is closed.
func (s *Server) Serve(l net.Listener) error {
	go s.flushLoop()
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
			// Clients only ping while they receive nothing, and a scanning proxy sends all the time, so
			// silence from the client is normal; TCP keepalive finds dead peers instead of a read timeout.
			tc.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second,
				Interval: 10 * time.Second, Count: 3})
		}
		cl := &client{s: s, c: c, log: s.log.With("client", c.RemoteAddr().String())}
		s.mu.Lock()
		s.clients[cl] = true
		s.mu.Unlock()
		go cl.run()
	}
}

func (s *Server) featureFlags() uint32 {
	return featurePassiveScan | featureRawAdvertisements | featureStateAndMode |
		featureActiveConnections | featureRemoteCaching | featureCacheClearing
}

// subscribers returns the clients that receive advertisements.
func (s *Server) advSubscribers() []*client {
	var out []*client
	for c := range s.clients {
		if c.advSubscribed {
			out = append(out, c)
		}
	}
	return out
}

// updateScan starts scanning when the first client subscribes to advertisements and stops after the last
// one leaves. Called with s.mu held.
func (s *Server) updateScan() {
	want := len(s.advSubscribers()) > 0
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
		s.pending = nil
		s.log.Info("scanning stopped")
	}
}

func (s *Server) onAdvert(a ble.Advert) {
	s.mu.Lock()
	s.pending = append(s.pending, a)
	full := len(s.pending) >= advBatchSize
	s.mu.Unlock()
	if full {
		s.flush()
	}
}

func (s *Server) flushLoop() {
	for range time.Tick(advFlushInterval) {
		s.flush()
	}
}

func (s *Server) flush() {
	s.mu.Lock()
	ads := s.pending
	s.pending = nil
	subs := s.advSubscribers()
	s.mu.Unlock()
	for len(ads) > 0 {
		n := min(len(ads), advBatchSize)
		msg := encRawAdvertisements(ads[:n])
		for _, c := range subs {
			c.send(msgBTRawAdvertisementsResponse, msg)
		}
		ads = ads[n:]
	}
}

// broadcast sends a Bluetooth connection message to every client. Home Assistant is normally the only one.
func (s *Server) broadcast(typ uint32, payload []byte) {
	s.mu.Lock()
	cls := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		cls = append(cls, c)
	}
	s.mu.Unlock()
	for _, c := range cls {
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
	})
	if err != nil {
		log.Warn("connect failed", "err", err)
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
		}
	}
	s.broadcast(msgBTDeviceConnectionResponse, encConnection(addr, false, 0, 0))
	s.sendConnectionsFree()
}

// disconnectAll drops every peripheral connection, as an ESP32 proxy does when its last client goes away.
func (s *Server) disconnectAll() {
	s.mu.Lock()
	addrs := make([]uint64, 0, len(s.conns))
	for a, bc := range s.conns {
		if bc.conn != nil {
			addrs = append(addrs, a)
		}
	}
	s.mu.Unlock()
	for _, a := range addrs {
		s.disconnect(a)
	}
}

func (s *Server) removeClient(c *client) {
	s.mu.Lock()
	delete(s.clients, c)
	s.updateScan()
	last := len(s.clients) == 0
	s.mu.Unlock()
	if last {
		s.disconnectAll()
	}
}

// client is one API connection.
type client struct {
	s   *Server
	c   net.Conn
	log *slog.Logger

	wmu           sync.Mutex
	advSubscribed bool // guarded by s.mu
}

func (c *client) send(typ uint32, payload []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.c.Write(AppendFrame(nil, typ, payload)); err != nil {
		c.log.Debug("write", "err", err)
		c.c.Close()
	}
}

func (c *client) run() {
	defer c.s.removeClient(c)
	defer c.c.Close()
	c.log.Info("client connected")
	r := bufio.NewReader(c.c)
	for {
		f, err := ReadFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.log.Info("client disconnected")
			} else {
				c.log.Warn("client dropped", "err", err)
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
			BTMAC: ble.FormatMAC(s.adapter.MAC()), Version: s.cfg.Version, Model: "Linux",
			Manufacturer: "btproxy", ProjectName: "yuzi.btproxy", ProjectVersion: s.cfg.ProjectVersion,
			SuggestedArea: s.cfg.SuggestedArea, FeatureFlags: s.featureFlags(),
		}))
	case msgDeviceCapabilitiesRequest:
		c.send(msgDeviceCapabilitiesResponse, encCapabilities(s.featureFlags(), ble.FormatMAC(s.adapter.MAC())))
	case msgListEntitiesRequest:
		c.send(msgListEntitiesDoneResponse, nil)
	case msgSubscribeStatesRequest, msgSubscribeLogsRequest, msgSubscribeHAServicesRequest,
		msgSubscribeHAStatesRequest:
		// Nothing to report: the proxy has no entities, services or log stream.
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
		c.send(msgBTScannerStateResponse, encScannerState(scannerRunning, scannerActive, scannerActive))
	case msgUnsubscribeBTAdvertisements:
		s.mu.Lock()
		c.advSubscribed = false
		s.updateScan()
		s.mu.Unlock()
	case msgBTScannerSetModeRequest:
		// BlueZ scans actively; passive scanning would need its experimental advertisement monitor API.
		c.send(msgBTScannerStateResponse, encScannerState(scannerRunning, scannerActive, scannerActive))
	case msgSubscribeBTConnectionsFree:
		c.send(msgBTConnectionsFreeResponse, s.connectionsFree())
	case msgBTDeviceRequest, msgBTGATTGetServicesRequest, msgBTGATTReadRequest, msgBTGATTWriteRequest,
		msgBTGATTReadDescriptorRequest, msgBTGATTWriteDescriptorRequest, msgBTGATTNotifyRequest:
		r, err := decRequest(f.Type, f.Payload)
		if err != nil {
			c.log.Warn("bad message", "type", f.Type, "err", err)
			return false
		}
		if f.Type == msgBTDeviceRequest {
			// Connects and disconnects block on the radio too, but must not wait behind a stuck GATT request.
			go c.deviceRequest(r)
		} else {
			s.enqueue(gattJob{c: c, typ: f.Type, r: r})
		}
	default:
		c.log.Info("ignoring unsupported message", "type", f.Type)
	}
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
		c.send(msgBTDevicePairingResponse, encAddrResult(r.addr, false, gattError))
	case reqUnpair:
		c.send(msgBTDeviceUnpairingResponse, encAddrResult(r.addr, false, gattError))
	}
}

// enqueue queues a GATT request on its connection and starts the connection's worker if it is idle. Requests
// for a device that is not connected fail at once.
func (s *Server) enqueue(j gattJob) {
	s.mu.Lock()
	bc := s.conns[j.r.addr]
	if bc == nil || bc.conn == nil {
		s.mu.Unlock()
		j.c.gatt(nil, j.typ, j.r)
		return
	}
	bc.queue = append(bc.queue, j)
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
			c.send(msgBTGATTGetServicesResponse, encServices(r.addr, svcs[i:i+1]))
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
