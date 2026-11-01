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

var version = "0.1.0"

// esphomeVersion is the ESPHome release btproxy reports itself as (DeviceInfo and mDNS).
const esphomeVersion = "2026.10.0"

// defaultBackend is the -backend default; the winrt backend makes itself the default on Windows.
var defaultBackend = "bluez"

// backends maps -backend names to constructors; bluez registers itself on Linux, winrt on Windows.
var backends = map[string]func(adapter string, log *logx.Logger) (ble.Adapter, error){
	"fake": func(name string, _ *logx.Logger) (ble.Adapter, error) {
		return &fake.Adapter{Addr: fakeAddr(name)}, nil
	},
}

// Flags read by the bluez backend; scan-pause-on-connect and connect-retries also by winrt.
var (
	useAgent          bool
	stallTimeout      time.Duration
	bluetoothdRestart string
	rawAdverts        string
	scanPause         bool
	connectRetries    int
)

func main() {
	host, _ := os.Hostname()
	name := flag.String("name", "btproxy-"+strings.ToLower(strings.Split(host, ".")[0]), "node name shown in Home Assistant")
	friendly := flag.String("friendly-name", "", "friendly name (default: the node name)")
	area := flag.String("area", "", "suggested area")
	listen := flag.String("listen", ":6053", "API listen address; with several adapters, the first of consecutive ports, or a comma-separated list")
	backend := flag.String("backend", defaultBackend, "Bluetooth backend: bluez, winrt (Windows) or fake")
	adapterName := flag.String("adapter", "hci0", "Bluetooth adapter, or a comma-separated list served as one device each (bluez backend)")
	maxConns := flag.Int("connections", 3, "maximum simultaneous peripheral connections")
	scanMode := flag.String("scan-mode", "active", "scanner mode: active or passive (bluez: needs bluetoothd --experimental, else active)")
	mac := flag.String("mac", "", "MAC reported as the device's unique id (default: first network interface; further adapters: their own address)")
	mdnsOn := flag.Bool("mdns", true, "advertise the API over mDNS so Home Assistant discovers it")
	mdnsIface := flag.String("mdns-iface", "", "comma-separated interfaces to advertise on (default: all with IPv4)")
	keyB64 := flag.String("encryption-key", "",
		"API encryption key, base64 of 32 bytes as in ESPHome's api: encryption: key: (visible in ps; prefer -encryption-key-file or $BTPROXY_ENCRYPTION_KEY)")
	keyFile := flag.String("encryption-key-file", "", "file holding the API encryption key")
	advAllow := flag.String("adv-allow", "",
		"forward only advertisements matching one of these comma-separated entries: MAC, OUI or prefix (AA:BB:CC, AA:BB:CC:DD:*), uuid:fe95, mfr:0x004c (default: all)")
	advDeny := flag.String("adv-deny", "", "drop advertisements matching one of these entries (as -adv-allow; deny wins)")
	advDenyRandom := flag.Bool("adv-deny-random", false,
		"drop advertisements from private (resolvable and non-resolvable) random addresses, phones included")
	debug := flag.Bool("debug", false, "debug logging")
	configFile := flag.String("config", defaultConfig,
		"settings file (flat JSON of flag values); flags on the command line override it, the settings page saves it")
	uiAddr := flag.String("ui", defaultUI, "serve the settings page on this address, such as :8080 (needs -ui-password, except on loopback on Windows)")
	uiPass := flag.String("ui-password", "", "settings page password, any user name (prefer -ui-password-file or $BTPROXY_UI_PASSWORD)")
	uiPassFile := flag.String("ui-password-file", "", "file holding the settings page password")
	flag.BoolVar(&useAgent, "agent", true, "register a BlueZ pairing agent (bluez backend)")
	flag.DurationVar(&stallTimeout, "stall-timeout", 3*time.Minute,
		"restart active scanning after this long without advertisements, 0 to disable (bluez backend)")
	flag.StringVar(&bluetoothdRestart, "bluetoothd-restart", "",
		"shell command that restarts bluetoothd when scanning keeps stalling or it stops answering (bluez backend)")
	flag.StringVar(&rawAdverts, "raw-adverts", "auto",
		"read advertisements from the HCI monitor: auto, on or off (bluez backend; needs CAP_NET_RAW)")
	flag.BoolVar(&scanPause, "scan-pause-on-connect", true,
		"stop scanning while a connection is being made, like ESPHome (bluez and winrt backends)")
	flag.IntVar(&connectRetries, "connect-retries", 2,
		"retry a connect that fails at once on the radio (HCI 0x3e) this many times, 0 to disable (bluez and winrt backends)")
	flag.Parse()
	settingsErr := loadSettings(*configFile)

	level := logx.LevelInfo
	if *debug {
		level = logx.LevelDebug
	}
	// Everything logged also goes to API clients that subscribe to logs, at the level each asks for.
	logs := &api.Logs{}
	app.problems.w = logOutput()
	log := logx.New(&app.problems, level, logs)
	app.log = log
	if settingsErr != nil {
		fatal(log, "settings file", "path", *configFile, "err", settingsErr)
	}
	if app.configErr != nil {
		log.Error("settings file", "path", *configFile, "err", app.configErr)
	}
	oneInstance(log, *listen)
	uiPassword, err := uiPassword(*uiPass, *uiPassFile, os.Getenv("BTPROXY_UI_PASSWORD"))
	if err != nil {
		fatal(log, "ui password", "err", err)
	}

	scan := map[string]ble.ScanMode{"active": ble.ScanActive, "passive": ble.ScanPassive}
	mode, ok := scan[*scanMode]
	if !ok {
		fatal(log, "unknown scan mode", "scan-mode", *scanMode)
	}
	key, err := encryptionKey(*keyB64, *keyFile, os.Getenv("BTPROXY_ENCRYPTION_KEY"))
	if err != nil {
		fatal(log, "encryption key", "err", err)
	}
	if key != nil && !api.Encryption {
		fatal(log, "encryption key set, but this build has no encryption (built with -tags nonoise)")
	}
	advFilter, err := ble.ParseAdvFilter(*advAllow, *advDeny, *advDenyRandom)
	if err != nil {
		fatal(log, "advertisement filter", "err", err)
	}
	newAdapter, ok := backends[*backend]
	if !ok {
		fatal(log, "unknown backend", "backend", *backend)
	}
	if advFilter != nil {
		log.Info("advertisement filter", "filter", advFilter.String())
	}
	nodes, err := planNodes(*adapterName, *listen, *name, *friendly)
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
		m := *mac
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
			log.Warn("API is not encrypted: any host that can reach it can use the proxy and its bonded devices; "+
				"see -encryption-key-file", "listen", l.Addr().String())
		}
		log.Info("btproxy started", "version", version, "listen", l.Addr().String(), "name", n.name,
			"adapter", n.adapter, "bt_mac", ble.FormatMAC(adapter.MAC()), "encryption", key != nil)
		if *mdnsOn {
			if r := startMDNS(log, n.name, n.friendly, m, l.Addr().(*net.TCPAddr).Port, *mdnsIface, key != nil); r != nil {
				responders = append(responders, r)
			}
		}
		nlog := log
		if len(nodes) > 1 {
			nlog = log.With("node", n.name)
		}
		srv := api.NewServer(api.Config{
			Name: n.name, FriendlyName: n.friendly, MAC: m, SuggestedArea: *area,
			Version: esphomeVersion, ProjectVersion: version, MaxConnections: *maxConns, ScanMode: mode,
			Logs: logs, Backend: *backend, Adapter: n.adapter,
			EncryptionKey: key, AdvFilter: advFilter,
		}, adapter, nlog)
		addNode(n.name, adapter, l, srv)
		go func() { served <- srv.Serve(l) }()
	}
	if len(responders) > 0 {
		go withdrawOnSignal(log, responders)
	}
	if err := startUI(log, responders, *uiAddr, uiPassword, uiToken); err != nil {
		fatal(log, "settings page", "err", err)
	}
	startTray(log)
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

// withdrawOnSignal withdraws the mDNS advertisements on SIGINT or SIGTERM and exits.
func withdrawOnSignal(log *logx.Logger, rs []*mdns.Responder) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	log.Info("stopping", "signal", <-sig)
	for _, r := range rs {
		r.Close()
	}
	os.Exit(0)
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
