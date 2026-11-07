package main

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"os"
	"runtime"
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

// The settings table describes every flag once: flag registration, the settings file (-config, a flat JSON
// object of flag values, {"name": "btproxy-office", ...}), the settings page (-ui) and README's flag table
// (readme_test.go) all derive from it. Flags given on the command line override the settings file; the
// settings page edits it and restarts the proxy.

// setting is one flag.
type setting struct {
	name  string
	def   any    // the default; its type (string, bool, int or time.Duration) is the flag's
	usage string // the -h text; the backends the setting is for are appended
	goos  string // registered on this OS only; "" for all

	// The settings file and page hold the settings with a section; the others are for the command line only.
	section     string
	label, help string
	kind        ui.Kind // Select or Secret; Bool and Number follow def
	options     []string
	backend     string               // shown only with these comma-separated backends; "" for all
	readOnly    bool                 // shown, but changed only in the settings file or on the command line
	check       func(v string) error // validation on the page beyond what the type needs

	// README's flag table.
	doc        string // the meaning column; the usage when ""
	docDefault string // the default column, where the default differs by OS or needs words
	docWith    string // the flag whose row this one shares
}

// settingsTable lists the flags in the order of the settings page and README. It is a function because some
// defaults are only known at run time.
func settingsTable() []setting {
	host, _ := os.Hostname()
	return []setting{
		{name: "name", def: "btproxy-" + strings.ToLower(strings.Split(host, ".")[0]),
			usage: "node name shown in Home Assistant", section: "Device", label: "Name",
			help: "Node name in Home Assistant: lower case, digits and dashes.", check: checkName,
			doc: "node name", docDefault: "`btproxy-<hostname>`"},
		{name: "friendly-name", def: "", usage: "friendly name (default: the node name)",
			section: "Device", label: "Friendly name", check: checkText, doc: "friendly name", docDefault: "the name"},
		{name: "area", def: "", usage: "suggested area", section: "Device", label: "Suggested area", check: checkText},
		{name: "mac", def: "",
			usage:   "MAC reported as the device's unique id (default: first network interface; further adapters: their own address)",
			section: "Device", label: "MAC (unique id)", help: "Home Assistant keys the device on it. Empty: detected.",
			check: checkMAC, doc: "unique id Home Assistant keys the device on",
			docDefault: "first interface MAC that is neither locally administered nor a hypervisor's"},
		{name: "listen", def: ":6053",
			usage:   "API listen address; with several adapters, the first of consecutive ports, or a comma-separated list",
			section: "Network", label: "API listen address", help: "Host and port, such as :6053.", check: checkListen,
			doc: "API address; see [several adapters](#several-adapters)"},
		{name: "mdns", def: true, usage: "advertise the API over mDNS so Home Assistant discovers it",
			section: "Network", label: "Advertise over mDNS", doc: "advertise over mDNS"},
		{name: "mdns-iface", def: "",
			usage:   "comma-separated interfaces to advertise on (default: all with IPv4, except container, VM and VPN ones)",
			section: "Network", label: "mDNS interfaces", help: "Comma-separated. Empty: all with IPv4.", check: checkIfaces,
			doc: "comma-separated, see [mDNS](#mdns)", docDefault: "all up IPv4 interfaces except container, VM, VPN"},
		{name: "encryption-key", def: "",
			usage:   "API encryption key, base64 of 32 bytes as in ESPHome's api: encryption: key: (visible in ps; prefer -encryption-key-file or $BTPROXY_ENCRYPTION_KEY)",
			section: "Network", label: "API encryption key", kind: ui.Secret,
			help: "Base64 of 32 bytes, as in ESPHome's api: encryption: key:. Empty: no encryption.", check: checkKey,
			doc: "see [Encryption](#encryption); also `$BTPROXY_ENCRYPTION_KEY`"},
		{name: "encryption-key-file", def: "", usage: "file holding the API encryption key", docWith: "encryption-key"},
		{name: "backend", def: defaultBackend,
			usage: "Bluetooth backend: auto (kernel, or bluez while bluetoothd runs), kernel or bluez on Linux, winrt on Windows, or fake",
			doc:   "`auto`, `kernel`, `bluez`, `winrt` or `fake`; see [Backends](#backends)", docDefault: "`auto`, `winrt` on Windows"},
		{name: "connections", def: 3, usage: "maximum simultaneous peripheral connections",
			section: "Bluetooth", label: "Connection slots", check: checkConnections, doc: "connection slots per adapter"},
		{name: "scan-mode", def: "active",
			usage:   "scanner mode: active or passive (Linux: needs kernel 5.10+, on bluez also bluetoothd --experimental, else active)",
			section: "Bluetooth", label: "Scan mode", kind: ui.Select, options: []string{"active", "passive"},
			doc: "`active` or `passive`"},
		{name: "adv-allow", def: "",
			usage:   "forward only advertisements matching one of these comma-separated entries: MAC, OUI or prefix (AA:BB:CC, AA:BB:CC:DD:*), uuid:fe95, mfr:0x004c (default: all)",
			section: "Bluetooth", label: "Forward only advertisements from",
			help: "Comma-separated MACs, prefixes (AA:BB:CC), uuid:fe95 or mfr:0x004c. Empty: all.", check: checkAdvAllow,
			doc: "see [Advertisement filter](#advertisement-filter)"},
		{name: "adv-deny", def: "", usage: "drop advertisements matching one of these entries (as -adv-allow; deny wins)",
			section: "Bluetooth", label: "Drop advertisements from", help: "As above; deny wins.", check: checkAdvDeny,
			docWith: "adv-allow"},
		{name: "adv-deny-random", def: false,
			usage:   "drop advertisements from private (resolvable and non-resolvable) random addresses, phones included",
			section: "Bluetooth", label: "Drop advertisements from random addresses (phones)", docWith: "adv-allow"},
		{name: "scan-pause-on-connect", def: true, usage: "stop scanning while a connection is being made, like ESPHome",
			section: "Bluetooth", label: "Pause scanning while connecting", backend: "bluez,kernel,winrt",
			doc: "stop scanning while connecting; see [Scan pause and connect retries](#scan-pause-and-connect-retries)"},
		{name: "connect-retries", def: 2,
			usage:   "retry a connect that fails at once on the radio (HCI 0x3e) or loses the link right after, this many times, 0 to disable",
			section: "Bluetooth", label: "Connect retries on radio failures", backend: "bluez,kernel,winrt",
			doc: "retries of a connect that fails at once on the radio or loses the link right after; `0` disables"},
		{name: "connect-retry-delay", def: time.Second,
			usage:   "wait before the first connect retry, doubling before each further one",
			section: "Bluetooth", label: "Wait before the first connect retry", backend: "bluez,kernel,winrt",
			help: "Such as 1s; doubles before each further retry.", check: checkDelay,
			doc: "wait before the first connect retry; doubles before each further one"},
		{name: "adapter", def: "hci0", usage: "Bluetooth adapter, or a comma-separated list served as one device each",
			section: "Linux", label: "Adapters", backend: "bluez,kernel", help: "Comma-separated, such as hci0,hci1.",
			doc: "adapter, or comma-separated list"},
		{name: "agent", def: true, usage: "register a BlueZ pairing agent",
			section: "Linux", label: "Register a pairing agent", backend: "bluez"},
		{name: "raw-adverts", def: "auto", usage: "read advertisements from the HCI monitor: auto, on or off; needs CAP_NET_RAW",
			section: "Linux", label: "Raw advertisements from the HCI monitor", kind: ui.Select, backend: "bluez",
			options: []string{"auto", "on", "off"},
			doc:     "`auto`, `on`, `off`: use the HCI monitor, see [Advertisements](#advertisements)"},
		{name: "stall-timeout", def: 3 * time.Minute,
			usage:   "restart scanning after this long without advertisements, 0 to disable; on bluez in active mode only",
			section: "Linux", label: "Restart scanning after silence", backend: "bluez,kernel", help: "Such as 3m; 0 disables.",
			doc:        "restart scanning after this long without advertisements; `0` disables; on bluez in active mode only",
			docDefault: "`3m`"},
		{name: "bluetoothd-restart", def: "",
			usage:   "shell command that restarts bluetoothd when scanning keeps stalling or it stops answering",
			section: "Linux", label: "bluetoothd restart command", backend: "bluez", readOnly: true,
			doc: "command run when `bluetoothd` hangs, such as `\"/etc/init.d/bluetoothd restart\"` or `\"systemctl restart bluetooth\"`"},
		{name: "usb-reset", def: false, usage: "reset the adapter's USB device when nothing else brings scanning back",
			section: "Linux", label: "Reset the USB device when nothing else brings scanning back", backend: "bluez,kernel",
			doc: "reset the adapter's USB device when nothing else brings scanning back; see [Hangs and recovery](#hangs-and-recovery)"},
		{name: "bond-dir", def: "/etc/btproxy", usage: "directory for the keys of bonded devices",
			section: "Linux", label: "Directory for bonded devices' keys", backend: "kernel",
			help: "Bonds already in the old directory stay there, so devices may need pairing again.",
			doc:  "keys of bonded devices"},
		{name: "config", def: defaultConfig,
			usage: "settings file (flat JSON of flag values); flags on the command line override it, the settings page saves it",
			doc:   "settings file", docDefault: "Windows: `%LOCALAPPDATA%\\btproxy\\config.json`"},
		{name: "ui", def: defaultUI,
			usage:      "serve the settings page on this address, such as :8080 (needs -ui-password, except on loopback on Windows)",
			doc:        "settings page address; needs a password, see [Settings page](#settings-page)",
			docDefault: "Windows: 127.0.0.1, free port"},
		{name: "ui-password", def: "",
			usage: "settings page password, any user name (prefer -ui-password-file or $BTPROXY_UI_PASSWORD)",
			doc:   "settings page password; also `$BTPROXY_UI_PASSWORD`"},
		{name: "ui-password-file", def: "", usage: "file holding the settings page password", docWith: "ui-password"},
		{name: "debug", def: false, usage: "debug logging", section: "Logging", label: "Debug logging"},
		{name: "tray", def: defaultTray(), usage: "show a tray icon (Windows; default: when there is no console)",
			goos: "windows", doc: "Windows tray icon", docDefault: "on when there is no console"},
		{name: "log-file", def: "",
			usage: `log file (Windows; default: %LOCALAPPDATA%\btproxy\btproxy.log when there is no console to log to)`,
			goos:  "windows", doc: "also log to this file (Windows)"},
	}
}

