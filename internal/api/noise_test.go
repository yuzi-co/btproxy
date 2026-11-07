//go:build !nonoise

package api

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/yuzi-co/btproxy/internal/logx"
)

// Test keys only: the PSK is bytes 0..31, the ephemeral private keys 0x40.. and 0x80...
var (
	testPSK        = seq(0x00)
	testInitiatorE = seq(0x40)
	testResponderE = seq(0x80)
)

func seq(start byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestNoiseRespondVector checks the responder against a transcript made with the Python noiseprotocol
// package (the one aioesphomeapi uses), with the keys above and ESPHome's prologue for an empty client hello.
func TestNoiseRespondVector(t *testing.T) {
	msg1 := unhex(t, "79a631eede1bf9c98f12032cdeadd0e7a079398fc786b88cc846ec89af85a51af6140b42e9427aa9d3e7d6d9506e1613")
	reply, recv, send, err := noiseRespond(testPSK, []byte("NoiseAPIInit\x00\x00"), msg1, testResponderE)
	if err != nil {
		t.Fatal(err)
	}
	if want := "493e82fc74464a59268817623d2053c5eb8e2cc4a988b4fee179ec6b010d531df8dd769f6865dda59e90b81a8de78a4c"; hex.EncodeToString(reply) != want {
		t.Fatalf("reply %x, want %s", reply, want)
	}
	for _, m := range []struct{ ct, plain string }{
		{"8f4ed6ad0cea38d87c50fb3c2c5daee414ed87db002376", "00010003616263"},
		{"b1b6a5c568a8b2f6b96f6153a6596446df62042e", "00070000"},
	} {
		got, err := recv.open(nil, nil, unhex(t, m.ct))
		if err != nil || hex.EncodeToString(got) != m.plain {
			t.Fatalf("decrypt %s: %x, %v; want %s", m.ct, got, err, m.plain)
		}
	}
	if got := hex.EncodeToString(send.seal(nil, nil, []byte("\x00\x02\x00\x02hi"))); got != "ece3d0af7c8b5f360bc66ebb993d916d79e79c499650" {
		t.Fatalf("encrypt: %s", got)
	}
}

func TestNoiseRespondWrongKey(t *testing.T) {
	msg1 := unhex(t, "79a631eede1bf9c98f12032cdeadd0e7a079398fc786b88cc846ec89af85a51af6140b42e9427aa9d3e7d6d9506e1613")
	if _, _, _, err := noiseRespond(seq(1), []byte("NoiseAPIInit\x00\x00"), msg1, nil); !errors.Is(err, errNoiseMAC) {
		t.Fatalf("err = %v, want a MAC failure", err)
	}
	if _, _, _, err := noiseRespond(testPSK, []byte("NoiseAPIInit\x00\x01x"), msg1, nil); !errors.Is(err, errNoiseMAC) {
		t.Fatalf("other prologue: err = %v, want a MAC failure", err)
	}
}

// noiseClient is the initiator side, as aioesphomeapi runs it.
type noiseClient struct {
	t          *testing.T
	c          net.Conn
	r          *bufio.Reader
	send, recv *noiseCipher
}

func startNoiseServer(t *testing.T, key []byte) net.Conn {
	t.Helper()
	return startNoiseServerWith(t, key, func(*Server) {})
}

// startNoiseServerWith lets a test adjust the server before it serves.
func startNoiseServerWith(t *testing.T, key []byte, setup func(*Server)) net.Conn {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	cfg := Config{Name: "test", MAC: "AA:BB:CC:00:11:22", EncryptionKey: key}
	s := NewServer(cfg, testAdapter{}, logx.Discard())
	setup(s)
	go s.Serve(l)
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	return c
}

// noiseHandshakeAsClient sends the client hello and handshake with psk and returns the server hello and the
// server's handshake frame.
func noiseHandshakeAsClient(t *testing.T, c net.Conn, psk []byte) (*noiseClient, []byte, []byte) {
	t.Helper()
	e, err := ecdh.X25519().NewPrivateKey(testInitiatorE)
	if err != nil {
		t.Fatal(err)
	}
	s := newNoiseSymmetric([]byte("NoiseAPIInit\x00\x00"))
	s.mixKeyAndHash(psk)
	pub := e.PublicKey().Bytes()
	s.mixHash(pub)
	s.mixKey(pub)
	msg1 := append(append([]byte{0x00}, pub...), s.encryptAndHash(nil)...)
	if _, err := c.Write(appendNoiseFrame([]byte{0x01, 0x00, 0x00}, msg1)); err != nil {
		t.Fatal(err)
	}
	nc := &noiseClient{t: t, c: c, r: bufio.NewReader(c)}
	hello, err := readNoiseFrame(nc.r, 1000)
	if err != nil {
		t.Fatal(err)
	}
	msg2, err := readNoiseFrame(nc.r, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg2) == 1+32+noiseTagSize && msg2[0] == 0x00 {
		re, err := ecdh.X25519().NewPublicKey(msg2[1:33])
		if err != nil {
			t.Fatal(err)
		}
		s.mixHash(msg2[1:33])
		s.mixKey(msg2[1:33])
		shared, err := e.ECDH(re)
		if err != nil {
			t.Fatal(err)
		}
		s.mixKey(shared)
		if _, err := s.decryptAndHash(msg2[33:]); err != nil {
			t.Fatal(err)
		}
		nc.send, nc.recv = s.split()
	}
	return nc, hello, msg2
}

func (nc *noiseClient) write(typ uint16, payload []byte) {
	m := []byte{byte(typ >> 8), byte(typ), byte(len(payload) >> 8), byte(len(payload))}
	if _, err := nc.c.Write(appendNoiseFrame(nil, nc.send.seal(nil, nil, append(m, payload...)))); err != nil {
		nc.t.Fatal(err)
	}
}

func (nc *noiseClient) read() (uint16, []byte) {
	ct, err := readNoiseFrame(nc.r, maxNoiseFrameSize)
	if err != nil {
		nc.t.Fatal(err)
	}
	m, err := nc.recv.open(nil, nil, ct)
	if err != nil {
		nc.t.Fatal(err)
	}
	return uint16(m[0])<<8 | uint16(m[1]), m[4:]
}

func TestNoiseSession(t *testing.T) {
	c := startNoiseServer(t, testPSK)
	nc, hello, _ := noiseHandshakeAsClient(t, c, testPSK)
	if want := "\x01test\x00aabbcc001122\x00"; string(hello) != want {
		t.Fatalf("server hello %q, want %q", hello, want)
	}
	if nc.send == nil {
		t.Fatal("handshake not completed")
	}
	nc.write(msgHelloRequest, nil)
	if typ, _ := nc.read(); typ != msgHelloResponse {
		t.Fatalf("got message %d, want HelloResponse", typ)
	}
	nc.write(msgDeviceInfoRequest, nil)
	typ, p := nc.read()
	if typ != msgDeviceInfoResponse {
		t.Fatalf("got message %d, want DeviceInfoResponse", typ)
	}
	fs, err := fields(p)
	if err != nil {
		t.Fatal(err)
	}
	if fieldValue(fs, 19) != 1 {
		t.Fatal("api_encryption_supported not set")
	}
	nc.write(msgPingRequest, nil)
	if typ, _ := nc.read(); typ != msgPingResponse {
		t.Fatalf("got message %d, want PingResponse", typ)
	}
}

func TestNoiseWrongKey(t *testing.T) {
	c := startNoiseServer(t, testPSK)
	_, _, msg2 := noiseHandshakeAsClient(t, c, seq(1))
	if want := "\x01Handshake MAC failure"; string(msg2) != want {
		t.Fatalf("handshake answer %q, want %q", msg2, want)
	}
}

// TestNoiseSetupTimeouts: the short pre-Hello timeout covers the handshake only. A client that has proved
// the key gets setupTimeout for its Hello, and one that stalls in the handshake is dropped sooner.
func TestNoiseSetupTimeouts(t *testing.T) {
	timeouts := func(s *Server) { s.helloTimeout, s.setupTimeout = 100*time.Millisecond, 600*time.Millisecond }
	c := startNoiseServerWith(t, testPSK, timeouts)
	nc, _, _ := noiseHandshakeAsClient(t, c, testPSK)
	time.Sleep(250 * time.Millisecond) // past helloTimeout, within setupTimeout
	nc.write(msgHelloRequest, nil)
	if typ, _ := nc.read(); typ != msgHelloResponse {
		t.Fatalf("got message %d, want HelloResponse", typ)
	}

	c = startNoiseServerWith(t, testPSK, timeouts)
	c.Write([]byte{0x01, 0x00, 0x00}) // the client hello, then nothing
	start := time.Now()
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= 300*time.Millisecond {
		t.Fatalf("stalled handshake dropped after %v, want about 100ms", d)
	}
}

// TestNoisePlaintextClient: a plaintext client gets a frame starting with the Noise indicator, which
// aioesphomeapi reports as "Connection requires encryption".
func TestNoisePlaintextClient(t *testing.T) {
	c := startNoiseServer(t, testPSK)
	c.Write(AppendFrame(nil, msgHelloRequest, nil))
	got, _ := io.ReadAll(c)
	if want := "\x01\x00\x13\x01Bad indicator byte"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestPlaintextNoiseClient: a Noise client on a plaintext server gets the plaintext indicator back, which
// aioesphomeapi reports as "The device is using plaintext protocol".
func TestPlaintextNoiseClient(t *testing.T) {
	c := startNoiseServer(t, nil)
	c.Write([]byte{0x01, 0x00, 0x00})
	got, _ := io.ReadAll(c)
	if !bytes.HasPrefix(got, []byte("\x00Bad indicator byte")) {
		t.Fatalf("got %q", got)
	}
}

// TestNoiseQueueOrder queues messages of varying size faster than the writer sends them: the writer, which
// encrypts several into one write, must keep them in order, or the client's nonces would not match.
func TestNoiseQueueOrder(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := newClient(&Server{cfg: Config{EncryptionKey: testPSK}}, a, logx.Discard())
	c.out.setCipher(newNoiseCipher(testPSK))
	const n = outQueue // no more, or the queue could overflow
	go func() {
		for i := range n {
			c.send(uint32(i%500), bytes.Repeat([]byte{byte(i)}, i%300))
		}
		c.out.close()
	}()
	nc := &noiseClient{t: t, c: b, r: bufio.NewReader(b), recv: newNoiseCipher(testPSK)}
	for i := range n {
		typ, p := nc.read()
		if int(typ) != i%500 || len(p) != i%300 || len(p) > 0 && p[0] != byte(i) {
			t.Fatalf("message %d: type %d, %d bytes", i, typ, len(p))
		}
	}
}
