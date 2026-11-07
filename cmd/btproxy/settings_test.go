package main

import (
	"bytes"
	"flag"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/api"
	"github.com/yuzi-co/btproxy/internal/ui"
)

func goodSettings() map[string]string {
	return map[string]string{
		"name": "btproxy-x", "friendly-name": "", "area": "", "mac": "", "listen": ":6053", "mdns": "true",
		"mdns-iface": "", "encryption-key": "", "connections": "3", "scan-mode": "active", "adv-allow": "",
		"adv-deny": "", "adv-deny-random": "false", "adapter": "hci0", "agent": "true", "raw-adverts": "auto",
		"scan-pause-on-connect": "true", "stall-timeout": "3m", "connect-retries": "2", "connect-retry-delay": "1s",
		"bluetoothd-restart": "", "usb-reset": "false", "bond-dir": "/etc/btproxy", "debug": "false",
	}
}

func TestValidateSettings(t *testing.T) {
	table := settingsTable()
	if errs := validateSettings(table, goodSettings()); len(errs) != 0 {
		t.Fatalf("good settings: %v", errs)
	}
	for _, c := range []struct{ key, value string }{
		{"name", ""},
		{"mac", "AA:BB:CC"},
		{"mac", "nonsense"},
		{"listen", "6053"},
		{"listen", ":70000"},
		{"connections", "0"},
		{"connections", "three"},
		{"scan-mode", "loud"},
		{"scan-mode", "active passive"},
		{"raw-adverts", "auto on"},
		{"name", "a\x00b"},
		{"friendly-name", "a\nb"},
		{"area", "a\x00b"},
		{"mdns", "maybe"},
		{"mdns-iface", "no-such-interface0"},
		{"encryption-key", "c2hvcnQ="},
		{"adv-allow", "uuid:zz"},
		{"adv-deny", "mfr:nope"},
		{"stall-timeout", "3 minutes"},
		{"stall-timeout", ""},
		{"connect-retries", "-1"},
		{"connect-retry-delay", "1 second"},
		{"connect-retry-delay", "-1s"},
		{"raw-adverts", "sometimes"},
		{"usb-reset", "yes please"},
		{"adapter", "hci0,hci0"},
	} {
		v := goodSettings()
		v[c.key] = c.value
		if errs := validateSettings(table, v); errs[c.key] == "" {
			t.Errorf("%s=%q: no error (%v)", c.key, c.value, errs)
		}
	}
	v := goodSettings()
	v["mac"], v["encryption-key"], v["listen"] = "aa-bb-cc-dd-ee-ff", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", "127.0.0.1:6053"
	if errs := validateSettings(table, v); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
}

func TestStatusText(t *testing.T) {
	for _, c := range []struct {
		st    api.Status
		nodes int
		want  string
	}{
		{api.Status{}, 0, "Starting"},
		{api.Status{Clients: 1, Connections: 2, AdvertRate: 12.4}, 1, "1 client, 2 connections, 12 adverts/s"},
		{api.Status{Clients: 1, ScanFailed: true}, 1, "Bluetooth error: scanning stopped"},
	} {
		if got := statusText(c.st, c.nodes); got != c.want {
			t.Errorf("%+v: %q, want %q", c.st, got, c.want)
		}
	}
}

func TestUIPassword(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "pw")
	os.WriteFile(f, []byte(" secret\n"), 0o600)
	if p, err := uiPassword("", f, "env"); p != "secret" || err != nil {
		t.Errorf("file: %q %v", p, err)
	}
	if p, _ := uiPassword("flag", "", "env"); p != "flag" {
		t.Errorf("flag: %q", p)
	}
	if p, _ := uiPassword("", "", "env"); p != "env" {
		t.Errorf("env: %q", p)
	}
	if _, err := uiPassword("a", f, ""); err == nil {
		t.Error("flag and file: no error")
	}
}

func TestLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:0": true, "localhost:80": true, "[::1]:1": true, ":8080": false,
		"0.0.0.0:80": false, "192.168.1.1:80": false, "nonsense": false} {
		if loopbackAddr(addr) != want {
			t.Errorf("%s: want %v", addr, want)
		}
	}
}