// here tells whether the setting is a flag on this OS.
func (s setting) here() bool { return s.goos == "" || s.goos == runtime.GOOS }

// onPage tells whether the setting is in the settings file and on the settings page.
func (s setting) onPage() bool { return s.section != "" && s.here() }

// forBackend tells whether the setting applies to backend.
func (s setting) forBackend(backend string) bool {
	return s.backend == "" || slices.Contains(strings.Split(s.backend, ","), backend)
}

// pageKind is how the settings page edits the setting.
func (s setting) pageKind() ui.Kind {
	switch s.def.(type) {
	case bool:
		return ui.Bool
	case int:
		return ui.Number
	}
	return s.kind
}

// flagUsage is the -h text, naming the backends the setting is for.
func (s setting) flagUsage() string {
	if s.backend == "" {
		return s.usage
	}
	return s.usage + " (" + backendNote(s.backend) + ")"
}

// backendNote names the backends a setting is for, as "bluez and kernel backends".
func backendNote(backend string) string {
	bs := strings.Split(backend, ",")
	if len(bs) == 1 {
		return bs[0] + " backend"
	}
	return strings.Join(bs[:len(bs)-1], ", ") + " and " + bs[len(bs)-1] + " backends"
}

// checkType checks a value of the settings page the way the flag's type would, plus the choice of a Select.
func (s setting) checkType(v string) error {
	switch s.def.(type) {
	case bool:
		_, err := strconv.ParseBool(v)
		return err
	case int:
		n, err := strconv.Atoi(v)
		if err == nil && n < 0 {
			return errors.New("out of range")
		}
		return err
	case time.Duration:
		_, err := time.ParseDuration(v)
		return err
	}
	if s.kind == ui.Select && !slices.Contains(s.options, v) {
		return fmt.Errorf("one of %s", strings.Join(s.options, ", "))
	}
	return nil
}

