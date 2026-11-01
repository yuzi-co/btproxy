// Command btproxy is an ESPHome-compatible Bluetooth proxy for Linux: Home Assistant adds it like an ESP32
// proxy (ESPHome integration, port 6053) and uses the host's Bluetooth adapter through it.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"btproxy/internal/api"
	"btproxy/internal/ble"
	"btproxy/internal/ble/fake"
	"btproxy/internal/mdns"
)

var version = "0.1.0"

// esphomeVersion is the ESPHome release btproxy reports itself as (DeviceInfo and mDNS).
const esphomeVersion = "2026.10.0"

// backends maps -backend names to constructors; bluez registers itself on Linux.
var backends = map[string]func(adapter string) (ble.Adapter, error){
	"fake": func(string) (ble.Adapter, error) { return &fake.Adapter{}, nil },
}

// Flags read by the bluez backend.
var (
	useAgent          bool
	stallTimeout      time.Duration
	bluetoothdRestart string
	rawAdverts        string
)

func main() {
	host, _ := os.Hostname()
	name := flag.String("name", "btproxy-"+strings.ToLower(strings.Split(host, ".")[0]), "node name shown in Home Assistant")
	friendly := flag.String("friendly-name", "", "friendly name (default: the node name)")
	area := flag.String("area", "", "suggested area")
	listen := flag.String("listen", ":6053", "API listen address")
	backend := flag.String("backend", "bluez", "Bluetooth backend: bluez or fake")
	adapterName := flag.String("adapter", "hci0", "Bluetooth adapter (bluez backend)")
	maxConns := flag.Int("connections", 3, "maximum simultaneous peripheral connections")
	scanMode := flag.String("scan-mode", "active", "scanner mode: active or passive (bluez: needs bluetoothd --experimental, else active)")
	mac := flag.String("mac", "", "MAC reported as the device's unique id (default: first network interface)")
	mdnsOn := flag.Bool("mdns", true, "advertise the API over mDNS so Home Assistant discovers it")
	mdnsIface := flag.String("mdns-iface", "", "comma-separated interfaces to advertise on (default: all with IPv4)")
	debug := flag.Bool("debug", false, "debug logging")
	flag.BoolVar(&useAgent, "agent", true, "register a BlueZ pairing agent (bluez backend)")
	flag.DurationVar(&stallTimeout, "stall-timeout", 3*time.Minute,
		"restart active scanning after this long without advertisements, 0 to disable (bluez backend)")
	flag.StringVar(&bluetoothdRestart, "bluetoothd-restart", "",
		"shell command that restarts bluetoothd when scanning keeps stalling or it stops answering (bluez backend)")
	flag.StringVar(&rawAdverts, "raw-adverts", "auto",
		"read advertisements from the HCI monitor: auto, on or off (bluez backend; needs CAP_NET_RAW)")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	// Everything logged also goes to API clients that subscribe to logs, at the level each asks for.
	logs := &api.Logs{}
	log := slog.New(logs.Handler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	slog.SetDefault(log)

	scan := map[string]ble.ScanMode{"active": ble.ScanActive, "passive": ble.ScanPassive}
	mode, ok := scan[*scanMode]
	if !ok {
		fatal(log, "unknown scan mode", "scan-mode", *scanMode)
	}
	newAdapter, ok := backends[*backend]
	if !ok {
		fatal(log, "unknown backend", "backend", *backend)
	}
	adapter, err := newAdapter(*adapterName)
	if err != nil {
		fatal(log, "open adapter", "err", err)
	}
	if *mac == "" {
		*mac = hostMAC(adapter)
	}
	if *friendly == "" {
		*friendly = *name
	}

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		fatal(log, "listen", "err", err)
	}
	log.Info("btproxy started", "version", version, "listen", l.Addr().String(), "name", *name,
		"bt_mac", ble.FormatMAC(adapter.MAC()))
	if *mdnsOn {
		startMDNS(log, *name, *friendly, *mac, l.Addr().(*net.TCPAddr).Port, *mdnsIface)
	}
	srv := api.NewServer(api.Config{
		Name: *name, FriendlyName: *friendly, MAC: *mac, SuggestedArea: *area,
		Version: esphomeVersion, ProjectVersion: version, MaxConnections: *maxConns, ScanMode: mode,
		Logs: logs, Backend: *backend, Adapter: *adapterName,
	}, adapter, log)
	fatal(log, "serve", "err", srv.Serve(l))
}

// startMDNS advertises the API like an ESPHome device and, on SIGINT or SIGTERM, withdraws the advertisement
// before exiting. A failure only disables discovery.
func startMDNS(log *slog.Logger, name, friendly, mac string, port int, ifaces string) {
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
		"platform=linux",
		"board=" + runtime.GOARCH,
		"network=ethernet",
		"project_name=yuzi.btproxy", // as DeviceInfo reports it
		"project_version=" + version,
	}
	r, err := mdns.Start(mdns.Config{Name: mdns.Label(name), Port: port, TXT: txt, Ifaces: names}, log)
	if err != nil {
		log.Warn("mdns disabled", "err", err)
		return
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		log.Info("stopping", "signal", <-sig)
		r.Close()
		os.Exit(0)
	}()
}

// hostMAC picks the first up, non-loopback interface with a hardware address; the adapter MAC is the fallback.
func hostMAC(a ble.Adapter) string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp != 0 && i.Flags&net.FlagLoopback == 0 && len(i.HardwareAddr) == 6 {
			return strings.ToUpper(i.HardwareAddr.String())
		}
	}
	return ble.FormatMAC(a.MAC())
}

func fatal(log *slog.Logger, msg string, args ...any) {
	log.Error(msg, args...)
	fmt.Fprintln(os.Stderr, "exiting")
	os.Exit(1)
}
