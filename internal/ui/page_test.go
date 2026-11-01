package ui

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeApp struct {
	mu        sync.Mutex
	saved     map[string]string
	errs      map[string]string
	restarted chan struct{}
}

func (a *fakeApp) Page() Page {
	return Page{Title: "btproxy <test>", Status: []KV{{"Uptime", "1s"}}, Sections: []Section{{Title: "Device", Fields: []Field{
		{Name: "name", Label: "Name", Value: "node-1"},
		{Name: "mac", Label: "MAC", Value: "", Locked: "Set on the command line"},
		{Name: "mdns", Label: "mDNS", Kind: Bool, Value: "true"},
		{Name: "debug", Label: "Debug", Kind: Bool, Value: "false"},
		{Name: "key", Label: "Key", Kind: Secret, Value: `a"b<c>`},
	}}}}
}

func (a *fakeApp) Status() []KV { return []KV{{"Uptime", "1s"}} }

func (a *fakeApp) Save(v map[string]string) (map[string]string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.saved = v
	if a.errs != nil {
		return a.errs, false, nil
	}
	return nil, v["name"] != "node-1", nil
}

func (a *fakeApp) Restart() { close(a.restarted) }

func newTestServer(t *testing.T, opt Options) (*Server, *fakeApp, string) {
	t.Helper()
	app := &fakeApp{restarted: make(chan struct{})}
	s := New(app, opt)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go s.Serve(l)
	return s, app, "http://" + l.Addr().String()
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func do(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func get(t *testing.T, u string, edit func(*http.Request)) (*http.Response, string) {
	req, _ := http.NewRequest("GET", u, nil)
	if edit != nil {
		edit(req)
	}
	return do(t, req)
}

func post(t *testing.T, u string, form url.Values, edit func(*http.Request)) (*http.Response, string) {
	req, _ := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if edit != nil {
		edit(req)
	}
	return do(t, req)
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func TestBasicAuth(t *testing.T) {
	_, _, base := newTestServer(t, Options{Password: "pw"})
	if r, _ := get(t, base+"/", nil); r.StatusCode != 401 || !strings.Contains(r.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("no credentials: %d", r.StatusCode)
	}
	start := time.Now()
	if r, _ := get(t, base+"/", func(q *http.Request) { q.SetBasicAuth("admin", "nope") }); r.StatusCode != 401 {
		t.Fatalf("wrong password: %d", r.StatusCode)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Error("a wrong password is answered without delay")
	}
	r, body := get(t, base+"/", func(q *http.Request) { q.SetBasicAuth("", "pw") })
	if r.StatusCode != 200 || !strings.Contains(body, "btproxy &lt;test&gt;") {
		t.Fatalf("right password: %d %q", r.StatusCode, body)
	}
	// With a password the Host header is not checked: the page may be reached by any name.
	if r, _ := get(t, base+"/status.json", func(q *http.Request) { q.SetBasicAuth("", "pw"); q.Host = "router.lan:8080" }); r.StatusCode != 200 {
		t.Fatalf("status by another host name: %d", r.StatusCode)
	}
}

func TestTokenLogin(t *testing.T) {
	_, _, base := newTestServer(t, Options{Token: "abc123"})
	if r, _ := get(t, base+"/", nil); r.StatusCode != 403 {
		t.Fatalf("no cookie: %d", r.StatusCode)
	}
	if r, _ := get(t, base+"/login?t=wrong", nil); r.StatusCode != 403 {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	r, _ := get(t, base+"/login?t=abc123", nil)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/" {
		t.Fatalf("login: %d %v", r.StatusCode, r.Header)
	}
	ck := r.Header.Get("Set-Cookie")
	if !strings.Contains(ck, "HttpOnly") || !strings.Contains(ck, "SameSite=Strict") {
		t.Fatalf("cookie %q", ck)
	}
	withCookie := func(q *http.Request) { q.Header.Set("Cookie", "other=1; "+strings.Split(ck, ";")[0]) }
	if r, _ := get(t, base+"/", withCookie); r.StatusCode != 200 {
		t.Fatalf("with cookie: %d", r.StatusCode)
	}
	if r, _ := get(t, base+"/", func(q *http.Request) { q.Header.Set("Cookie", cookieName+"=abc124") }); r.StatusCode != 403 {
		t.Fatalf("wrong cookie: %d", r.StatusCode)
	}
}

// Without a password, only loopback host names are served (DNS rebinding).
func TestHostCheck(t *testing.T) {
	_, _, base := newTestServer(t, Options{})
	for host, want := range map[string]int{"evil.example:80": 403, "localhost:1": 200, "[::1]:5": 200, "127.0.0.2": 200, "10.0.0.1:80": 403} {
		if r, _ := get(t, base+"/status.json", func(q *http.Request) { q.Host = host }); r.StatusCode != want {
			t.Errorf("host %s: %d, want %d", host, r.StatusCode, want)
		}
	}
}

func TestSave(t *testing.T) {
	_, app, base := newTestServer(t, Options{})
	_, page := get(t, base+"/", nil)
	if !strings.Contains(page, `value="a&#34;b&lt;c&gt;"`) {
		t.Errorf("secret value not escaped:\n%s", page)
	}
	m := csrfRE.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token in the page")
	}
	form := url.Values{"csrf": {m[1]}, "name": {" node-2 "}, "mac": {"AA:BB:CC:DD:EE:FF"}, "debug": {"true"}, "key": {"k"}}

	if r, _ := post(t, base+"/save", url.Values{"name": {"x"}}, nil); r.StatusCode != 403 {
		t.Errorf("no csrf: %d", r.StatusCode)
	}
	if r, _ := post(t, base+"/save", url.Values{"csrf": {"0" + m[1][1:]}, "name": {"x"}}, nil); r.StatusCode != 403 {
		t.Errorf("wrong csrf: %d", r.StatusCode)
	}
	if r, _ := post(t, base+"/save", form, func(q *http.Request) { q.Header.Set("Origin", "http://evil.example") }); r.StatusCode != 403 {
		t.Errorf("cross origin: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("POST", base+"/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "text/plain")
	if r, _ := do(t, req); r.StatusCode != 415 {
		t.Errorf("not a form: %d", r.StatusCode)
	}
	if app.saved != nil {
		t.Fatalf("saved without a valid request: %v", app.saved)
	}

	// Errors come back on the page, with the values as sent.
	app.errs = map[string]string{"name": "bad <name>"}
	r, body := post(t, base+"/save", form, nil)
	if r.StatusCode != 400 || !strings.Contains(body, "bad &lt;name&gt;") || !strings.Contains(body, `value="node-2"`) {
		t.Fatalf("errors: %d\n%s", r.StatusCode, body)
	}
	app.errs = nil
	r, body = post(t, base+"/save", form, func(q *http.Request) { q.Header.Set("Origin", base) })
	if r.StatusCode != 200 || !strings.Contains(body, "restarting") {
		t.Fatalf("save: %d %s", r.StatusCode, body)
	}
	app.mu.Lock()
	got := app.saved
	app.mu.Unlock()
	// Locked fields are left out, unchecked boxes are false, values are trimmed.
	want := map[string]string{"name": "node-2", "mdns": "false", "debug": "true", "key": "k"}
	if len(got) != len(want) {
		t.Fatalf("saved %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("saved %v, want %v", got, want)
		}
	}
	select {
	case <-app.restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("no restart")
	}

	// Nothing changed: back to the page, no restart.
	form.Set("name", "node-1")
	if r, _ := post(t, base+"/save", form, nil); r.StatusCode != 303 {
		t.Fatalf("unchanged: %d", r.StatusCode)
	}
}

func TestStatusJSON(t *testing.T) {
	_, _, base := newTestServer(t, Options{})
	r, body := get(t, base+"/status.json", nil)
	m, err := DecodeFlat([]byte(body))
	if r.StatusCode != 200 || err != nil || m["Uptime"] != "1s" {
		t.Fatalf("%d %q %v", r.StatusCode, body, err)
	}
	if r, _ := get(t, base+"/nope", nil); r.StatusCode != 404 {
		t.Fatalf("404: %d", r.StatusCode)
	}
	if r, _ := post(t, base+"/status.json", nil, nil); r.StatusCode != 405 {
		t.Fatalf("405: %d", r.StatusCode)
	}
}
