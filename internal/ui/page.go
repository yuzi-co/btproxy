package ui

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
)

// Kind is how a setting is edited.
type Kind int

const (
	Text Kind = iota
	Number
	Bool
	Select // one of Field.Options
	Secret // never sent to the browser; a new value can be typed or generated, or the stored one removed
)

// secretClear is added to a Secret field's name for its checkbox that removes the stored value.
const secretClear = ".clear"

// Field is one setting on the page.
type Field struct {
	Name, Label, Help string
	Kind              Kind
	Options           []string // for Select
	Value             string
	Placeholder       string // shown when Value is empty, such as the default
	Locked            string // why the field cannot be edited here ("set on the command line"); "" if it can
	Error             string
}

// Section is a group of fields under a heading.
type Section struct {
	Title  string
	Fields []Field
}

// KV is one line of the status table.
type KV struct{ Key, Value string }

// Page is what the page shows.
type Page struct {
	Title    string
	Notice   string // a warning above the form, such as a settings file that could not be read
	Status   []KV
	Sections []Section
	ReadOnly string // why nothing can be saved; "" if saving works
}

// App is the program behind the page.
type App interface {
	Page() Page
	Status() []KV
	// Save validates and stores the values of the editable fields (Bool as "true" or "false"). It returns
	// errors by field name, or whether the program must restart to apply what changed.
	Save(values map[string]string) (fieldErrs map[string]string, restart bool, err error)
	// Restart applies the saved settings. It does not return.
	Restart()
}

// Options set how the page is protected.
type Options struct {
	// Password protects the page with HTTP Basic authentication (any user name).
	Password string
	// Token, used when there is no password, must be presented once as /login?t=<token>; the page then
	// sets it as a session cookie. The Windows tray opens that URL.
	Token string
	Log   *logx.Logger
}

// Server is the settings page.
type Server struct {
	app  App
	opt  Options
	csrf string // embedded in the form; a POST must carry it
}

// New returns the page for app. Without a password or token the page is open to anyone who can reach it,
// which main allows on loopback addresses only.
func New(app App, opt Options) *Server {
	if opt.Log == nil {
		opt.Log = logx.Discard()
	}
	return &Server{app: app, opt: opt, csrf: RandomToken()}
}

// Serve answers requests on l until it is closed.
func (s *Server) Serve(l net.Listener) error { return serve(l, s.handle, s.opt.Log, defaultLimits) }

