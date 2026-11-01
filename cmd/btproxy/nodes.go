package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/yuzi-co/btproxy/internal/ble/fake"
)

// node is one API endpoint. Home Assistant treats each ESPHome device as one Bluetooth scanner, so every
// adapter is served as a device of its own: its own listener, name, unique id and mDNS record.
type node struct {
	adapter, name, friendly, listen string
}

// planNodes splits -adapter (a comma-separated list) into nodes. The first adapter keeps the name and listen
// address of the flags, so adding an adapter leaves the Home Assistant entry of the first alone. Each
// further adapter gets "-<adapter>" appended to the name (and " <adapter>" to the friendly name) and the
// port after the one before, unless listen is a list with an address for every adapter.
func planNodes(adapters, listen, name, friendly string) ([]node, error) {
	var names []string
	for _, a := range strings.Split(adapters, ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		for _, b := range names {
			if a == b {
				return nil, fmt.Errorf("adapter %s given twice", a)
			}
		}
		names = append(names, a)
	}
	if len(names) == 0 {
		return nil, errors.New("no adapter")
	}
	addrs, err := listenAddrs(listen, len(names))
	if err != nil {
		return nil, err
	}
	nodes := make([]node, len(names))
	for i, a := range names {
		n := node{adapter: a, name: name, friendly: friendly, listen: addrs[i]}
		if i > 0 {
			n.name += "-" + strings.ToLower(a)
			if n.friendly != "" {
				n.friendly += " " + a
			}
		}
		if n.friendly == "" {
			n.friendly = n.name
		}
		nodes[i] = n
	}
	return nodes, nil
}

// listenAddrs gives n listen addresses: the entries of a comma-separated list, which must have n, or a single
// address and the n-1 ports after it. Port 0 stays 0, a free port each.
func listenAddrs(listen string, n int) ([]string, error) {
	if strings.Contains(listen, ",") {
		list := strings.Split(listen, ",")
		if len(list) != n {
			return nil, fmt.Errorf("-listen has %d addresses for %d adapters", len(list), n)
		}
		for i := range list {
			list[i] = strings.TrimSpace(list[i])
		}
		return list, nil
	}
	if n == 1 {
		return []string{listen}, nil
	}
	host, p, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 0 || port+n-1 > 65535 {
		return nil, fmt.Errorf("-listen %s: no %d ports from %s", listen, n, p)
	}
	addrs := make([]string, n)
	for i := range addrs {
		q := port
		if port != 0 {
			q += i
		}
		addrs[i] = net.JoinHostPort(host, strconv.Itoa(q))
	}
	return addrs, nil
}

// fakeAddr gives the fake adapter hciN the address fake.AdapterAddr+N, so several have distinct ones.
func fakeAddr(name string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimPrefix(name, "hci"), 10, 8)
	return fake.AdapterAddr + n
}
