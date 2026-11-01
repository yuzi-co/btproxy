package main

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/yuzi-co/btproxy/internal/api"
	"github.com/yuzi-co/btproxy/internal/ble"
	"github.com/yuzi-co/btproxy/internal/logx"
	"github.com/yuzi-co/btproxy/internal/mdns"
	"github.com/yuzi-co/btproxy/internal/ui"
)

// The settings file (-config) holds flag values as a flat JSON object, {"name": "btproxy-office", ...}.
// Flags given on the command line override it. The settings page (-ui) edits it and restarts the proxy.

// setting is a flag the settings file and page cover.
type setting struct {
	name, label, help string
	kind              ui.Kind
	options           []string
	backend           string // shown only with these comma-separated backends; "" for all
	readOnly          bool   // shown, but changed only in the settings file or on the command line
}

var settings = []struct {
	title string
	list  []setting
}{
	{"Device", []setting{
		{name: "name", label: "Name", help: "Node name in Home Assistant: lower case, digits and dashes."},
		{name: "friendly-name", label: "Friendly name"},
		{name: "area", label: "Suggested area"},
		{name: "mac", label: "MAC (unique id)", help: "Home Assistant keys the device on it. Empty: detected."},
	}},
	{"Network", []setting{
		{name: "listen", label: "API listen address", help: "Host and port, such as :6053."},
		{name: "mdns", label: "Advertise over mDNS", kind: ui.Bool},
		{name: "mdns-iface", label: "mDNS interfaces", help: "Comma-separated. Empty: all with IPv4."},
		{name: "encryption-key", label: "API encryption key", kind: ui.Secret,
			help: "Base64 of 32 bytes, as in ESPHome's api: encryption: key:. Empty: no encryption."},
	}},
	{"Bluetooth", []setting{
		{name: "connections", label: "Connection slots", kind: ui.Number},
		{name: "scan-mode", label: "Scan mode", kind: ui.Select, options: []string{"active", "passive"}},
		{name: "adv-allow", label: "Forward only advertisements from",
			help: "Comma-separated MACs, prefixes (AA:BB:CC), uuid:fe95 or mfr:0x004c. Empty: all."},
		{name: "adv-deny", label: "Drop advertisements from", help: "As above; deny wins."},
		{name: "adv-deny-random", label: "Drop advertisements from random addresses (phones)", kind: ui.Bool},
		{name: "scan-pause-on-connect", label: "Pause scanning while connecting", kind: ui.Bool,
			backend: "bluez,winrt"},
		{name: "connect-retries", label: "Connect retries on radio failures", kind: ui.Number,
			backend: "bluez,winrt"},
	}},
	{"BlueZ", []setting{
		{name: "adapter", label: "Adapters", backend: "bluez", help: "Comma-separated, such as hci0,hci1."},
		{name: "agent", label: "Register a pairing agent", kind: ui.Bool, backend: "bluez"},
		{name: "raw-adverts", label: "Raw advertisements from the HCI monitor", kind: ui.Select, backend: "bluez",
			options: []string{"auto", "on", "off"}},
		{name: "stall-timeout", label: "Restart scanning after silence", backend: "bluez",
			help: "Such as 3m; 0 disables."},
		{name: "bluetoothd-restart", label: "bluetoothd restart command", backend: "bluez", readOnly: true},
	}},
	{"Logging", []setting{
		{name: "debug", label: "Debug logging", kind: ui.Bool},
	}},
}

// forBackend tells whether the setting applies to backend.
func (s setting) forBackend(backend string) bool {
	return s.backend == "" || slices.Contains(strings.Split(s.backend, ","), backend)
}

func lookupSetting(name string) (setting, bool) {
	for _, sec := range settings {
		for _, s := range sec.list {
			if s.name == name {
				return s, true
			}
		}
	}
	return setting{}, false
}

// app is the running proxy as the settings page, the tray and a restart see it.
var app struct {
	started    time.Time
	cmdline    map[string]bool   // flags given on the command line
	configPath string            // "" without a settings file
	config     map[string]string // the settings file as loaded or last saved
	configErr  error
	problems   problemWatch
	log        *logx.Logger
	logPath    string // the log file, if any (Windows)

	mu         sync.Mutex
	nodes      []appNode
	responders []*mdns.Responder
	uiAddr     string // the settings page's address, once it serves
	uiToken    string

	stopOnce sync.Once
}

type appNode struct {
	name    string
	adapter ble.Adapter
	addr    string // listen address
	srv     *api.Server
}