// RandomToken returns 128 random bits in hex.
func RandomToken() string {
	b := make([]byte, 16)
	if err := randomBytes(b); err != nil {
		panic("ui: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// equal compares in constant time for strings of the same length.
func equal(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var d byte
	for i := 0; i < len(a); i++ {
		d |= a[i] ^ b[i]
	}
	return d == 0
}

const cookieName = "btproxy_session"

func (s *Server) handle(r *Request) *Response {
	if s.opt.Password == "" && !loopbackHost(r.Header["host"]) {
		// Without a password the page relies on being reachable from this machine only; a foreign Host
		// header means DNS rebinding or a proxy.
		return text(403, "forbidden host")
	}
	if r.Path == "/login" && s.opt.Password == "" && s.opt.Token != "" {
		if r.Method != "GET" || !equal(r.Query.Get("t"), s.opt.Token) {
			return text(403, "invalid or expired link: open the settings from the btproxy tray icon")
		}
		return &Response{Status: 303, Headers: []string{
			"Location: /",
			"Set-Cookie: " + cookieName + "=" + s.opt.Token + "; Path=/; HttpOnly; SameSite=Strict",
		}}
	}
	if resp := s.authorize(r); resp != nil {
		return resp
	}
	switch {
	case r.Path == "/" && r.Method == "GET":
		return s.render(s.app.Page(), "", nil)
	case r.Path == "/status.json" && r.Method == "GET":
		m := map[string]string{}
		for _, kv := range s.app.Status() {
			m[kv.Key] = kv.Value
		}
		body, _ := json.Marshal(m) // cannot fail for a map of strings
		return &Response{Status: 200, Type: "application/json", Body: body}
	case r.Path == "/save" && r.Method == "POST":
		return s.save(r)
	case r.Path == "/" || r.Path == "/status.json" || r.Path == "/save":
		return text(405, "method not allowed")
	}
	return text(404, "not found")
}

// authorize returns the response refusing r, or nil if r may go on.
func (s *Server) authorize(r *Request) *Response {
	switch {
	case s.opt.Password != "":
		user, pass, ok := basicAuth(r.Header["authorization"])
		if ok && equal(pass, s.opt.Password) {
			return nil
		}
		if ok {
			s.opt.Log.Warn("ui: wrong password", "user", user)
			time.Sleep(time.Second) // slows down guessing; the connection limit caps parallel guesses
		}
		return text(401, "password required", `WWW-Authenticate: Basic realm="btproxy", charset="UTF-8"`)
	case s.opt.Token != "":
		if equal(cookie(r.Header["cookie"], cookieName), s.opt.Token) {
			return nil
		}
		return text(403, "open the settings from the btproxy tray icon")
	}
	return nil
}

func basicAuth(h string) (user, pass string, ok bool) {
	const prefix = "basic "
	if len(h) < len(prefix) || strings.ToLower(h[:len(prefix)]) != prefix {
		return "", "", false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	return strings.Cut(string(b), ":")
}

func cookie(h, name string) string {
	for _, c := range strings.Split(h, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(c), "="); ok && k == name {
			return v
		}
	}
	return ""
}

// loopbackHost tells whether a Host header names this machine by a loopback address or localhost.
func loopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"))
	return ip != nil && ip.IsLoopback()
}

func (s *Server) save(r *Request) *Response {
	if ct := r.Header["content-type"]; !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		return text(415, "expected a form")
	}
	if o := r.Header["origin"]; o != "" && o != "null" && o != "http://"+r.Header["host"] {
		return text(403, "cross-origin request")
	}
	form, err := url.ParseQuery(string(r.Body))
	if err != nil {
		return text(400, "malformed form")
	}
	if !equal(form.Get("csrf"), s.csrf) {
		return text(403, "the page is out of date: reload it and try again")
	}
	page := s.app.Page()
	if page.ReadOnly != "" {
		return s.render(page, "", nil)
	}
	values := map[string]string{}
	for _, sec := range page.Sections {
		for _, f := range sec.Fields {
			if f.Locked != "" {
				continue
			}
			switch {
			case f.Kind == Secret:
				switch v := strings.TrimSpace(form.Get(f.Name)); {
				case v != "":
					values[f.Name] = v
				case form.Get(f.Name+secretClear) != "":
					values[f.Name] = ""
				default:
					values[f.Name] = f.Value // the page never holds the stored value, so empty keeps it
				}
			case f.Kind == Bool:
				values[f.Name] = "false"
				if form.Get(f.Name) != "" {
					values[f.Name] = "true"
				}
			case form.Has(f.Name):
				values[f.Name] = strings.TrimSpace(form.Get(f.Name))
			}
		}
	}
	errs, restart, err := s.app.Save(values)
	switch {
	case err != nil:
		return s.render(page, "Not saved: "+err.Error(), values)
	case len(errs) > 0:
		for i := range page.Sections {
			for j := range page.Sections[i].Fields {
				f := &page.Sections[i].Fields[j]
				f.Error = errs[f.Name]
			}
		}
		resp := s.render(page, "Not saved: correct the fields marked below.", values)
		resp.Status = 400
		return resp
	case restart:
		s.opt.Log.Info("ui: settings saved, restarting")
		go func() {
			time.Sleep(500 * time.Millisecond) // lets this response go out first
			s.app.Restart()
		}()
		return html(200, restartPage)
	}
	return &Response{Status: 303, Headers: []string{"Location: /"}}
}
