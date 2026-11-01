// Package netif tells the machine's own network interfaces from virtual ones: hypervisor switches, VPN
// tunnels and the like, which the default -mac and mDNS should not use.
package netif

import (
	"net"
	"strings"
)

// hypervisorOUIs are the vendor prefixes of hypervisors' virtual network adapters.
var hypervisorOUIs = [][3]byte{
	{0x00, 0x15, 0x5d},                                         // Hyper-V
	{0x00, 0x50, 0x56}, {0x00, 0x0c, 0x29}, {0x00, 0x05, 0x69}, // VMware
	{0x08, 0x00, 0x27}, // VirtualBox
	{0x00, 0x16, 0x3e}, // Xen
	{0x00, 0x1c, 0x42}, // Parallels
}

// HypervisorMAC tells whether a MAC belongs to a hypervisor vendor's virtual adapters.
func HypervisorMAC(m net.HardwareAddr) bool {
	if len(m) < 3 {
		return false
	}
	for _, o := range hypervisorOUIs {
		if m[0] == o[0] && m[1] == o[1] && m[2] == o[2] {
			return true
		}
	}
	return false
}

// VirtualMAC tells whether a MAC is locally administered (bridges, containers, VPNs, randomized Wi-Fi) or
// belongs to a hypervisor, so likely not the machine's own hardware.
func VirtualMAC(m net.HardwareAddr) bool {
	return len(m) > 0 && m[0]&0x02 != 0 || HypervisorMAC(m)
}

// Adapter is what the OS tells about an interface, beyond net.Interface.
type Adapter struct {
	Name, Description string
	MAC               net.HardwareAddr
	Tunnel            bool // a tunnel or PPP link, without a MAC of its own
	Gateway           bool // has a default gateway, so it is (one of) the machine's way out
}

// vpnWords mark the drivers of VPNs and tunnels, which are skipped even with a gateway: multicast does not
// cross them, and a full-tunnel VPN has one.
var vpnWords = []string{"tap-windows", "openvpn", "wireguard", "wintun", "tailscale", "zerotier", "anyconnect",
	"globalprotect", "fortinet", "forticlient", "juniper", "pangp", "nordlynx", "npcap loopback", "loopback",
	"kernel debug"}

// virtualWords mark hypervisor and container adapters. These are skipped only without a gateway: the host
// adapter of a Hyper-V external switch is "Hyper-V Virtual Ethernet Adapter" too, and it is the LAN.
var virtualWords = []string{"hyper-v", "vethernet", "virtualbox", "vmware", "vmnet", "docker", "wsl",
	"virtual ethernet", "parallels"}

// VirtualReason returns why an adapter is virtual, or "" for one that looks like the machine's own: a
// physical NIC, or any adapter that has a default gateway and is not a VPN.
func VirtualReason(a Adapter) string {
	text := strings.ToLower(a.Name + " " + a.Description)
	for _, w := range vpnWords {
		if strings.Contains(text, w) {
			return "vpn or tunnel (" + w + ")"
		}
	}
	if a.Gateway {
		return ""
	}
	for _, w := range virtualWords {
		if strings.Contains(text, w) {
			return "virtual adapter (" + w + ")"
		}
	}
	switch {
	case a.Tunnel || len(a.MAC) == 0:
		return "tunnel"
	case HypervisorMAC(a.MAC):
		return "hypervisor MAC"
	case VirtualMAC(a.MAC):
		return "locally administered MAC"
	}
	return ""
}

// virtualPrefixes are Linux interface name prefixes of container, VM and VPN plumbing.
var virtualPrefixes = []struct{ prefix, reason string }{
	{"veth", "container veth"},
	{"docker", "Docker bridge"},
	{"podman", "Podman bridge"},
	{"cni", "CNI bridge"},
	{"flannel", "Kubernetes overlay"},
	{"cali", "Kubernetes overlay"},
	{"virbr", "libvirt bridge"},
	{"vnet", "libvirt VM tap"},
	{"lxcbr", "LXC bridge"},
	{"lxdbr", "LXD bridge"},
	{"tun", "VPN tunnel"},
	{"tap", "VPN tap"},
}

// VirtualName returns why an interface name looks like container, VM or VPN plumbing, or "". Docker's own
// networks are br- and 12 hex digits, so br-lan and other bridges named by hand are kept.
func VirtualName(name string) string {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p.prefix) {
			return p.reason
		}
	}
	if h, ok := strings.CutPrefix(name, "br-"); ok && len(h) == 12 && strings.Trim(h, "0123456789abcdef") == "" {
		return "Docker network bridge"
	}
	return ""
}
