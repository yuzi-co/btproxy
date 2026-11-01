// Package mdns is a small mDNS/DNS-SD responder (RFC 6762, 6763) for a single service, enough for Home
// Assistant's zeroconf browser to discover btproxy as an ESPHome device (_esphomelib._tcp) and follow its address.
// IPv4 only. It shares UDP port 5353 with other responders on the host (umdns, avahi) through SO_REUSEADDR and
// SO_REUSEPORT, and answers each query with the addresses of the interface it came from.
package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
	"github.com/yuzi-co/btproxy/internal/netif"
)

const (
	port         = 5353
	scanInterval = 30 * time.Second // how often interface addresses are checked for changes
	announceN    = 3
)

var group = net.IPv4(224, 0, 0, 251)

// Config describes the advertised service.
type Config struct {
	Name string   // instance and host name; must already be a DNS label (see Label)
	Port int      // API port
	TXT  []string // key=value pairs
	// Ifaces are the interface names to serve. Empty means every up, multicast, non-loopback one with IPv4,
	// except the virtual ones (see netif.Virtual) while any other is left.
	Ifaces []string
}

type iface struct {
	name  string
	index int
	ips   []net.IP // IPv4
	nets  []*net.IPNet
}

func (i iface) String() string { return fmt.Sprint(i.name, i.ips) }

// Responder answers queries and announces the service until Close.
type Responder struct {
	svc   service
	names []string
	log   *logx.Logger
	conn  *net.UDPConn
	raw   syscall.RawConn

	mu     sync.Mutex // guards ifaces and joined, and pairs IP_MULTICAST_IF with the write that depends on it
	ifaces []iface
	joined map[string]string // interface name -> String() when the group was joined on it

	stop      chan struct{}
	closeOnce sync.Once
}

// Start opens the socket, joins the group on the configured interfaces and starts answering and announcing.
func Start(cfg Config, log *logx.Logger) (*Responder, error) {
	lc := net.ListenConfig{Control: reuse}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	conn := pc.(*net.UDPConn)
	raw, err := conn.SyscallConn()
	if err == nil {
		err = control(raw, setup)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	r := &Responder{svc: newService(cfg.Name, cfg.Port, cfg.TXT), names: cfg.Ifaces, log: log, conn: conn, raw: raw,
		joined: map[string]string{}, stop: make(chan struct{})}
	r.refresh()
	if len(cfg.Ifaces) == 0 {
		if _, skipped := scan(nil); len(skipped) > 0 {
			log.Info("mdns skips virtual interfaces", "ifaces", skipped)
		}
	}
	for _, n := range cfg.Ifaces {
		if !slices.ContainsFunc(r.ifaces, func(i iface) bool { return i.name == n }) {
			log.Warn("mdns interface not up or without IPv4, will retry", "iface", n)
		}
	}
	log.Info("mdns started", "host", strings.TrimSuffix(r.svc.host, "."), "ifaces", r.current())
	go r.read()
	go r.watch()
	return r, nil
}

// Close sends goodbye packets (TTL 0) so browsers drop the service at once, then closes the socket.
func (r *Responder) Close() {
	r.closeOnce.Do(func() {
		close(r.stop)
		for _, i := range r.current() {
			r.multicast(i, encodeResponse(0, nil, r.svc.all(i.ips), nil, false, true))
		}
		r.conn.Close()
	})
}

func (r *Responder) current() []iface {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ifaces
}

// watch announces at start and again whenever the interface addresses change.
func (r *Responder) watch() {
	r.announce()
	t := time.NewTicker(scanInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			if r.refresh() {
				r.log.Info("mdns addresses changed", "ifaces", r.current())
				r.announce()
			}
		}
	}
}

func (r *Responder) announce() {
	for n := range announceN {
		if n > 0 {
			select {
			case <-r.stop:
				return
			case <-time.After(time.Second):
			}
		}
		for _, i := range r.current() {
			r.multicast(i, encodeResponse(0, nil, r.svc.all(i.ips), nil, false, false))
		}
	}
}

