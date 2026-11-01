//go:build !windows

package netif

// Virtual returns nothing outside Windows: the default mDNS interface list stays every up interface with IPv4.
// On Linux, bridges such as OpenWrt's br-lan, VLAN devices and macvlans often have locally administered MACs
// and are the very interfaces to advertise on, and a Linux VM's own NIC has a hypervisor's OUI, so neither MAC
// test is safe there; -mdns-iface restricts the list instead.
func Virtual() map[int]string { return nil }
