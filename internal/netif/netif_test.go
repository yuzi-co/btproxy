package netif

import (
	"net"
	"testing"
)

func mac(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

func TestVirtualMAC(t *testing.T) {
	for s, want := range map[string]bool{
		"00:11:22:aa:bb:01": false, // Realtek NIC
		"f0:b4:29:00:00:01": false, // a router's br-lan
		"00:15:5d:01:02:06": true,  // Hyper-V
		"08:00:27:12:34:56": true,  // VirtualBox
		"0a:00:27:00:00:05": true,  // VirtualBox host-only (locally administered)
		"02:42:ac:11:00:02": true,  // Docker
	} {
		if got := VirtualMAC(mac(s)); got != want {
			t.Errorf("VirtualMAC(%s) = %v", s, got)
		}
	}
	if VirtualMAC(nil) || HypervisorMAC(nil) {
		t.Error("empty MAC")
	}
}

func TestVirtualReason(t *testing.T) {
	for _, c := range []struct {
		name    string
		a       Adapter
		virtual bool
	}{
		{"ethernet", Adapter{Name: "Ethernet", Description: "Realtek PCIe GbE Family Controller",
			MAC: mac("00:11:22:aa:bb:01"), Gateway: true}, false},
		{"ethernet without gateway", Adapter{Name: "Ethernet 2", Description: "Intel(R) Ethernet Connection",
			MAC: mac("00:11:22:aa:bb:02")}, false},
		{"default switch", Adapter{Name: "vEthernet (Default Switch)", Description: "Hyper-V Virtual Ethernet Adapter",
			MAC: mac("00:15:5d:01:02:06")}, true},
		{"wsl", Adapter{Name: "vEthernet (WSL (Hyper-V firewall))", Description: "Hyper-V Virtual Ethernet Adapter #2",
			MAC: mac("00:15:5d:01:02:05")}, true},
		{"external switch", Adapter{Name: "vEthernet (External)", Description: "Hyper-V Virtual Ethernet Adapter #3",
			MAC: mac("00:15:5d:01:02:03"), Gateway: true}, false},
		{"windows VM's NIC", Adapter{Name: "Ethernet", Description: "Microsoft Hyper-V Network Adapter",
			MAC: mac("00:15:5d:01:02:04"), Gateway: true}, false},
		{"random Wi-Fi MAC", Adapter{Name: "Wi-Fi", Description: "Intel(R) Wi-Fi 6E AX211 160MHz",
			MAC: mac("3a:11:22:33:44:55"), Gateway: true}, false},
		{"virtualbox host-only", Adapter{Name: "Ethernet 3", Description: "VirtualBox Host-Only Ethernet Adapter",
			MAC: mac("0a:00:27:00:00:05")}, true},
		{"vmware vmnet8", Adapter{Name: "VMware Network Adapter VMnet8", Description: "VMware Virtual Ethernet Adapter for VMnet8",
			MAC: mac("00:50:56:c0:00:08")}, true},
		{"openvpn with gateway", Adapter{Name: "OpenVPN TAP-Windows6", Description: "TAP-Windows Adapter V9",
			MAC: mac("00:ff:12:34:56:78"), Gateway: true}, true},
		{"wireguard", Adapter{Name: "wg0", Description: "WireGuard Tunnel", Tunnel: true}, true},
		{"tailscale", Adapter{Name: "Tailscale", Description: "Tailscale Tunnel"}, true},
		{"unknown tunnel", Adapter{Name: "Local Area Connection* 8", Description: "Some Tunnel", Tunnel: true}, true},
		{"bluetooth pan", Adapter{Name: "Bluetooth Network Connection", Description: "Bluetooth Device (Personal Area Network)",
			MAC: mac("00:11:22:aa:bb:03")}, false},
	} {
		r := VirtualReason(c.a)
		if (r != "") != c.virtual {
			t.Errorf("%s: reason %q, want virtual=%v", c.name, r, c.virtual)
		}
	}
}

func TestVirtualName(t *testing.T) {
	for name, virtual := range map[string]bool{
		"br-lan": false, "br-iot": false, "eth0": false, "eth0.10": false, "wlan0": false, "end0": false,
		"enp6s18": false, "br0": false, "macvlan0": false,
		"docker0": true, "veth1a2b3c4": true, "br-3f2a1b4c5d6e": true, "virbr0": true, "vnet3": true,
		"cni0": true, "flannel.1": true, "cali12ab": true, "podman0": true, "lxcbr0": true, "tun0": true, "tap0": true,
	} {
		if got := VirtualName(name) != ""; got != virtual {
			t.Errorf("VirtualName(%q) = %q, want virtual=%v", name, VirtualName(name), virtual)
		}
	}
}