// loadSettings reads the settings file and applies it to the flags not given on the command line. It returns
// an error if the file exists but cannot be read or parsed: the file may hold the encryption key, so the proxy
// must not carry on without it. Entries that cannot be applied only end up in app.configErr.
func loadSettings(path string) error {
	app.started = time.Now()
	app.cmdline = map[string]bool{}
	flag.Visit(func(f *flag.Flag) { app.cmdline[f.Name] = true })
	app.configPath = path
	app.config = map[string]string{}
	if path == "" {
		return nil
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	app.config = cfg
	app.configErr = applyConfig(cfg, app.cmdline)
	return nil
}

// loadConfig reads a settings file; a missing file is an empty configuration.
func loadConfig(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m, err := ui.DecodeFlat(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// saveConfig writes a settings file through a temporary file, so a crash leaves the old one. It is
// readable by its owner only, set before anything is written: it may hold the encryption key.
func saveConfig(path string, m map[string]string) error {
	if err := makeConfigDir(path); err != nil {
		return err
	}
	tmp := path + ".tmp"
	os.Remove(tmp) // left by a crash, possibly with other permissions
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = protectFile(tmp)
	if err == nil {
		_, err = f.Write(ui.EncodeFlat(m))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// applyConfig sets the flags named in cfg, except those in skip. Unknown names and values a flag rejects
// are errors; the other settings still apply. A key file or $BTPROXY_ENCRYPTION_KEY wins over the file's
// key, as the flag would.
func applyConfig(cfg map[string]string, skip map[string]bool) error {
	var errs []error
	for k, v := range cfg {
		if _, ok := lookupSetting(k); !ok || flag.Lookup(k) == nil {
			errs = append(errs, fmt.Errorf("unknown setting %q", k))
			continue
		}
		if skip[k] || k == "encryption-key" && keyElsewhere(skip) {
			continue
		}
		if err := flag.Set(k, v); err != nil {
			errs = append(errs, fmt.Errorf("setting %s: %w", k, err))
		}
	}
	return errors.Join(errs...)
}

func keyElsewhere(cmdline map[string]bool) bool {
	return cmdline["encryption-key-file"] || os.Getenv("BTPROXY_ENCRYPTION_KEY") != ""
}

func addNode(name string, adapter ble.Adapter, l net.Listener, srv *api.Server) {
	app.mu.Lock()
	app.nodes = append(app.nodes, appNode{name: name, adapter: adapter, addr: l.Addr().String(), srv: srv})
	app.mu.Unlock()
}

// envUI carries the settings page's address and token across a restart, so the open page finds it again.
const envUI = "BTPROXY_UI_STATE"

// startUI serves the settings page on addr. It needs a password, except with useToken (Windows) on a loopback
// address, where it asks for a token instead, which the tray icon's menu passes. The page can show and change
// the settings, so on loopback too it must keep out other local users.
func startUI(log *logx.Logger, rs []*mdns.Responder, addr, password string, useToken bool) error {
	app.mu.Lock()
	app.responders = rs
	app.mu.Unlock()
	token := ""
	if s := os.Getenv(envUI); s != "" {
		os.Unsetenv(envUI)
		if a, t, ok := strings.Cut(s, " "); ok && addr != "" {
			addr, token = a, t
		}
	}
	if addr == "" {
		return nil
	}
	if password == "" && !(useToken && loopbackAddr(addr)) {
		return fmt.Errorf("-ui %s needs a password: set -ui-password-file (or $BTPROXY_UI_PASSWORD, -ui-password)", addr)
	}
	if !useToken || password != "" {
		token = ""
	} else if token == "" {
		token = ui.RandomToken()
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	app.mu.Lock()
	app.uiAddr, app.uiToken = l.Addr().String(), token
	app.mu.Unlock()
	auth := "none"
	if password != "" {
		auth = "password"
	} else if token != "" {
		auth = "token"
	}
	log.Info("settings page", "listen", l.Addr().String(), "auth", auth)
	srv := ui.New(settingsPage{}, ui.Options{Password: password, Token: token, Log: log})
	go srv.Serve(l)
	return nil
}

// settingsURL is the page's address to open in a browser, "" while it does not serve.
func settingsURL() string {
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.uiAddr == "" {
		return ""
	}
	host, port, _ := net.SplitHostPort(app.uiAddr)
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	u := "http://" + net.JoinHostPort(host, port) + "/"
	if app.uiToken != "" {
		u += "login?t=" + app.uiToken
	}
	return u
}

func loopbackAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// uiPassword picks the settings page password from the flag, a file or the environment.
func uiPassword(flagValue, file, env string) (string, error) {
	if flagValue != "" && file != "" {
		return "", errors.New("-ui-password and -ui-password-file are both set")
	}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
		return "", fmt.Errorf("%s is empty", file)
	}
	if flagValue != "" {
		return flagValue, nil
	}
	return env, nil
}

// stop withdraws the mDNS advertisements and exits, as SIGTERM does; with restart it starts the proxy
// again with the same command line, which then reads the saved settings.
func stop(reason string, restart bool) {
	app.stopOnce.Do(func() {
		log := app.log
		log.Info("stopping", "reason", reason)
		beforeExit()
		app.mu.Lock()
		rs := app.responders
		env := os.Environ()
		if app.uiAddr != "" {
			env = append(env, envUI+"="+app.uiAddr+" "+app.uiToken)
		}
		app.mu.Unlock()
		for _, r := range rs {
			r.Close()
		}
		if restart {
			err := restartSelf(env) // returns only on failure where the process image is replaced
			if err != nil {
				log.Error("restart", "err", err)
				platformFatal("restart failed", []any{"err", err})
				os.Exit(1) // a supervisor (procd, systemd) starts it again
			}
		}
		os.Exit(0)
	})
}

// status sums the servers' state.
func status() (st api.Status, nodes []appNode) {
	app.mu.Lock()
	nodes = append(nodes, app.nodes...)
	app.mu.Unlock()
	for _, n := range nodes {
		s := n.srv.Status()
		st.Clients += s.Clients
		st.Connections += s.Connections
		st.AdvertRate += s.AdvertRate
		st.ScanFailed = st.ScanFailed || s.ScanFailed
	}
	return st, nodes
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + one + "s"
}

// statusText is the one-line status of the tray menu.
func statusText(st api.Status, nodes int) string {
	if nodes == 0 {
		return "Starting"
	}
	if st.ScanFailed {
		return "Bluetooth error: scanning stopped"
	}
	return fmt.Sprintf("%s, %s, %.0f adverts/s", plural(st.Clients, "client"), plural(st.Connections, "connection"),
		st.AdvertRate)
}

// settingsPage is the ui.App of the settings page.
type settingsPage struct{}

func (settingsPage) Status() []ui.KV {
	st, nodes := status()
	scanner := "running"
	if st.ScanFailed {
		scanner = "failed (Bluetooth off or unavailable?)"
	}
	kv := []ui.KV{
		{Key: "Version", Value: version},
		{Key: "Process", Value: strconv.Itoa(os.Getpid())},
		{Key: "Uptime", Value: time.Since(app.started).Round(time.Second).String()},
		{Key: "Home Assistant", Value: plural(st.Clients, "client") + " connected"},
		{Key: "Connections", Value: strconv.Itoa(st.Connections)},
		{Key: "Advertisements", Value: strconv.FormatFloat(float64(st.AdvertRate), 'f', 1, 32) + "/s"},
		{Key: "Scanner", Value: scanner},
	}
	if len(nodes) > 0 {
		var a []string
		for _, n := range nodes {
			a = append(a, n.name+" on "+n.addr)
		}
		kv = append(kv, ui.KV{Key: "Serving", Value: strings.Join(a, ", ")})
	}
	last := "none"
	if line, at := app.problems.lastProblem(); line != "" {
		last = line + " (" + time.Since(at).Round(time.Second).String() + " ago)"
	}
	kv = append(kv, ui.KV{Key: "Last problem", Value: last})
	if app.logPath != "" {
		kv = append(kv, ui.KV{Key: "Log file", Value: app.logPath})
	}
	return kv
}

func (p settingsPage) Page() ui.Page {
	backend := flagValue("backend")
	page := ui.Page{Title: "btproxy " + flagValue("name"), Status: p.Status()}
	if app.configErr != nil {
		page.Notice = "Settings file: " + app.configErr.Error()
	}
	if app.configPath == "" {
		page.ReadOnly = "Read only: start btproxy with -config FILE to save settings from this page."
	}
	for _, sec := range settings {
		var fields []ui.Field
		for _, s := range sec.list {
			if !s.forBackend(backend) || s.name == "encryption-key" && !api.Encryption {
				continue
			}
			fields = append(fields, s.field())
		}
		if len(fields) > 0 {
			page.Sections = append(page.Sections, ui.Section{Title: sec.title, Fields: fields})
		}
	}
	if f, ok := autostartField(); ok {
		page.Sections = append(page.Sections, ui.Section{Title: "Windows", Fields: []ui.Field{f}})
	}
	return page
}

func flagValue(name string) string {
	if f := flag.Lookup(name); f != nil {
		return f.Value.String()
	}
	return ""
}

func (s setting) field() ui.Field {
	f := ui.Field{Name: s.name, Label: s.label, Help: s.help, Kind: s.kind, Options: s.options,
		Value: flagValue(s.name), Placeholder: flag.Lookup(s.name).DefValue}
	switch {
	case app.cmdline[s.name]:
		f.Locked = "Set on the command line (-" + s.name + "); remove the flag to change it here."
	case s.name == "encryption-key" && keyElsewhere(app.cmdline):
		f.Locked = "Set by -encryption-key-file or $BTPROXY_ENCRYPTION_KEY."
	case s.readOnly:
		f.Locked = "Runs a shell command, so it is changed in the settings file or on the command line only."
	}
	if s.name == "mac" {
		app.mu.Lock()
		if len(app.nodes) > 0 {
			f.Placeholder = "detected: " + hostMAC(app.nodes[0].adapter)
		}
		app.mu.Unlock()
	}
	return f
}

func (settingsPage) Save(values map[string]string) (map[string]string, bool, error) {
	if app.configPath == "" {
		return nil, false, errors.New("no settings file (-config)")
	}
	eff := map[string]string{}
	for _, sec := range settings {
		for _, s := range sec.list {
			eff[s.name] = flagValue(s.name)
		}
	}
	maps.Copy(eff, values)
	errs := validateSettings(eff)
	for k := range errs {
		if _, edited := values[k]; !edited {
			delete(errs, k) // a problem in a locked field is not the form's to fix
		}
	}
	if v, ok := values["autostart"]; ok && len(errs) == 0 {
		if err := setAutostartFromUI(v == "true"); err != nil {
			errs["autostart"] = err.Error()
		}
	}
	if len(errs) > 0 {
		return errs, false, nil
	}
	cfg := make(map[string]string, len(app.config))
	maps.Copy(cfg, app.config)
	for k, v := range values {
		s, ok := lookupSetting(k)
		if !ok || s.readOnly {
			continue
		}
		if v == flag.Lookup(k).DefValue {
			delete(cfg, k)
		} else {
			cfg[k] = v
		}
	}
	if maps.Equal(cfg, app.config) {
		return nil, false, nil
	}
	if err := saveConfig(app.configPath, cfg); err != nil {
		return nil, false, err
	}
	app.config = cfg
	return nil, true, nil
}

func (settingsPage) Restart() { stop("settings saved", true) }

// validateSettings checks setting values with the parsers main uses, and returns errors by setting name.
func validateSettings(v map[string]string) map[string]string {
	errs := map[string]string{}
	set := func(name string, err error) {
		if err != nil && errs[name] == "" {
			errs[name] = err.Error()
		}
	}
	for _, sec := range settings {
		for _, s := range sec.list {
			val := v[s.name]
			switch s.kind {
			case ui.Bool:
				_, err := strconv.ParseBool(val)
				set(s.name, err)
			case ui.Number:
				n, err := strconv.Atoi(val)
				if err == nil && (n < 0 || s.name == "connections" && n < 1) {
					err = errors.New("out of range")
				}
				set(s.name, err)
			case ui.Select:
				if !slices.Contains(s.options, val) {
					set(s.name, fmt.Errorf("one of %s", strings.Join(s.options, ", ")))
				}
			}
		}
	}
	if v["name"] == "" {
		set("name", errors.New("required"))
	}
	for _, name := range []string{"name", "friendly-name", "area"} {
		if strings.ContainsFunc(v[name], unicode.IsControl) {
			set(name, errors.New("control characters are not allowed"))
		}
	}
	if m := v["mac"]; m != "" {
		if hw, err := net.ParseMAC(m); err != nil || len(hw) != 6 {
			set("mac", errors.New("not a MAC such as AA:BB:CC:DD:EE:FF"))
		}
	}
	for _, a := range strings.Split(v["listen"], ",") {
		_, port, err := net.SplitHostPort(strings.TrimSpace(a))
		if err == nil {
			if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
				err = errors.New("port must be 1 to 65535")
			}
		}
		set("listen", err)
	}
	if _, err := planNodes(v["adapter"], v["listen"], v["name"], v["friendly-name"]); err != nil {
		if strings.Contains(err.Error(), "adapter") {
			set("adapter", err)
		} else {
			set("listen", err)
		}
	}
	for _, n := range strings.Split(v["mdns-iface"], ",") {
		if n = strings.TrimSpace(n); n != "" {
			if _, err := net.InterfaceByName(n); err != nil {
				set("mdns-iface", fmt.Errorf("no interface %s", n))
			}
		}
	}
	if k := v["encryption-key"]; k != "" {
		_, err := encryptionKey(k, "", "")
		set("encryption-key", err)
	}
	_, err := ble.ParseAdvFilter(v["adv-allow"], "", false)
	set("adv-allow", err)
	_, err = ble.ParseAdvFilter("", v["adv-deny"], false)
	set("adv-deny", err)
	if d := v["stall-timeout"]; d != "" {
		_, err := time.ParseDuration(d)
		set("stall-timeout", err)
	}
	return errs
}
