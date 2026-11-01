package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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
		"scan-pause-on-connect": "true", "stall-timeout": "3m", "connect-retries": "2", "bluetoothd-restart": "",
		"debug": "false",
	}
}

func TestValidateSettings(t *testing.T) {
	if errs := validateSettings(goodSettings()); len(errs) != 0 {
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
		{"connect-retries", "-1"},
		{"raw-adverts", "sometimes"},
		{"adapter", "hci0,hci0"},
	} {
		v := goodSettings()
		v[c.key] = c.value
		if errs := validateSettings(v); errs[c.key] == "" {
			t.Errorf("%s=%q: no error (%v)", c.key, c.value, errs)
		}
	}
	v := goodSettings()
	v["mac"], v["encryption-key"], v["listen"] = "aa-bb-cc-dd-ee-ff", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", "127.0.0.1:6053"
	if errs := validateSettings(v); len(errs) != 0 {
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

// A settings file that exists but cannot be used stops the proxy: it may hold the encryption key, and carrying
// on would serve a plaintext API.
func TestBadSettingsFile(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "btproxy.json")
	if err := os.WriteFile(cfg, []byte(`{"encryption-key": `), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-backend", "fake", "-mdns=false", "-name", "proxy", "-mac", "02:00:00:00:00:01",
		"-listen", "127.0.0.1:" + freePort(t), "-config", cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], append(append([]string{}, testChildArgs...), args...)...)
	cmd.Env = append(os.Environ(), "BTPROXY_RUN_MAIN=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("still running with a bad settings file:\n%s", out)
	}
	if err == nil || !strings.Contains(string(out), "settings file") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// TestSettingsPage runs the proxy with a settings file and the settings page, changes a setting through the
// page and checks that the proxy restarts with it.
func TestSettingsPage(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "btproxy.json")
	if err := os.WriteFile(cfg, []byte(`{"friendly-name": "Before", "connections": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	uiAddr := "127.0.0.1:" + freePort(t)
	args := []string{"-backend", "fake", "-mdns=false", "-name", "proxy", "-mac", "02:00:00:00:00:01",
		"-listen", "127.0.0.1:" + freePort(t), "-config", cfg, "-ui", uiAddr}
	cmd := exec.Command(os.Args[0], append(append([]string{}, testChildArgs...), args...)...) // args win
	cmd.Env = append(os.Environ(), "BTPROXY_RUN_MAIN=1", "BTPROXY_UI_PASSWORD=pw")
	if testing.Verbose() {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pids := map[string]bool{strconv.Itoa(cmd.Process.Pid): true}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		for pid := range pids { // a restart on Windows is a new process
			if p, err := strconv.Atoi(pid); err == nil {
				if pr, err := os.FindProcess(p); err == nil {
					pr.Kill()
				}
			}
		}
	})

	base := "http://" + uiAddr
	fetch := func(method, path string, form url.Values) (int, string) {
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
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for", what)
			}
		}
	}

	var page string
	waitFor("the page", func() bool {
		var code int
		code, page = fetch("GET", "/", nil)
		return code == 200
	})
	for _, want := range []string{`id="f-friendly-name" name="friendly-name"`, `value="Before"`, `value="2"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %s:\n%s", want, page)
		}
	}
	if !regexp.MustCompile(`id="f-name" name="name" disabled`).MatchString(page) {
		t.Fatal("-name on the command line does not lock the field")
	}
	csrf := regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`).FindStringSubmatch(page)[1]

	form := url.Values{"csrf": {csrf}, "friendly-name": {"After"}, "area": {""}, "listen": {"127.0.0.1:1"},
		"connections": {"0"}, "scan-mode": {"active"}}
	if code, body := fetch("POST", "/save", form); code != 400 || !strings.Contains(body, "out of range") {
		t.Fatalf("invalid save: %d\n%s", code, body)
	}
	form.Set("connections", "3")
	form.Del("listen") // locked: ignored even if sent
	if v := autostartFormValue(); v != "" {
		form.Set("autostart", v) // leave the real autostart as it is: unchecked boxes are left out
	}
	if code, body := fetch("POST", "/save", form); code != 200 || !strings.Contains(body, "restarting") {
		t.Fatalf("save: %d\n%s", code, body)
	}
	saved, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ui.DecodeFlat(saved)
	if err != nil || m["friendly-name"] != "After" || m["connections"] != "" || len(m) != 1 {
		t.Fatalf("settings file %s: %v", saved, err)
	}
	waitFor("the restarted proxy", func() bool {
		code, body := fetch("GET", "/", nil)
		return code == 200 && strings.Contains(body, `value="After"`)
	})
	_, status := fetch("GET", "/status.json", nil)
	st, _ := ui.DecodeFlat([]byte(status))
	pids[st["Process"]] = true
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
