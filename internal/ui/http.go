// Package ui serves btproxy's settings page. net/http would add about 4 MB to the router build (it brings
// crypto/tls along), so the package speaks just enough HTTP/1.1 itself: one request per connection
// (Connection: close), no chunked bodies, strict size limits and one deadline for the whole exchange.
package ui

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"btproxy/internal/logx"
)

// Limits on a request, and on the server. Variables, so the tests can shorten the timeout.
var (
	maxHeaderBytes = 8 << 10
	maxBodyBytes   = 16 << 10
	maxConns       = 8
	ioTimeout      = 10 * time.Second // reading the request and writing the response, together
)

// Request is a parsed HTTP request.
type Request struct {
	Method string
	Path   string            // the request target's path, not unescaped
	Query  url.Values        // from the request target
	Header map[string]string // lower-case names; repeated headers joined with ", "
	Body   []byte
}

// Response is what a handler returns.
type Response struct {
	Status  int
	Type    string   // Content-Type
	Headers []string // further "Name: value" lines
	Body    []byte
}

// httpError is a request the parser rejects, with the status to answer.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func bad(status int, msg string) error { return &httpError{status, msg} }

// serve accepts connections and answers each with handle, at most maxConns at a time; connections beyond
// that are closed at once.
func serve(l net.Listener, handle func(*Request) *Response, log *logx.Logger) error {
	sem := make(chan struct{}, maxConns)
	for {
		c, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return err
		}
		if err != nil {
			log.Warn("ui accept", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				serveConn(c, handle)
			}()
		default:
			c.Close()
		}
	}
}

func serveConn(c net.Conn, handle func(*Request) *Response) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(ioTimeout))
	req, err := readRequest(bufio.NewReaderSize(c, 4096))
	var resp *Response
	switch e := (*httpError)(nil); {
	case errors.As(err, &e):
		resp = &Response{Status: e.status, Type: "text/plain; charset=utf-8", Body: []byte(e.msg + "\n")}
	case err != nil:
		return // timeout or the client went away
	default:
		resp = handle(req)
	}
	c.Write(resp.bytes())
}

// readRequest parses one request. Errors that deserve an answer are *httpError.
func readRequest(r *bufio.Reader) (*Request, error) {
	budget := maxHeaderBytes
	line := func() (string, error) {
		b, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) || len(b) > budget {
			return "", bad(431, "request header too large")
		}
		if err != nil {
			return "", err
		}
		budget -= len(b)
		return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
	}
	first, err := line()
	if err != nil {
		return nil, err
	}
	parts := strings.Split(first, " ")
	if len(parts) != 3 || parts[0] == "" || !strings.HasPrefix(parts[1], "/") {
		return nil, bad(400, "malformed request line")
	}
	if parts[2] != "HTTP/1.1" && parts[2] != "HTTP/1.0" {
		return nil, bad(505, "HTTP version not supported")
	}
	req := &Request{Method: parts[0], Header: map[string]string{}}
	target := parts[1]
	if i := strings.IndexByte(target, '#'); i >= 0 {
		return nil, bad(400, "fragment in request target")
	}
	rawQuery := ""
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target, rawQuery = target[:i], target[i+1:]
	}
	req.Path = target
	if req.Query, err = url.ParseQuery(rawQuery); err != nil {
		return nil, bad(400, "malformed query")
	}
	for n := 0; ; n++ {
		h, err := line()
		if err != nil {
			return nil, err
		}
		if h == "" {
			break
		}
		if n >= 100 {
			return nil, bad(431, "too many header fields")
		}
		if h[0] == ' ' || h[0] == '\t' {
			return nil, bad(400, "folded header field")
		}
		name, value, ok := strings.Cut(h, ":")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, bad(400, "malformed header field")
		}
		name, value = strings.ToLower(name), strings.TrimSpace(value)
		if prev, dup := req.Header[name]; dup {
			if name == "content-length" && prev != value {
				return nil, bad(400, "conflicting Content-Length")
			}
			if name != "content-length" {
				value = prev + ", " + value
			}
		}
		req.Header[name] = value
	}
	if _, ok := req.Header["transfer-encoding"]; ok {
		return nil, bad(501, "Transfer-Encoding not supported")
	}
	cl, hasCL := req.Header["content-length"]
	if !hasCL {
		if req.Method == "POST" {
			return nil, bad(411, "Content-Length required")
		}
		return req, nil
	}
	n, err := strconv.Atoi(cl)
	if err != nil || n < 0 || strings.Trim(cl, "0123456789") != "" {
		return nil, bad(400, "malformed Content-Length")
	}
	if n > maxBodyBytes {
		return nil, bad(413, "request body too large")
	}
	req.Body = make([]byte, n)
	if _, err := io.ReadFull(r, req.Body); err != nil {
		return nil, err
	}
	return req, nil
}

// reason is the reason phrase for the statuses the package sends. Clients ignore it, so others get none.
func reason(status int) string {
	switch status {
	case 200:
		return "OK"
	case 303:
		return "See Other"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 411:
		return "Length Required"
	case 413:
		return "Content Too Large"
	case 415:
		return "Unsupported Media Type"
	case 431:
		return "Request Header Fields Too Large"
	case 501:
		return "Not Implemented"
	case 505:
		return "HTTP Version Not Supported"
	}
	return ""
}

// securityHeaders go with every response. The page's script and style are inline; it loads nothing else
// and talks only to its own origin. no-referrer keeps the login token out of other sites' logs.
const securityHeaders = "Cache-Control: no-store\r\n" +
	"X-Content-Type-Options: nosniff\r\n" +
	"X-Frame-Options: DENY\r\n" +
	"Referrer-Policy: no-referrer\r\n" +
	"Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; " +
	"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'\r\n"

func (r *Response) bytes() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", r.Status, reason(r.Status))
	if r.Type != "" {
		fmt.Fprintf(&b, "Content-Type: %s\r\n", r.Type)
	}
	for _, h := range r.Headers {
		b.WriteString(h + "\r\n")
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\nConnection: close\r\n%s\r\n", len(r.Body), securityHeaders)
	b.Write(r.Body)
	return b.Bytes()
}
