// Command btproxy is an ESPHome-compatible Bluetooth proxy for Linux: Home Assistant adds it like an ESP32
// proxy (ESPHome integration, port 6053) and uses the host's Bluetooth adapter through it.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"btproxy/internal/api"
	"btproxy/internal/ble"
	"btproxy/internal/ble/fake"
)

var version = "0.1.0"

// backends maps -backend names to constructors; bluez registers itself on Linux.
var backends = map[string]func(adapter string) (ble.Adapter, error){
	"fake": func(string) (ble.Adapter, error) { return fake.Adapter{}, nil },
}

func main() {
	host, _ := os.Hostname()
	name := flag.String("name", "btproxy-"+strings.ToLower(strings.Split(host, ".")[0]), "node name shown in Home Assistant")
	friendly := flag.String("friendly-name", "", "friendly name (default: the node name)")
	area := flag.String("area", "", "suggested area")
	listen := flag.String("listen", ":6053", "API listen address")
	backend := flag.String("backend", "bluez", "Bluetooth backend: bluez or fake")
	adapterName := flag.String("adapter", "hci0", "Bluetooth adapter (bluez backend)")
	maxConns := flag.Int("connections", 3, "maximum simultaneous peripheral connections")
	mac := flag.String("mac", "", "MAC reported as the device's unique id (default: first network interface)")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

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
	srv := api.NewServer(api.Config{
		Name: *name, FriendlyName: *friendly, MAC: *mac, SuggestedArea: *area,
		Version: "2026.10.0", ProjectVersion: version, MaxConnections: *maxConns,
	}, adapter, log)
	fatal(log, "serve", "err", srv.Serve(l))
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