// refresh rescans the interfaces, joins the group on new ones and reports whether anything changed.
func (r *Responder) refresh() bool {
	ifs, _ := scan(r.names)
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := !slices.EqualFunc(ifs, r.ifaces, func(a, b iface) bool { return a.String() == b.String() })
	r.ifaces = ifs
	seen := map[string]bool{}
	for _, i := range ifs {
		seen[i.name] = true
		if r.joined[i.name] == i.String() {
			continue
		}
		// Joining again on an interface the socket is already a member on fails harmlessly.
		if err := control(r.raw, func(fd uintptr) error { return join(fd, i) }); err != nil {
			r.log.Debug("mdns join", "iface", i.name, "err", err)
		}
		r.joined[i.name] = i.String()
	}
	for n := range r.joined {
		if !seen[n] {
			delete(r.joined, n) // gone: the kernel dropped the membership, join again when it is back
		}
	}
	return changed
}

// scan lists the usable interfaces, restricted to names when given. Without names it leaves out the virtual
// ones, unless that would leave none, and returns those it left out, with the reason.
func scan(names []string) (out []iface, skipped []string) {
	all, _ := net.Interfaces()
	var virtual map[int]string
	if len(names) == 0 {
		virtual = netif.Virtual()
	}
	var virt []iface
	for _, ni := range all {
		if ni.Flags&net.FlagUp == 0 || ni.Flags&net.FlagMulticast == 0 || ni.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(names) > 0 && !slices.Contains(names, ni.Name) {
			continue
		}
		i := iface{name: ni.Name, index: ni.Index}
		addrs, _ := ni.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				i.ips = append(i.ips, n.IP.To4())
				i.nets = append(i.nets, n)
			}
		}
		if len(i.ips) == 0 {
			continue
		}
		if r, ok := virtual[ni.Index]; ok {
			virt = append(virt, i)
			skipped = append(skipped, ni.Name+": "+r)
			continue
		}
		out = append(out, i)
	}
	if len(out) == 0 {
		return virt, nil
	}
	return out, skipped
}

// ifaceFor finds the served interface whose subnet holds src: mDNS is link-local, so that is where the query
// came from. Queries from anywhere else (other VLANs, the WAN) are not answered.
func (r *Responder) ifaceFor(src net.IP) (iface, bool) {
	for _, i := range r.current() {
		for _, n := range i.nets {
			if n.Contains(src) {
				return i, true
			}
		}
	}
	return iface{}, false
}

func (r *Responder) read() {
	buf := make([]byte, 9000)
	for {
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			r.log.Debug("mdns read", "err", err)
			continue
		}
		q, err := parseQuery(buf[:n])
		if err != nil {
			continue // responses, including our own looped back
		}
		i, ok := r.ifaceFor(src.IP)
		if !ok {
			continue
		}
		answers, extra, unicast := r.svc.answer(q, i.ips)
		if len(answers) == 0 {
			continue
		}
		r.log.Debug("mdns query", "from", src, "iface", i.name, "questions", len(q.questions), "unicast", unicast)
		switch {
		case src.Port != port: // legacy unicast query (RFC 6762 section 6.7)
			r.conn.WriteToUDP(encodeResponse(q.id, q.questions, answers, extra, true, false), src)
		case unicast:
			r.conn.WriteToUDP(encodeResponse(0, nil, answers, extra, false, false), src)
		default:
			r.multicast(i, encodeResponse(0, nil, answers, extra, false, false))
		}
	}
}

func (r *Responder) multicast(i iface, pkt []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := control(r.raw, func(fd uintptr) error { return setIf(fd, i) })
	if err == nil {
		_, err = r.conn.WriteToUDP(pkt, &net.UDPAddr{IP: group, Port: port})
	}
	if err != nil {
		r.log.Debug("mdns send", "iface", i.name, "err", err)
	}
}

// control runs f on the socket's descriptor and returns f's error.
func control(raw syscall.RawConn, f func(fd uintptr) error) error {
	var ferr error
	if err := raw.Control(func(fd uintptr) { ferr = f(fd) }); err != nil {
		return err
	}
	return ferr
}