func checkText(v string) error {
	if strings.ContainsFunc(v, unicode.IsControl) {
		return errors.New("control characters are not allowed")
	}
	return nil
}

func checkName(v string) error {
	if v == "" {
		return errors.New("required")
	}
	return checkText(v)
}

func checkMAC(v string) error {
	if v == "" {
		return nil
	}
	if hw, err := net.ParseMAC(v); err != nil || len(hw) != 6 {
		return errors.New("not a MAC such as AA:BB:CC:DD:EE:FF")
	}
	return nil
}

func checkListen(v string) error {
	for _, a := range strings.Split(v, ",") {
		_, port, err := net.SplitHostPort(strings.TrimSpace(a))
		if err != nil {
			return err
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return errors.New("port must be 1 to 65535")
		}
	}
	return nil
}

func checkIfaces(v string) error {
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			if _, err := net.InterfaceByName(n); err != nil {
				return fmt.Errorf("no interface %s", n)
			}
		}
	}
	return nil
}

func checkKey(v string) error {
	if v == "" {
		return nil
	}
	_, err := encryptionKey(v, "", "")
	return err
}

func checkAdvAllow(v string) error {
	_, err := ble.ParseAdvFilter(v, "", false)
	return err
}

func checkAdvDeny(v string) error {
	_, err := ble.ParseAdvFilter("", v, false)
	return err
}

