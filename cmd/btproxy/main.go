// Command btproxy is an ESPHome-compatible Bluetooth proxy for Linux and Windows: Home Assistant adds it like
// an ESP32 proxy (ESPHome integration, port 6053) and uses the host's Bluetooth adapter through it.
package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/yuzi-co/btproxy/internal/api"
	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/ble/fake"
	"github.com/yuzi-co/btproxy/internal/logx"
	"github.com/yuzi-co/btproxy/internal/mdns"
	"github.com/yuzi-co/btproxy/internal/netif"
)

// version is set at build time for releases. Reading the module's version instead, for go install, would add
// 64 KB for runtime/debug.
var version = "dev"

// esphomeVersion is the ESPHome release btproxy reports itself as (DeviceInfo and mDNS).
const esphomeVersion = "2026.10.0"

// defaultBackend is the -backend default; winrt makes itself the default on Windows, and a build without BlueZ
// makes kernel the default.
var defaultBackend = "auto"

// backends maps -backend names to constructors; bluez and kernel register themselves on Linux, winrt on Windows.
var backends = map[string]func(adapter string, log *logx.Logger) (ble.Adapter, error){
	"fake": func(name string, _ *logx.Logger) (ble.Adapter, error) {
		return &fake.Adapter{Addr: fakeAddr(name)}, nil
	},
}

// autoBackend picks the backend for -backend auto; set on Linux, the only place with a choice.
var autoBackend func(log *logx.Logger) string

// Flag values the backend constructors read (bluez_linux.go, kernel_linux.go, winrt_windows.go); the settings
// table says which backend each is for.
var (
	bondDir           string
	useAgent          bool
	stallTimeout      time.Duration
	usbReset          bool
	bluetoothdRestart string
	rawAdverts        string
	scanPause         bool
	connectRetries    int
	connectRetryDelay time.Duration
)

// setBackendOptions copies the flags the backends read into the variables above, once the settings file is
// applied.
func setBackendOptions(a *app) {
	bondDir = a.str("bond-dir")
	useAgent = a.boolean("agent")
	stallTimeout = a.duration("stall-timeout")
	usbReset = a.boolean("usb-reset")
	bluetoothdRestart = a.str("bluetoothd-restart")
	rawAdverts = a.str("raw-adverts")
	scanPause = a.boolean("scan-pause-on-connect")
	connectRetries = a.integer("connect-retries")
	connectRetryDelay = a.duration("connect-retry-delay")
}

func main() {
	a, err := newApp(flag.NewFlagSet("btproxy", flag.ExitOnError), os.Args[1:])
	if err != nil {
		fatal(a.log, "settings file", "path", a.configPath, "err", err)
	}
	a.run()
}

// run serves the proxy until it stops.
func (a *app) run() {
	log := a.log
	if a.configErr != nil {
		log.Error("settings file", "path", a.configPath, "err", a.configErr)
	}
	listen := a.str("listen")
	oneInstance(log, listen)
	uiPassword, err := uiPassword(a.str("ui-password"), a.str("ui-password-file"), os.Getenv("BTPROXY_UI_PASSWORD"))
	if err != nil {
		fatal(log, "ui password", "err", err)
	}

	scan := map[string]ble.ScanMode{"active": ble.ScanActive, "passive": ble.ScanPassive}
	mode, ok := scan[a.str("scan-mode")]
	if !ok {
		fatal(log, "unknown scan mode", "scan-mode", a.str("scan-mode"))
	}
	key, err := encryptionKey(a.str("encryption-key"), a.str("encryption-key-file"), os.Getenv("BTPROXY_ENCRYPTION_KEY"))
	if err != nil {
		fatal(log, "encryption key", "err", err)
	}
	if key != nil && !api.Encryption {
		fatal(log, "encryption key set, but this build has no encryption (built with -tags nonoise)")
	}
	advFilter, err := ble.ParseAdvFilter(a.str("adv-allow"), a.str("adv-deny"), a.boolean("adv-deny-random"))
	if err != nil {
		fatal(log, "advertisement filter", "err", err)
	}
	backend := a.str("backend")
	if backend == "auto" && autoBackend != nil {
		backend = autoBackend(log)
		a.fs.Set("backend", backend) // the settings page and the sensors go by the flag's value
	}
	newAdapter, ok := backends[backend]
	if !ok {
		fatal(log, "unknown backend", "backend", backend)
	}
	if advFilter != nil {
		log.Info("advertisement filter", "filter", advFilter.String())
	}
	nodes, err := planNodes(a.str("adapter"), listen, a.str("name"), a.str("friendly-name"))
	if err != nil {
		fatal(log, "adapters", "err", err)
	}
	var responders []*mdns.Responder
	macs := map[string]string{}
	served := make(chan error, len(nodes))
	for i, n := range nodes {
		adapter, err := newAdapter(n.adapter, log)
		if err != nil && i > 0 {
			// A missing extra adapter must not take the others down; restart the proxy once it is back.
			log.Error("open adapter, not serving it", "adapter", n.adapter, "err", err)
			continue
		} else if err != nil {
			fatal(log, "open adapter", "adapter", n.adapter, "err", err)
		}
		m := a.str("mac")
		if i > 0 {
			m = ble.FormatMAC(adapter.MAC()) // the first keeps the unique id it had with one adapter
		} else if m == "" {
			m = hostMAC(adapter)
		}
		if other, dup := macs[m]; dup {
			fatal(log, "two adapters with the same MAC", "mac", m, "adapters", other+","+n.adapter)
		}
		macs[m] = n.adapter

		l, err := net.Listen("tcp", n.listen)
		if err != nil {
			fatal(log, "listen", "adapter", n.adapter, "err", err)
		}
		if key == nil && !loopbackAddr(l.Addr().String()) {
			log.Warn(notEncryptedWarning, "listen", l.Addr().String())
		}
		log.Info("btproxy started", "version", version, "listen", l.Addr().String(), "name", n.name,
			"adapter", n.adapter, "bt_mac", ble.FormatMAC(adapter.MAC()), "encryption", key != nil)
		if a.boolean("mdns") {
			if r := startMDNS(log, n.name, n.friendly, m, l.Addr().(*net.TCPAddr).Port, a.str("mdns-iface"), key != nil); r != nil {
				responders = append(responders, r)
			}
		}
		nlog := log
		if len(nodes) > 1 {
			nlog = log.With("node", n.name)
		}
		srv := api.NewServer(api.Config{
			Name: n.name, FriendlyName: n.friendly, MAC: m, SuggestedArea: a.str("area"),
			Version: esphomeVersion, ProjectVersion: version, MaxConnections: a.integer("connections"), ScanMode: mode,
			Logs: a.logs, Backend: backend, Adapter: n.adapter,
			EncryptionKey: key, AdvFilter: advFilter,
			Restart: func() { a.stop("restart requested through the API", true) },
			LastProblem: func() string {
				line, _ := a.problems.lastProblem()
				return line
			},
		}, adapter, nlog)
		a.addNode(n.name, adapter, l, srv)
		go func() { served <- srv.Serve(l) }()
	}
	a.mu.Lock()
	a.responders = responders
	a.mu.Unlock()
	if err := a.startUI(a.str("ui"), uiPassword, tokenLogin); err != nil {
		fatal(log, "settings page", "err", err)
	}
	go a.stopOnSignal()
	startTray(a)
	fatal(log, "serve", "err", <-served)
}

