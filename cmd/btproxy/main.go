// Command btproxy is an ESPHome-compatible Bluetooth proxy for Linux: Home Assistant adds it like an ESP32
// proxy (ESPHome integration, port 6053) and uses the host's Bluetooth adapter through it.
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

	"btproxy/internal/api"
	"btproxy/internal/ble"
	"btproxy/internal/ble/fake"
	"btproxy/internal/logx"
	"btproxy/internal/mdns"
)

var version = "0.1.0"

// esphomeVersion is the ESPHome release btproxy reports itself as (DeviceInfo and mDNS).
const esphomeVersion = "2026.10.0"

// backends maps -backend names to constructors; bluez registers itself on Linux.
var backends = map[string]func(adapter string, log *logx.Logger) (ble.Adapter, error){
	"fake": func(string, *logx.Logger) (ble.Adapter, error) { return &fake.Adapter{}, nil },
}

// Flags read by the bluez backend.
var (
	useAgent          bool
	stallTimeout      time.Duration
	bluetoothdRestart string
	rawAdverts        string
	scanPause         bool
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
	keyB64 := flag.String("encryption-key", "",
		"API encryption key, base64 of 32 bytes as in ESPHome's api: encryption: key: (visible in ps; prefer -encryption-key-file or $BTPROXY_ENCRYPTION_KEY)")
	keyFile := flag.String("encryption-key-file", "", "file holding the API encryption key")
	advAllow := flag.String("adv-allow", "",
		"forward only advertisements matching one of these comma-separated entries: MAC, OUI or prefix (AA:BB:CC, AA:BB:CC:DD:*), uuid:fe95, mfr:0x004c (default: all)")
	advDeny := flag.String("adv-deny", "", "drop advertisements matching one of these entries (as -adv-allow; deny wins)")
	advDenyRandom := flag.Bool("adv-deny-random", false,
		"drop advertisements from private (resolvable and non-resolvable) random addresses, phones included")
	debug := flag.Bool("debug", false, "debug logging")
	flag.BoolVar(&useAgent, "agent", true, "register a BlueZ pairing agent (bluez backend)")
	flag.DurationVar(&stallTimeout, "stall-timeout", 3*time.Minute,
		"restart active scanning after this long without advertisements, 0 to disable (bluez backend)")
	flag.StringVar(&bluetoothdRestart, "bluetoothd-restart", "",
		"shell command that restarts bluetoothd when scanning keeps stalling or it stops answering (bluez backend)")
	flag.StringVar(&rawAdverts, "raw-adverts", "auto",
		"read advertisements from the HCI monitor: auto, on or off (bluez backend; needs CAP_NET_RAW)")
	flag.BoolVar(&scanPause, "scan-pause-on-connect", true,
		"stop scanning while a connection is being made, like ESPHome (bluez backend)")
	flag.Parse()

	level := logx.LevelInfo
	if *debug {
		level = logx.LevelDebug
	}
	// Everything logged also goes to API clients that subscribe to logs, at the level each asks for.
	logs := &api.Logs{}
	log := logx.New(os.Stderr, level, logs)

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
	adapter, err := newAdapter(*adapterName, log)
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
		"bt_mac", ble.FormatMAC(adapter.MAC()), "encryption", key != nil)
	if advFilter != nil {
		log.Info("advertisement filter", "filter", advFilter.String())
	}
	if *mdnsOn {
		startMDNS(log, *name, *friendly, *mac, l.Addr().(*net.TCPAddr).Port, *mdnsIface, key != nil)
	}
	srv := api.NewServer(api.Config{
		Name: *name, FriendlyName: *friendly, MAC: *mac, SuggestedArea: *area,
		Version: esphomeVersion, ProjectVersion: version, MaxConnections: *maxConns, ScanMode: mode,
		Logs: logs, Backend: *backend, Adapter: *adapterName,
		EncryptionKey: key, AdvFilter: advFilter,
	}, adapter, log)
	fatal(log, "serve", "err", srv.Serve(l))
}

// startMDNS advertises the API like an ESPHome device and, on SIGINT or SIGTERM, withdraws the advertisement
// before exiting. A failure only disables discovery.
func startMDNS(log *logx.Logger, name, friendly, mac string, port int, ifaces string, encrypted bool) {
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
	if encrypted {
		txt = append(txt, "api_encryption="+api.NoiseProtocol)
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
	for _, i := range ifs {
		if i.Flags&net.FlagUp != 0 && i.Flags&net.FlagLoopback == 0 && len(i.HardwareAddr) == 6 {
			return strings.ToUpper(i.HardwareAddr.String())
		}
	}
	return ble.FormatMAC(a.MAC())
}

func fatal(log *logx.Logger, msg string, args ...any) {
	log.Error(msg, args...)
	fmt.Fprintln(os.Stderr, "exiting")
	os.Exit(1)
}
