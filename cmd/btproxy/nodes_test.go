package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestPlanNodes(t *testing.T) {
	for _, c := range []struct {
		adapters, listen, name, friendly string
		want                             []node
		err                              bool
	}{
		{adapters: "hci0", listen: ":6053", name: "btproxy-gw",
			want: []node{{"hci0", "btproxy-gw", "btproxy-gw", ":6053"}}},
		{adapters: "hci0", listen: "not-an-address", name: "p",
			want: []node{{"hci0", "p", "p", "not-an-address"}}}, // passed through as before, net.Listen judges
		{adapters: "hci0,hci1", listen: ":6053", name: "btproxy-gw",
			want: []node{{"hci0", "btproxy-gw", "btproxy-gw", ":6053"},
				{"hci1", "btproxy-gw-hci1", "btproxy-gw-hci1", ":6054"}}},
		{adapters: "hci0, hci1 ,", listen: "10.0.10.2:7000", name: "p", friendly: "Proxy",
			want: []node{{"hci0", "p", "Proxy", "10.0.10.2:7000"}, {"hci1", "p-hci1", "Proxy hci1", "10.0.10.2:7001"}}},
		{adapters: "hci1,hci0", listen: ":6053,:7053", name: "p",
			want: []node{{"hci1", "p", "p", ":6053"}, {"hci0", "p-hci0", "p-hci0", ":7053"}}},
		{adapters: "hci0,hci1", listen: "127.0.0.1:0", name: "p",
			want: []node{{"hci0", "p", "p", "127.0.0.1:0"}, {"hci1", "p-hci1", "p-hci1", "127.0.0.1:0"}}},
		{adapters: "hci0,hci1", listen: ":6053,:6054,:6055", name: "p", err: true},
		{adapters: "hci0,hci1", listen: ":65535", name: "p", err: true},
		{adapters: "hci0,hci0", listen: ":6053", name: "p", err: true},
		{adapters: " , ", listen: ":6053", name: "p", err: true},
	} {
		got, err := planNodes(c.adapters, c.listen, c.name, c.friendly)
		if (err != nil) != c.err || !c.err && !reflect.DeepEqual(got, c.want) {
			t.Errorf("planNodes(%q, %q): got %v, %v; want %v", c.adapters, c.listen, got, err, c.want)
		}
	}
}

// TestMain runs the proxy itself when TestTwoAdapters starts the test binary as a child.
func TestMain(m *testing.M) {
	if os.Getenv("BTPROXY_RUN_MAIN") == "1" {
		os.Args = append([]string{"btproxy"}, os.Args[1:]...)
		main()
		return
	}
	os.Exit(m.Run())
}

// TestTwoAdapters runs the proxy with two fake adapters and checks that each is served as a device of its
// own: the first exactly as with one adapter, the second on the next port with its own name and MAC.
func TestTwoAdapters(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if port >= 65535 {
		t.Skip("no port after", port)
	}
	cmd := exec.Command(os.Args[0], "-backend", "fake", "-adapter", "hci0,hci1", "-mdns=false",
		"-name", "proxy", "-mac", "02:00:00:00:00:01", "-listen", "127.0.0.1:"+strconv.Itoa(port))
	cmd.Env = append(os.Environ(), "BTPROXY_RUN_MAIN=1")
	if testing.Verbose() {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	for i, want := range []deviceInfo{
		{name: "proxy", mac: "02:00:00:00:00:01", btMAC: "00:11:22:33:44:55"},
		{name: "proxy-hci1", mac: "00:11:22:33:44:56", btMAC: "00:11:22:33:44:56"},
	} {
		got, err := queryDeviceInfo("127.0.0.1:" + strconv.Itoa(port+i))
		if err != nil {
			t.Fatalf("adapter %d: %v", i, err)
		}
		if got != want {
			t.Errorf("adapter %d: got %+v, want %+v", i, got, want)
		}
	}
}

type deviceInfo struct{ name, mac, btMAC string }

// queryDeviceInfo sends HelloRequest and DeviceInfoRequest over the plaintext API, retrying the connection
// until the proxy listens.
func queryDeviceInfo(addr string) (deviceInfo, error) {
	var c net.Conn
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if c, err = net.Dial("tcp", addr); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		return deviceInfo{}, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	// Frames: 0x00, payload length, message type, payload. Both requests here have empty payloads.
	if _, err := c.Write([]byte{0, 0, 1, 0, 0, 9}); err != nil {
		return deviceInfo{}, err
	}
	r := bufio.NewReader(c)
	for {
		if b, err := r.ReadByte(); err != nil || b != 0 {
			return deviceInfo{}, fmt.Errorf("frame start: %x, %v", b, err)
		}
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return deviceInfo{}, err
		}
		typ, err := binary.ReadUvarint(r)
		if err != nil {
			return deviceInfo{}, err
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return deviceInfo{}, err
		}
		if typ != 10 { // DeviceInfoResponse
			continue
		}
		var d deviceInfo
		for p := payload; len(p) > 0; {
			key, k := binary.Uvarint(p)
			p = p[k:]
			switch key & 7 {
			case 0:
				_, k = binary.Uvarint(p)
				p = p[k:]
			case 2:
				l, k := binary.Uvarint(p)
				s := string(p[k : k+int(l)])
				p = p[k+int(l):]
				switch key >> 3 {
				case 2:
					d.name = s
				case 3:
					d.mac = s
				case 18:
					d.btMAC = s
				}
			default:
				return deviceInfo{}, fmt.Errorf("wire type %d", key&7)
			}
		}
		return d, nil
	}
}