// newTestApp builds the proxy in this process from args, as main would, with the fake backend, no mDNS and no
// settings page or file unless args say otherwise.
func newTestApp(t *testing.T, args ...string) (*app, error) {
	t.Helper()
	fs := flag.NewFlagSet("btproxy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return newApp(fs, append([]string{"-backend", "fake", "-mdns=false", "-ui", "", "-config", ""}, args...))
}

func writeSettings(t *testing.T, content string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "btproxy.json")
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A settings file that exists but cannot be used is an error main stops on: it may hold the encryption key,
// and carrying on would serve a plaintext API.
func TestBadSettingsFile(t *testing.T) {
	cfg := writeSettings(t, `{"encryption-key": `)
	a, err := newTestApp(t, "-config", cfg)
	if err == nil || !strings.Contains(err.Error(), cfg) {
		t.Fatalf("err %v", err)
	}
	if a == nil || a.log == nil {
		t.Fatal("no app to log the error with")
	}
}

// The settings file holds any setting of the settings page, usb-reset included, as a flag would; the command
// line wins over it, and what it cannot hold is reported without stopping the proxy.
func TestSettingsFile(t *testing.T) {
	for _, c := range []struct {
		name, file string
		args       []string
		err        string
		check      func(a *app) string
	}{
		{name: "usb-reset", file: `{"usb-reset": true, "connections": 2}`, check: func(a *app) string {
			if !a.boolean("usb-reset") || !usbReset || a.integer("connections") != 2 {
				return "not applied"
			}
			return ""
		}},
		{name: "command line wins", file: `{"connections": 2}`, args: []string{"-connections", "5"}, check: func(a *app) string {
			if a.integer("connections") != 5 {
				return "the settings file won"
			}
			return ""
		}},
		{name: "not a setting", file: `{"ui": ":8080", "name": "x"}`, err: `unknown setting "ui"`, check: func(a *app) string {
			if a.str("name") != "x" {
				return "the other setting did not apply"
			}
			return ""
		}},
		{name: "bad value", file: `{"connections": "many"}`, err: "setting connections"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := newTestApp(t, append([]string{"-config", writeSettings(t, c.file)}, c.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			if got := errString(a.configErr); c.err == "" && got != "" || !strings.Contains(got, c.err) {
				t.Fatalf("configErr %q, want %q", got, c.err)
			}
			if c.check != nil {
				if msg := c.check(a); msg != "" {
					t.Fatal(msg)
				}
			}
		})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The settings page needs a password, on loopback too, unless the platform hands out a token.
func TestUINeedsPassword(t *testing.T) {
	a, err := newTestApp(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.startUI("127.0.0.1:0", "", false); err == nil || !strings.Contains(err.Error(), "needs a password") {
		t.Fatalf("err %v", err)
	}
	if err := a.startUI("", "", false); err != nil {
		t.Fatalf("no page: %v", err)
	}
}

// serveSettingsPage serves a's settings page, with password, and returns its base URL.
func serveSettingsPage(t *testing.T, a *app, password string) string {
	t.Helper()
	srv := ui.New(settingsPage{a}, ui.Options{Password: password, Log: a.log})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go srv.Serve(l)
	return "http://" + l.Addr().String()
}

// fetch makes a request to the settings page with the password pw, and returns the status and body.
func fetch(t *testing.T, base, method, path string, form url.Values) (int, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, base+path, body)
	req.SetBasicAuth("", "pw")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

// TestSettingsPage runs the settings page in this process over a settings file: the page shows the file's
// values, locks what the command line set, rejects a bad value, saves a good one and asks for a restart.
func TestSettingsPage(t *testing.T) {
	cfg := writeSettings(t, `{"friendly-name": "Before", "connections": 2}`)
	a, err := newTestApp(t, "-name", "proxy", "-mac", "02:00:00:00:00:01", "-listen", "127.0.0.1:1", "-config", cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted := make(chan string, 1)
	a.exit = func(reason string, restart bool) {
		if restart {
			restarted <- reason
		}
	}
	base := serveSettingsPage(t, a, "pw")

	code, page := fetch(t, base, "GET", "/", nil)
	if code != 200 {
		t.Fatalf("page: %d\n%s", code, page)
	}
	for _, want := range []string{`id="f-friendly-name" name="friendly-name"`, `value="Before"`, `value="2"`,
		`id="f-name" name="name" disabled`} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %s:\n%s", want, page)
		}
	}
	if strings.Contains(page, `id="f-agent"`) {
		t.Fatal("a bluez setting is shown with the fake backend")
	}
	csrf := csrfRE.FindStringSubmatch(page)[1]

	form := url.Values{"csrf": {csrf}, "friendly-name": {"After"}, "area": {""}, "listen": {"127.0.0.1:1"},
		"connections": {"0"}, "scan-mode": {"active"}}
	if code, body := fetch(t, base, "POST", "/save", form); code != 400 || !strings.Contains(body, "out of range") {
		t.Fatalf("invalid save: %d\n%s", code, body)
	}
	form.Set("connections", "3")
	form.Del("listen") // locked: ignored even if sent
	if v := autostartFormValue(); v != "" {
		form.Set("autostart", v) // leave the real autostart as it is: unchecked boxes are left out
	}
	if code, body := fetch(t, base, "POST", "/save", form); code != 200 || !strings.Contains(body, "restarting") {
		t.Fatalf("save: %d\n%s", code, body)
	}
	saved, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseConfig(saved)
	if err != nil || m["friendly-name"] != "After" || m["connections"] != "" || len(m) != 1 {
		t.Fatalf("settings file %s: %v", saved, err)
	}
	select {
	case reason := <-restarted:
		if reason != "settings saved" {
			t.Fatalf("restart reason %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no restart after saving")
	}
}

// TestSettingsPageRestart runs the proxy as a child process with the settings page, changes a setting through
// the page and checks that the proxy restarts with it: the restart itself needs the real binary.
func TestSettingsPageRestart(t *testing.T) {
	cfg := writeSettings(t, `{"friendly-name": "Before"}`)
	uiAddr := "127.0.0.1:" + freePort(t)
	p := startProxy(t, []string{"BTPROXY_UI_PASSWORD=pw"}, "-backend", "fake", "-mdns=false", "-name", "proxy",
		"-mac", "02:00:00:00:00:01", "-listen", "127.0.0.1:"+freePort(t), "-config", cfg, "-ui", uiAddr)
	pids := map[string]bool{strconv.Itoa(p.cmd.Process.Pid): true}
	t.Cleanup(func() {
		for pid := range pids { // a restart on Windows is a new process
			if n, err := strconv.Atoi(pid); err == nil {
				if pr, err := os.FindProcess(n); err == nil {
					pr.Kill()
				}
			}
		}
	})
	base := "http://" + uiAddr
	var page string
	waitFor(t, "the page", func() bool {
		var code int
		code, page = fetch(t, base, "GET", "/", nil)
		return code == 200
	})
	if !strings.Contains(page, `value="Before"`) {
		t.Fatalf("page lacks the settings file's value:\n%s", page)
	}
	form := url.Values{"csrf": {csrfRE.FindStringSubmatch(page)[1]}, "friendly-name": {"After"}, "area": {""},
		"connections": {"3"}, "scan-mode": {"active"}}
	if v := autostartFormValue(); v != "" {
		form.Set("autostart", v)
	}
	if code, body := fetch(t, base, "POST", "/save", form); code != 200 || !strings.Contains(body, "restarting") {
		t.Fatalf("save: %d\n%s", code, body)
	}
	waitFor(t, "the restarted proxy", func() bool {
		code, body := fetch(t, base, "GET", "/", nil)
		return code == 200 && strings.Contains(body, `value="After"`)
	})
	_, status := fetch(t, base, "GET", "/status.json", nil)
	st, _ := parseConfig([]byte(status))
	pids[st["Process"]] = true
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func TestProblemWatch(t *testing.T) {
	var p problemWatch
	p.Write([]byte("time=x level=INFO msg=fine\n"))
	if l, _ := p.lastProblem(); l != "" {
		t.Fatalf("%q", l)
	}
	p.Write([]byte("time=x level=WARN msg=\"scanning stopped\" err=y\n"))
	if l, _ := p.lastProblem(); l != `level=WARN msg="scanning stopped" err=y` {
		t.Fatalf("%q", l)
	}
}

// The warning about an unencrypted API reports a choice, and is logged at every start: as the latest problem it
// would hide that nothing has gone wrong.
func TestProblemWatchIgnoresTheStartWarnings(t *testing.T) {
	for _, msg := range []string{notEncryptedWarning} {
		var out bytes.Buffer
		p := problemWatch{w: &out}
		line := "time=x level=WARN msg=\"" + msg + "\" listen=[::]:6053\n"
		p.Write([]byte(line))
		if l, _ := p.lastProblem(); l != "" {
			t.Errorf("counted as a problem: %q", l)
		}
		if out.String() != line {
			t.Errorf("not logged: %q", out.String())
		}
	}
}