// startMDNS advertises the API like an ESPHome device. A failure only disables discovery and returns nil.
func startMDNS(log *logx.Logger, name, friendly, mac string, port int, ifaces string, encrypted bool) *mdns.Responder {
	var names []string
	for _, n := range strings.Split(ifaces, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	// TXT keys as ESPHome's mdns component sends them; Home Assistant matches mac (lower case hex, no
	// separators) against the unique id of the existing entry, which comes from DeviceInfo's MAC.
	txt := []string{
		"friendly_name=" + friendly,
		"version=" + esphomeVersion,
		"mac=" + strings.ToLower(strings.NewReplacer(":", "", "-", "").Replace(mac)),
		"platform=" + runtime.GOOS,
		"board=" + runtime.GOARCH,
		"network=ethernet",
		"project_name=yuzi.btproxy", // as DeviceInfo reports it
		"project_version=" + version,
	}
	if encrypted {
		txt = append(txt, "api_encryption="+api.NoiseProtocol)
	}
	r, err := mdns.Start(mdns.Config{Name: mdns.Label(name), Port: port, TXT: txt, Ifaces: names}, log)
	if err != nil {
		log.Warn("mdns disabled", "name", name, "err", err)
		return nil
	}
	return r
}

// stopOnSignal stops the proxy on SIGINT or SIGTERM the way the settings page and the tray icon do: the mDNS
// advertisements are withdrawn and the tray icon removed.
func (a *app) stopOnSignal() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	a.stop("signal "+(<-sig).String(), false)
}

// encryptionKey decodes the API key from the flag, the key file or the environment, in that order of
// preference. No key means a plaintext API.
func encryptionKey(flagValue, file, env string) ([]byte, error) {
	if flagValue != "" && file != "" {
		return nil, errors.New("-encryption-key and -encryption-key-file are both set")
	}
	s := flagValue
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if s = strings.TrimSpace(string(b)); s == "" {
			return nil, fmt.Errorf("%s is empty", file)
		}
	}
	if s == "" {
		s = env
	}
	if s = strings.TrimSpace(s); s == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(key) != 32 {
		return nil, errors.New("not base64 of 32 bytes") // the key itself stays out of the log
	}
	return key, nil
}

// hostMAC picks the first up, non-loopback interface with a hardware address; the adapter MAC is the fallback.
func hostMAC(a ble.Adapter) string {
	ifs, _ := net.Interfaces()
	var virtual net.HardwareAddr
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || len(i.HardwareAddr) != 6 {
			continue
		}
		if !netif.VirtualMAC(i.HardwareAddr) {
			return strings.ToUpper(i.HardwareAddr.String())
		}
		if virtual == nil {
			virtual = i.HardwareAddr
		}
	}
	if virtual != nil {
		return strings.ToUpper(virtual.String())
	}
	return ble.FormatMAC(a.MAC())
}

func fatal(log *logx.Logger, msg string, args ...any) {
	log.Error(msg, args...)
	platformFatal(msg, args)
	fmt.Fprintln(os.Stderr, "exiting")
	os.Exit(1)
}