func checkConnections(v string) error {
	if n, _ := strconv.Atoi(v); n < 1 {
		return errors.New("out of range")
	}
	return nil
}

func checkDelay(v string) error {
	if d, _ := time.ParseDuration(v); d < 0 {
		return errors.New("out of range")
	}
	return nil
}

// registerFlags adds the table's flags for this OS to fs.
func registerFlags(fs *flag.FlagSet, table []setting) {
	for _, s := range table {
		if !s.here() {
			continue
		}
		switch d := s.def.(type) {
		case string:
			fs.String(s.name, d, s.flagUsage())
		case bool:
			fs.Bool(s.name, d, s.flagUsage())
		case int:
			fs.Int(s.name, d, s.flagUsage())
		case time.Duration:
			fs.Duration(s.name, d, s.flagUsage())
		default:
			panic(fmt.Sprintf("setting %s: default of type %T", s.name, d))
		}
	}
}

// pageSetting returns the setting the settings file and page may hold under name.
func pageSetting(table []setting, name string) (setting, bool) {
	for _, s := range table {
		if s.name == name && s.onPage() {
			return s, true
		}
	}
	return setting{}, false
}

// app is the running proxy: its flags and settings as main, the settings page, the tray and a restart see them.
type app struct {
	fs         *flag.FlagSet
	table      []setting
	started    time.Time
	cmdline    map[string]bool // flags given on the command line
	configPath string          // "" without a settings file
	problems   problemWatch
	log        *logx.Logger
	logs       *api.Logs // what API clients that subscribe to logs get
	logPath    string    // the log file, if any (Windows)

	mu         sync.Mutex
	config     map[string]string // the settings file as loaded or last saved
	configErr  error
	nodes      []appNode
	responders []*mdns.Responder
	uiAddr     string // the settings page's address, once it serves
	uiToken    string

	stopOnce sync.Once
	exit     func(reason string, restart bool) // in place of exiting in stop; set by in-process tests
}

