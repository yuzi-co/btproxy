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
	"sync/atomic"
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

	// write sends pkt to dst, or to the group on i when dst is nil. Tests replace it with a fake.
	write func(i iface, dst *net.UDPAddr, pkt []byte) error

	mu     sync.Mutex // guards ifaces, joined and sent, and pairs IP_MULTICAST_IF with the write that depends on it
	ifaces []iface
	joined map[string]string // interface name -> String() when the group was joined on it
	// sent is when each record last went out, keyed by destination (an interface name for multicast, an address
	// for unicast) and record, for the one-per-second limit of RFC 6762 section 6. Entries older than a second
	// are swept on use.
	sent map[string]time.Time

	stop      chan struct{}
	closed    atomic.Bool // set first thing in Close: only goodbyes may be multicast from then on
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
	r := newResponder(newService(cfg.Name, cfg.Port, cfg.TXT), log, conn)
	r.names, r.raw = cfg.Ifaces, raw
	r.write = r.writeUDP
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

func newResponder(svc service, log *logx.Logger, conn *net.UDPConn) *Responder {
	return &Responder{svc: svc, log: log, conn: conn, joined: map[string]string{}, sent: map[string]time.Time{},
		stop: make(chan struct{})}
}

// Close sends goodbye packets (TTL 0) so browsers drop the service at once, then closes the socket. An
// announcement in flight cannot follow the goodbyes: closed stops every other multicast first.
func (r *Responder) Close() {
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		close(r.stop)
		for _, i := range r.current() {
			r.multicast(i, encodeResponse(0, nil, r.svc.all(i.ips), nil, false, true), true)
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

// announce multicasts the full record set announceN times, a second apart. Announcements are exempt from the
// rate limit but count as the records' last multicast.
func (r *Responder) announce() {
	for n := range announceN {
		if n > 0 {
			select {
			case <-r.stop:
				return
			case <-time.After(time.Second):
			}
		}
		now := time.Now()
		for _, i := range r.current() {
			rs := r.svc.all(i.ips)
			r.mark(i.name, rs, now)
			r.multicast(i, encodeResponse(0, nil, rs, nil, false, false), false)
		}
	}
}

// mark records rs as sent to dest at now.
func (r *Responder) mark(dest string, rs []record, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range rs {
		r.sent[dest+"|"+rec.key()] = now
	}
}

// limit drops the records sent to dest within the last second and marks the rest as sent now. extra is left
// empty when nothing of answers remains, since a response needs an answer.
func (r *Responder) limit(dest string, answers, extra []record, now time.Time) ([]record, []record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, t := range r.sent {
		if now.Sub(t) >= time.Second {
			delete(r.sent, k)
		}
	}
	fresh := func(rec record) bool { _, ok := r.sent[dest+"|"+rec.key()]; return !ok }
	answers = slices.DeleteFunc(slices.Clone(answers), func(rec record) bool { return !fresh(rec) })
	if len(answers) == 0 {
		return nil, nil
	}
	extra = slices.DeleteFunc(slices.Clone(extra), func(rec record) bool { return !fresh(rec) })
	for _, rec := range slices.Concat(answers, extra) {
		r.sent[dest+"|"+rec.key()] = now
	}
	return answers, extra
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
		r.handle(buf[:n], src, time.Now())
	}
}

// handle answers the query in b from src, received at now. Records the querier already knows, and records that
// went to the same destination within the last second, are left out; the answer is dropped when nothing is left.
func (r *Responder) handle(b []byte, src *net.UDPAddr, now time.Time) {
	i, ok := r.ifaceFor(src.IP)
	if !ok {
		return
	}
	q, err := parseQuery(b)
	if err != nil {
		return // responses, including our own looped back
	}
	answers, extra, unicast := r.svc.answer(q, i.ips)
	if len(answers) == 0 {
		return
	}
	legacy := src.Port != port // legacy unicast query (RFC 6762 section 6.7)
	dest := i.name
	if legacy || unicast {
		dest = src.String()
	}
	if answers, extra = r.limit(dest, answers, extra, now); len(answers) == 0 {
		return
	}
	r.log.Debug("mdns query", "from", src, "iface", i.name, "questions", len(q.questions), "unicast", unicast)
	switch {
	case legacy:
		r.unicast(encodeResponse(q.id, q.questions, answers, extra, true, false), src)
	case unicast:
		r.unicast(encodeResponse(0, nil, answers, extra, false, false), src)
	default:
		r.multicast(i, encodeResponse(0, nil, answers, extra, false, false), false)
	}
}

func (r *Responder) unicast(pkt []byte, dst *net.UDPAddr) {
	if err := r.write(iface{}, dst, pkt); err != nil {
		r.log.Debug("mdns send", "to", dst, "err", err)
	}
}

// multicast sends pkt to the group on i. Once Close has begun only goodbyes go out, so that a late announcement
// cannot follow a goodbye and leave the service in browsers' caches for its TTL.
func (r *Responder) multicast(i iface, pkt []byte, goodbye bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() && !goodbye {
		return
	}
	if err := r.write(i, nil, pkt); err != nil {
		r.log.Debug("mdns send", "iface", i.name, "err", err)
	}
}

// writeUDP is the write of a real socket. A multicast first selects the outgoing interface, which is why the
// caller holds mu.
func (r *Responder) writeUDP(i iface, dst *net.UDPAddr, pkt []byte) error {
	if dst == nil {
		if err := control(r.raw, func(fd uintptr) error { return setIf(fd, i) }); err != nil {
			return err
		}
		dst = &net.UDPAddr{IP: group, Port: port}
	}
	_, err := r.conn.WriteToUDP(pkt, dst)
	return err
}

// control runs f on the socket's descriptor and returns f's error.
func control(raw syscall.RawConn, f func(fd uintptr) error) error {
	var ferr error
	if err := raw.Control(func(fd uintptr) { ferr = f(fd) }); err != nil {
		return err
	}
	return ferr
}