type appNode struct {
	name    string
	adapter ble.Adapter
	addr    string // listen address
	srv     *api.Server
}

// newApp registers the table's flags on fs, parses args, applies the settings file and sets up logging. The
// app comes back even with an error, so that main can log it: a settings file that exists but cannot be used
// stops the proxy, since it may hold the encryption key. With flag.ContinueOnError, a parse error comes
// back before logging is set up.
func newApp(fs *flag.FlagSet, args []string) (*app, error) {
	a := &app{fs: fs, table: settingsTable(), started: time.Now(), cmdline: map[string]bool{},
		config: map[string]string{}}
	registerFlags(fs, a.table)
	if err := fs.Parse(args); err != nil {
		return a, err
	}
	fs.Visit(func(f *flag.Flag) { a.cmdline[f.Name] = true })
	settingsErr := a.loadSettings()
	setBackendOptions(a)
	setPlatformOptions(a)

	level := logx.LevelInfo
	if a.boolean("debug") {
		level = logx.LevelDebug
	}
	// Everything logged also goes to API clients that subscribe to logs, at the level each asks for.
	a.logs = &api.Logs{}
	a.problems.w, a.logPath = logOutput()
	a.log = logx.New(&a.problems, level, a.logs)
	return a, settingsErr
}

// flagValue is the string form of a flag's value, "" for a flag not registered on this OS.
func (a *app) flagValue(name string) string {
	if f := a.fs.Lookup(name); f != nil {
		return f.Value.String()
	}
	return ""
}

// get is a flag's value; the flag package's values all implement flag.Getter.
func (a *app) get(name string) any { return a.fs.Lookup(name).Value.(flag.Getter).Get() }

func (a *app) str(name string) string             { return a.get(name).(string) }
func (a *app) boolean(name string) bool           { return a.get(name).(bool) }
func (a *app) integer(name string) int            { return a.get(name).(int) }
func (a *app) duration(name string) time.Duration { return a.get(name).(time.Duration) }

// loadSettings reads the settings file and applies it to the flags not given on the command line. It returns
// an error if the file exists but cannot be read or parsed. Entries that cannot be applied only end up in
// configErr.
func (a *app) loadSettings() error {
	a.configPath = a.str("config")
	if a.configPath == "" {
		return nil
	}
	cfg, err := loadConfig(a.configPath)
	if err != nil {
		return err
	}
	a.config = cfg
	a.configErr = a.applyConfig(cfg)
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
	m, err := parseConfig(b)
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
		_, err = f.Write(formatConfig(m))
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

// applyConfig sets the flags named in cfg, except those given on the command line. Names that are not settings
// and values a flag rejects are errors; the other settings still apply. A key file or $BTPROXY_ENCRYPTION_KEY
// wins over the file's key, as the flag would.
func (a *app) applyConfig(cfg map[string]string) error {
	var errs []error
	for _, k := range slices.Sorted(maps.Keys(cfg)) {
		if _, ok := pageSetting(a.table, k); !ok {
			errs = append(errs, fmt.Errorf("unknown setting %q", k))
			continue
		}
		if a.cmdline[k] || k == "encryption-key" && a.keyElsewhere() {
			continue
		}
		if err := a.fs.Set(k, cfg[k]); err != nil {
			errs = append(errs, fmt.Errorf("setting %s: %w", k, err))
		}
	}
	return errors.Join(errs...)
}

func (a *app) keyElsewhere() bool {
	return a.cmdline["encryption-key-file"] || os.Getenv("BTPROXY_ENCRYPTION_KEY") != ""
}

func (a *app) addNode(name string, adapter ble.Adapter, l net.Listener, srv *api.Server) {
	a.mu.Lock()
	a.nodes = append(a.nodes, appNode{name: name, adapter: adapter, addr: l.Addr().String(), srv: srv})
	a.mu.Unlock()
}

// envUI carries the settings page's address and token across a restart, so the open page finds it again.
const envUI = "BTPROXY_UI_STATE"

// startUI serves the settings page on addr. It needs a password, except with useToken (Windows) on a loopback
// address, where it asks for a token instead, which the tray icon's menu passes. The page can show and change
// the settings, so on loopback too it must keep out other local users.
func (a *app) startUI(addr, password string, useToken bool) error {
	token := ""
	if s := os.Getenv(envUI); s != "" {
		os.Unsetenv(envUI)
		if ad, t, ok := strings.Cut(s, " "); ok && addr != "" {
			addr, token = ad, t
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
	a.mu.Lock()
	a.uiAddr, a.uiToken = l.Addr().String(), token
	a.mu.Unlock()
	auth := "none"
	if password != "" {
		auth = "password"
	} else if token != "" {
		auth = "token"
	}
	a.log.Info("settings page", "listen", l.Addr().String(), "auth", auth)
	srv := ui.New(settingsPage{a}, ui.Options{Password: password, Token: token, Log: a.log})
	go srv.Serve(l)
	return nil
}

// settingsURL is the page's address to open in a browser, "" while it does not serve.
func (a *app) settingsURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.uiAddr == "" {
		return ""
	}
	host, port, _ := net.SplitHostPort(a.uiAddr)
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	u := "http://" + net.JoinHostPort(host, port) + "/"
	if a.uiToken != "" {
		u += "login?t=" + a.uiToken
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
func (a *app) stop(reason string, restart bool) {
	a.stopOnce.Do(func() {
		if a.exit != nil {
			a.exit(reason, restart)
			return
		}
		log := a.log
		log.Info("stopping", "reason", reason)
		beforeExit()
		a.mu.Lock()
		rs := a.responders
		env := os.Environ()
		if a.uiAddr != "" {
			env = append(env, envUI+"="+a.uiAddr+" "+a.uiToken)
		}
		a.mu.Unlock()
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
func (a *app) status() (st api.Status, nodes []appNode) {
	a.mu.Lock()
	nodes = append(nodes, a.nodes...)
	a.mu.Unlock()
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
type settingsPage struct{ a *app }

func (p settingsPage) Status() []ui.KV {
	a := p.a
	st, nodes := a.status()
	scanner := "running"
	if st.ScanFailed {
		scanner = "failed (Bluetooth off or unavailable?)"
	}
	kv := []ui.KV{
		{Key: "Version", Value: version},
		{Key: "Process", Value: strconv.Itoa(os.Getpid())},
		{Key: "Uptime", Value: time.Since(a.started).Round(time.Second).String()},
		{Key: "Home Assistant", Value: plural(st.Clients, "client") + " connected"},
		{Key: "Connections", Value: strconv.Itoa(st.Connections)},
		{Key: "Advertisements", Value: strconv.FormatFloat(float64(st.AdvertRate), 'f', 1, 32) + "/s"},
		{Key: "Scanner", Value: scanner},
	}
	if len(nodes) > 0 {
		var names []string
		for _, n := range nodes {
			names = append(names, n.name+" on "+n.addr)
		}
		kv = append(kv, ui.KV{Key: "Serving", Value: strings.Join(names, ", ")})
	}
	last := "none"
	if line, at := a.problems.lastProblem(); line != "" {
		last = line + " (" + time.Since(at).Round(time.Second).String() + " ago)"
	}
	kv = append(kv, ui.KV{Key: "Last problem", Value: last})
	if a.logPath != "" {
		kv = append(kv, ui.KV{Key: "Log file", Value: a.logPath})
	}
	return kv
}

func (p settingsPage) Page() ui.Page {
	a := p.a
	backend := a.str("backend")
	page := ui.Page{Title: "btproxy " + a.str("name"), Status: p.Status()}
	a.mu.Lock()
	configErr := a.configErr
	a.mu.Unlock()
	if configErr != nil {
		page.Notice = "Settings file: " + configErr.Error()
	}
	if a.configPath == "" {
		page.ReadOnly = "Read only: start btproxy with -config FILE to save settings from this page."
	}
	for _, s := range a.table {
		if !s.onPage() || !s.forBackend(backend) || s.name == "encryption-key" && !api.Encryption {
			continue
		}
		page.Sections = addField(page.Sections, s.section, a.field(s))
	}
	if f, ok := autostartField(); ok {
		page.Sections = append(page.Sections, ui.Section{Title: "Windows", Fields: []ui.Field{f}})
	}
	return page
}

// addField puts f in the section titled title, adding the section after the others if it is new.
func addField(secs []ui.Section, title string, f ui.Field) []ui.Section {
	for i := range secs {
		if secs[i].Title == title {
			secs[i].Fields = append(secs[i].Fields, f)
			return secs
		}
	}
	return append(secs, ui.Section{Title: title, Fields: []ui.Field{f}})
}

func (a *app) field(s setting) ui.Field {
	f := ui.Field{Name: s.name, Label: s.label, Help: s.help, Kind: s.pageKind(), Options: s.options,
		Value: a.flagValue(s.name), Placeholder: a.fs.Lookup(s.name).DefValue}
	switch {
	case a.cmdline[s.name]:
		f.Locked = "Set on the command line (-" + s.name + "); remove the flag to change it here."
	case s.name == "encryption-key" && a.keyElsewhere():
		f.Locked = "Set by -encryption-key-file or $BTPROXY_ENCRYPTION_KEY."
	case s.readOnly:
		f.Locked = "Runs a shell command, so it is changed in the settings file or on the command line only."
	}
	if s.name == "mac" {
		a.mu.Lock()
		if len(a.nodes) > 0 {
			f.Placeholder = "detected: " + hostMAC(a.nodes[0].adapter)
		}
		a.mu.Unlock()
	}
	return f
}

func (p settingsPage) Save(values map[string]string) (map[string]string, bool, error) {
	a := p.a
	if a.configPath == "" {
		return nil, false, errors.New("no settings file (-config)")
	}
	eff := map[string]string{}
	for _, s := range a.table {
		if s.onPage() {
			eff[s.name] = a.flagValue(s.name)
		}
	}
	maps.Copy(eff, values)
	errs := validateSettings(a.table, eff)
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
	// The page serves several connections at once: two saves must not each build on the file as it was
	// before the other, or one's keys are lost.
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg := make(map[string]string, len(a.config))
	maps.Copy(cfg, a.config)
	for k, v := range values {
		s, ok := pageSetting(a.table, k)
		if !ok || s.readOnly {
			continue
		}
		if v == a.fs.Lookup(k).DefValue {
			delete(cfg, k)
		} else {
			cfg[k] = v
		}
	}
	if maps.Equal(cfg, a.config) {
		return nil, false, nil
	}
	if err := saveConfig(a.configPath, cfg); err != nil {
		return nil, false, err
	}
	a.config = cfg
	return nil, true, nil
}

func (p settingsPage) Restart() { p.a.stop("settings saved", true) }

// validateSettings checks the page's values with the parsers main uses, and returns errors by setting name.
func validateSettings(table []setting, v map[string]string) map[string]string {
	errs := map[string]string{}
	set := func(name string, err error) {
		if err != nil && errs[name] == "" {
			errs[name] = err.Error()
		}
	}
	for _, s := range table {
		if !s.onPage() {
			continue
		}
		err := s.checkType(v[s.name])
		if err == nil && s.check != nil {
			err = s.check(v[s.name])
		}
		set(s.name, err)
	}
	if _, err := planNodes(v["adapter"], v["listen"], v["name"], v["friendly-name"]); err != nil {
		if strings.Contains(err.Error(), "adapter") {
			set("adapter", err)
		} else {
			set("listen", err)
		}
	}
	return errs
}
